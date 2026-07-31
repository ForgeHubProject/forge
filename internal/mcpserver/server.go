// Package mcpserver serves one forge repository to an MCP client over stdio,
// so an agent can ask the semantic questions that were previously reachable
// only from a terminal or a rendered page (issue #45), and complete the one loop
// nothing else can complete: read a semantic conflict, decide it, write the
// resolution back (issue #50).
//
// Writes are served by default. A server started read-only offers exactly the
// tools annotated read-only, and that annotation is the whole filter — there is
// no second list of which tools write. The repository root is resolved once, at
// startup, and every path parameter is resolved against it; the server never
// changes directory and never touches the source list.
package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// version identifies the server to a client. It tracks the tool contract rather
// than the forge release: a client reads it to know which tool shapes it is
// talking to. 1.1 adds the write tools; every v1 shape is unchanged.
const version = "1.1.0"

// defaultMaxChanges caps a change tree when the caller names no cap. A real
// tree runs to hundreds or thousands of nodes — the handlers cap themselves in
// that range — and a response that large is useless to the agent it was meant
// to inform.
const defaultMaxChanges = 200

// toolTimeout bounds one tool call. A handler is a native executable this server
// does not control, and unlike a command — which ends, taking its children with
// it — a server keeps answering, so one call that never returns would hold a
// goroutine and a subprocess for the rest of the session. Past this the call is
// abandoned and everything it started is killed. A client's own cancellation
// arrives by the same route, usually sooner.
const toolTimeout = 2 * time.Minute

// instructionsRead is what a client shows its model about this server. It states
// the three rules the tool shapes follow and the one boundary the server will
// not cross, so an agent neither expects completeness it will not get nor
// wastes turns probing for a tool that does not exist.
const instructionsRead = `forge answers semantic questions about files whose format has a handler: what
changed inside a structured file, which handler claims a path, what a commit
changed. Plain git can only report that such a file's bytes differ.

Three rules govern every tool here.

  Everything semantic is reachable. Handler id, build, per-side state, format
  opt-in, source list — nothing is withheld on the assumption that a caller
  will not want it.

  Nothing is complete by default. A change tree can be enormous, so responses
  are capped. Read the summary a call returns, then drill into a subtree with
  "at", page past the last change of a level with "after", and narrow with
  "kinds".

  Truncation is always explicit. A capped response carries truncated, returned,
  total and a hint naming the cursors that reach what it withheld: "at" for a
  subtree that was cut, "after" for the rest of every level the cap cut short.
  Both take a path from the response being read, so a capped response is never a
  dead end, and a cursor that would return the response naming it is never named.
  Where a tool does not have those parameters, the hint names the tool that does
  and the call that reaches the same content there. A response that does not say
  it was truncated was not truncated.

Trust boundary. Handlers are native executables resolved from the source list,
and that list is managed by a human at a terminal — deliberately, because
everything downstream of it (resolution, pinned builds, installs) is mechanical
once a source is trusted. This server reads the list and never mutates it.
There is no tool here that adds or removes a source and there will not be one
in this form: an agent that can perform the consenting act can be talked into
it by the very repository content it is reviewing, which turns a human decision
into a prompt-injection target (see issue #47). Report what is configured and
leave the terminal command to the human.`

// instructionsWrite is what the write tools add to that: the loop they exist for,
// and the operations that are absent by construction rather than gated. An agent
// told what is missing stops probing for it.
const instructionsWrite = `

Writes are served. The loop they exist for is the one nothing else can run: a
merge stops on a file whose format has a handler, forge_conflicts reports the
semantic conflicts inside it, you decide each one, forge_resolve_conflict writes
the resolution, forge_add stages it, forge_commit records it.

What is absent is absent by construction, not gated: nothing here pushes, pulls,
or fetches, so nothing leaves this machine except a handler download from a
source already configured; nothing amends or rewrites history; nothing forces
anything; nothing resets the working tree — forge_reset unstages and stops
there. forge_checkout does not force, so git's own refusal protects uncommitted
work and you are shown that refusal as git wrote it.

Every write tool says in its own description what it will not do. A server
started read-only serves only the tools annotated read-only and the writes are
not listed at all, so what you can see is what you can call.`

// instructions is what this server tells a client about itself, which depends on
// the surface it is actually serving: a read-only server must not describe tools
// its client will never be offered.
func instructions(readOnly bool) string {
	if readOnly {
		return instructionsRead + "\n\nThis server was started read-only. Every tool it serves is read-only: nothing\nhere commits, merges, installs, or edits a file. The write tools exist but are\nnot served in this mode."
	}
	return instructionsRead + instructionsWrite
}

// Run resolves the repository the server will serve and then serves it on
// stdio. readOnly collapses the surface to the tools annotated read-only, and
// takes precedence over anything else: a client cannot ask for a write tool back.
//
// stdout belongs to the protocol from that point on: anything a human should
// read goes to stderr, which is why the failure to find a repository is returned
// rather than printed — and why the line naming the mode is printed there too,
// since a human starting this by hand should see which surface they got.
//
// A signal ends the session, and ending it cancels the calls in flight, which
// kills the handler subprocesses they are waiting on. The wait at the end is for
// that killing: it is asynchronous, and a process that has already returned from
// here cannot kill anything. A process killed outright still leaves a handler
// running — nothing in it gets to run at all.
func Run(ctx context.Context, readOnly bool) error {
	stopping, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	root, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	surface := "read and write tools"
	if readOnly {
		surface = "read tools only"
	}
	fmt.Fprintf(os.Stderr, "forge mcp: serving %s on stdio (%s)\n", root, surface)

	s := &server{root: root, readOnly: readOnly, stopping: stopping}
	err = s.mcpServer().Run(stopping, &mcp.StdioTransport{})
	s.calls.Wait()
	if errors.Is(err, context.Canceled) && ctx.Err() == nil {
		// The signal this function installed, not a failure: a server asked to
		// stop stopped, and reporting that as an error would print a usage block
		// over a clean shutdown.
		return nil
	}
	return err
}

// repoRoot resolves the one repository this server answers for. A server
// outside a repository has nothing to resolve paths against, so it refuses to
// start rather than answering from whatever directory it happens to be in.
func repoRoot(ctx context.Context) (string, error) {
	out, err := forgerepo.GitOutput(ctx, "", "rev-parse", "--show-toplevel")
	root := strings.TrimSpace(string(out))
	if err != nil {
		return "", fmt.Errorf("forge mcp serves one repository and must be started inside one: %w", err)
	}
	if root == "" {
		return "", errors.New("forge mcp serves one repository and must be started inside one")
	}
	return root, nil
}

// server holds the bound repository root. It carries no cached registry: a
// handler installed during a session is meant to be visible to the next call,
// as it is to the next CLI invocation.
type server struct {
	root string
	// readOnly withholds every tool that is not annotated read-only. It is set
	// once, at startup, from the flag a human typed into a client's config, and
	// nothing a client sends can change it.
	readOnly bool
	// stopping is cancelled when the server is shutting down. The SDK derives a
	// call's context from the connection and not from the context Run was given,
	// so this is how shutdown reaches the calls in flight; without it a stopping
	// server waits out every handler it is holding.
	stopping context.Context
	// calls counts the tool calls in flight, so a server on its way out can wait
	// for them: what they are doing as they end is killing their subprocesses.
	calls sync.WaitGroup
}

// New builds the MCP server for one repository root, serving the full tool set.
func New(root string) *mcp.Server {
	return (&server{root: root, stopping: context.Background()}).mcpServer()
}

// NewReadOnly builds the server for one repository root with the writes
// withheld — the surface `forge mcp --read-only` serves.
func NewReadOnly(root string) *mcp.Server {
	return (&server{root: root, readOnly: true, stopping: context.Background()}).mcpServer()
}

// mcpServer registers the tools this repository is served through.
func (s *server) mcpServer() *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "forge", Version: version, Title: "forge — semantic version control"},
		&mcp.ServerOptions{Instructions: instructions(s.readOnly)},
	)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_status",
		Annotations: readOnly("Working tree status, with handlers"),
		Description: `What the working tree holds right now, with the handler that claims each changed file.

Answers: the repository root, the branch (or detached commit) checked out, every
changed or untracked path, whether the change is staged, and — where the
repository has opted the extension in and the handler is installed — the handler
id that makes that path answerable by forge_semantic_diff.

Does not answer what changed inside any file, and does not list unchanged files.`,
	}, s.status)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_semantic_diff",
		Annotations: readOnly("Semantic diff of one file"),
		Description: `What changed inside one file, as the handler that claims it reports it. This is
the capability nothing else here has: for a file with a handler, plain git can
only say the bytes differ.

Revisions work exactly as the CLI's do. Neither base nor head compares the
working tree against HEAD; base alone compares the working tree against base;
base and head compare one revision against the other. base_empty with head
compares a revision against nothing, the way a root commit is compared — the one
pair the CLI reaches only through forge show. comparison echoes what was actually
compared.

Nothing is complete by default. The response carries a summary (counts by kind
and the roots of the change tree, each with its child count), a depth-first slice
of the tree capped at max_changes, and truncated{returned,total,hint}. Every
node's path is a stable address: pass one as "at" for its subtree, or as "after"
for the changes that follow it at its own level — which is how a level wider than
the cap is paged through instead of fetched whole. kinds narrows; max_changes
raises the cap.

A path no handler claims is not an error: handlerId comes back null, fallback is
"text", and git's own text diff is returned under the same explicit cap.

Cannot answer anything about a file's raw bytes, and writes nothing.`,
	}, s.semanticDiff)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_show",
		Annotations: readOnly("What a commit changed"),
		Description: `What one commit changed, file by file, semantically.

Answers: the commit's own metadata (sha, author, date, subject, first parent),
and one entry per changed file — the same summary shape forge_semantic_diff
returns for a file with a handler, git's line counts for a file without one.

The comparison is the commit against its first parent, or against nothing for a
root commit. Pass path to narrow to a single file and get its change tree, capped
at max_changes with the same explicit truncation; pass after with a path from a
previous response to continue a file list the cap cut short — after here is a file
in the listing, not a change path, and this tool has no "at" at all: a change tree
is paged in forge_semantic_diff, which every capped tree here names the call for.
max_changes is the whole response's cap and not each file's, so the files listed
share the roots their summaries may name. For any other pair of revisions, use
forge_semantic_diff with base and head.`,
	}, s.show)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_handler_for",
		Annotations: readOnly("Which handler claims a path"),
		Description: `Which handler claims a path and what that handler says it can do — ask this
before assuming a semantic answer is available for a file.

Answers: the handler id for the path's extension, whether this repository has
opted the extension in or deliberately ignored it, whether the handler binary is
installed, the build installed and the build this repository pins, the source it
came from, and the capabilities the handler declares in its own info call —
including whether it supports merge.

Reports honestly rather than guessing: a handler that declares no capabilities is
reported as having declared none, a handler that does not answer the info call is
reported as silent, and a path no handler claims is reported as one the semantic
tools will fall back to text for. semanticDiffAvailable is what forge_semantic_diff
would really do with the path: in a repository that opts no format in an empty
opt-in list filters nothing, so an unlisted extension is still answered, while an
extension the repository lists as ignored is not — an ignore is a decision, and it
holds whether or not anything else is opted in.`,
	}, s.handlerFor)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_formats",
		Annotations: readOnly("Formats configured for this repository"),
		Description: `Which file extensions this repository has opted into, which it has deliberately
ignored, and the handler state of each: id, whether it is installed, the
installed build, the build the repository pins, and semanticDiffAvailable — what
forge_semantic_diff would really do with a path of that extension.

Answers "what can forge be asked about here" before any path is named. An
extension listed with no installed handler is inactive — semantic tools fall
back to text for it until a human installs one.

A repository that opts no format in has not opted out of everything, and optInList
comes back false to say so: an empty opt-in list filters nothing, so every
installed handler answers here and the extensions they claim are listed as
"unlisted". An extension listed as ignored is the exception — an ignore is a
decision, so it holds whether or not anything else is opted in.

Changes nothing: adding, ignoring, or installing a format is a terminal command.`,
	}, s.formats)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_source_list",
		Annotations: readOnly("Configured handler sources (read-only)"),
		Description: `The handler sources configured on this machine, read-only.

Adding or removing a source is a deliberate human action at a terminal, by
design. The source list is forge's entire trust boundary — a handler is a native
executable, and everything downstream of the list is mechanical once a source is
trusted. This server does not expose source mutation and never will in its
current form: an agent that could add a source could be talked into it by text in
the repository it is reviewing, which collapses a human decision into a
prompt-injection target (see issue #47).

Use this to report what is configured, and ask the human to run the terminal
command when something is missing.`,
	}, s.sourceList)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_conflicts",
		Annotations: readOnly("Semantic conflicts of an unfinished merge"),
		Description: `What an unfinished merge could not reconcile, path by path — and, for a file
whose format has a handler, which semantic units inside it disagree. This is the
question git cannot answer for such a file: it can say the merge stopped, not
what stopped it.

Answers: every unmerged path, and for each the handler's conflicts as
path/ours/theirs, where ours is the side already checked out and theirs the side
being merged in. The conflict paths are addresses — pass them back to
forge_resolve_conflict to decide them.

Reads only. It recomputes the merge in memory from the three stages the index
holds and writes nothing, so calling it does not disturb a resolution in progress
and calling it twice gives the same answer.

Nothing is complete by default: max_conflicts caps the whole response, path
narrows to one file, and after continues the file listing from a path a previous
response returned. Truncation is explicit and the hint names the call that
reaches what was withheld.

A path no handler claims is listed with no semantic conflicts: git's own markers
are in the working tree, and the resolution for it is an ordinary text edit.`,
	}, s.conflicts)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_resolve_conflict",
		Annotations: writeTool("Resolve one file's semantic conflicts", false, true, false),
		Description: `Decide the semantic conflicts in one unmerged file and write the resolved result
to the working tree. Call forge_conflicts first: its conflict paths are what this
takes.

Choices only. Every conflict the handler reports must be given "ours" or
"theirs", and a call that leaves one undecided is refused by name rather than
resolved with a default nobody chose. There is no parameter for replacement
content: handing a file's whole bytes to a merge tool is a plain file write in a
merge costume, and forge does not offer it.

Not destructive, and this is a fact about the construction rather than a claim:
the merge is recomputed from the three stages the index holds, so until the file
is staged the conflicted pre-image is still recoverable from the index and this
call can be made again with different choices to overwrite its own result.

Writes the merged file, and stops there. It does not stage — that is forge_add —
and it does not commit or conclude the merge.

A file whose format has no handler has no semantic conflicts to decide, and is
refused here: resolve its markers as text and stage it.`,
	}, s.resolveConflict)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_add",
		Annotations: writeTool("Stage paths", false, true, false),
		Description: `Stage the given paths, the way forge add and git add do — including marking a
resolved file as resolved, which is how the merge loop tells git a conflict is
settled.

Paths are resolved against the repository this server serves and one that points
outside it is refused. A path is always passed to git as a path and never as a
flag, so a file whose name begins with "-" stages as itself.

Staging the same paths twice leaves the same index, and nothing here removes a
path from the index — that is forge_reset.`,
	}, s.add)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_commit",
		Annotations: writeTool("Commit staged changes", false, false, false),
		Description: `Record what is staged as a new commit, with the message given.

Commits only what is staged: it does not stage on the way, so what lands is what
forge_status reports as staged and nothing else. Concluding a merge is the same
call — stage every resolved path, then commit.

There is no amend and there will not be one: rewriting a commit that exists is
not something this server does. There is no author override either; the commit
is recorded as the identity this machine's git is configured with, and git's own
complaint is returned if there is none.

Nothing is pushed. A commit made here stays on this machine until a human pushes
it.`,
	}, s.commit)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_create_branch",
		Annotations: writeTool("Create a branch", false, true, false),
		Description: `Create a branch, optionally at a revision other than HEAD.

Creates and stops there: it does not check the new branch out — that is
forge_checkout — and it does not move a branch that already exists. git's own
refusal is returned for a name that is already taken or that is not a legal ref
name.

The branch is local. Nothing here publishes it.`,
	}, s.createBranch)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_checkout",
		Annotations: writeTool("Check out a branch or revision", false, false, false),
		Description: `Check out an existing branch or revision.

Never forced. git refuses to check out over a change that has not been committed,
and that refusal is what protects work in the working tree — it is returned here
as git wrote it, unmodified, so the reason is the real one. Commit or stage the
work, or ask the human to deal with it; there is no flag here that overrides the
refusal, because none is built.

Does not create branches — that is forge_create_branch — and does not fetch: a
revision this machine does not have cannot be checked out from here.`,
	}, s.checkout)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_reset",
		Annotations: writeTool("Unstage — index only, never the working tree", true, true, false),
		Description: `Unstage: take the given paths, or everything, back out of the index. With no
paths the whole index is reset to HEAD.

The working tree is never touched. This is a soft index reset by construction —
there is no hard reset here, no --hard, and no way to ask for one, so no edit of
yours can be destroyed by this call. What it does destroy is the staging you had
arranged, which is why it is annotated destructive: a client should ask before
running it, and the arrangement it discards is not recoverable from anywhere.

Files themselves are untouched: a path unstaged here still holds exactly the
bytes it held before. One consequence is worth knowing before you call it with
no paths during a merge: resetting the whole index also ends the merge git
thought was in progress. Every file keeps its contents, but forge_conflicts will
have nothing left to report. The response says so when it happens.`,
	}, s.reset)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_formats_add",
		Annotations: writeTool("Opt an extension in for this repository", false, true, false),
		Description: `Opt one file extension into this repository's format list, so the semantic tools
answer for paths of that extension instead of falling back to text.

Edits one committed, reviewable file — the repository's format list — and nothing
else. The change shows up in forge_status as an ordinary edit for a human to read
in the diff before it is committed, which is what makes this write self-auditing.

Recording an extension does not install anything: forge_install does that, and
forge_formats reports whether a handler is actually installed for it. An
extension recorded with no handler installed is inactive rather than broken.`,
	}, s.formatsAdd)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_formats_ignore",
		Annotations: writeTool("Mark an extension as deliberately unhandled", false, true, false),
		Description: `Mark one file extension as deliberately having no handler in this repository, so
the semantic tools leave paths of that extension to git's own text diff even when
a handler for them is installed.

This is the decision that survives an empty format list: a repository that opts
nothing in filters nothing, but an ignore holds regardless. Use it to record that
a format is meant to be treated as text, rather than leaving the absence of a
handler to say it.

Edits the same committed, reviewable file forge_formats_add does, and flips an
extension out of the opted-in list if it was there.`,
	}, s.formatsIgnore)

	addTool(s, srv, &mcp.Tool{
		Name:        "forge_install",
		Annotations: writeTool("Install a handler from a configured source", false, true, true),
		Description: `Install the handler that claims an extension, from a source already configured on
this machine, and record the build this repository pins.

Refuses if no configured source offers a handler for the extension. That refusal
is the trust boundary and not a gap to work around: the source list is what makes
a handler safe to run at all, adding to it is a human action at a terminal (issue
#47), and this server has no tool that adds one. Report the refusal and ask.

This is the one tool here that reaches the network, and it reaches only the
sources forge_source_list reports. Installing an already-installed handler
downloads nothing and simply records the pin.

Installing does not opt the extension in — that is forge_formats_add — and does
not configure this repository's merge driver, which stays a terminal command.`,
	}, s.install)

	return srv
}

// addTool registers a tool unless the mode this server runs in withholds it.
//
// The annotation is the whole filter: a read-only server serves exactly the
// tools whose readOnlyHint is true, so the surface a client is offered is
// derived from the same metadata the client is shown and cannot drift from it.
// There is deliberately no list of "the write tools" anywhere — a list would be
// a second truth to keep in step, and the one time it fell behind, a tool would
// be served under a hint that contradicts it. A tool with no annotations at all
// is withheld too: an unannotated tool is, by the spec's defaults, a destructive
// one.
func addTool[In, Out any](s *server, srv *mcp.Server, t *mcp.Tool, h mcp.ToolHandlerFor[In, Out]) {
	if !s.serves(t) {
		return
	}
	mcp.AddTool(srv, t, bounded(s, h))
}

// serves reports whether a tool is offered in this server's mode. Read-only
// takes precedence over everything: there is no configuration that puts a write
// tool back.
func (s *server) serves(t *mcp.Tool) bool {
	return !s.readOnly || (t.Annotations != nil && t.Annotations.ReadOnlyHint)
}

// bounded gives a tool handler the deadline every call gets, counts it while it
// runs so shutdown can wait for it, and reports a call that hits the deadline as
// abandoned rather than as whatever error its killed subprocess produced last.
func bounded[In, Out any](s *server, h mcp.ToolHandlerFor[In, Out]) mcp.ToolHandlerFor[In, Out] {
	return func(ctx context.Context, req *mcp.CallToolRequest, in In) (*mcp.CallToolResult, Out, error) {
		s.calls.Add(1)
		defer s.calls.Done()

		ctx, cancel := context.WithTimeout(ctx, toolTimeout)
		defer cancel()
		if s.stopping != nil {
			defer context.AfterFunc(s.stopping, cancel)()
		}

		res, out, err := h(ctx, req, in)
		if err != nil && errors.Is(ctx.Err(), context.DeadlineExceeded) {
			var zero Out
			return nil, zero, fmt.Errorf("this call did not answer within %s and was abandoned, along with everything it started: %w", toolTimeout, err)
		}
		return res, out, err
	}
}

// readOnly annotates a tool as one that cannot change anything. The hint is not
// decoration: at least one major client auto-approves on it, so annotating a
// tool read-only is a statement that it may be run without asking, and only a
// tool that writes nothing may carry it.
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, Title: title}
}

// writeTool annotates a tool that changes something. Every hint is set here,
// including the two the caller might think it could leave out: DestructiveHint
// and OpenWorldHint are pointers in the SDK precisely because their spec default
// when omitted is true, so an unset destructive hint tells a client this tool
// destroys things and an unset open-world hint tells it this tool reaches the
// network. Neither is left to a default anywhere in this package.
func writeTool(title string, destructive, idempotent, openWorld bool) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{
		Title:           title,
		ReadOnlyHint:    false,
		DestructiveHint: &destructive,
		IdempotentHint:  idempotent,
		OpenWorldHint:   &openWorld,
	}
}

// resolve turns a path parameter into a repo-relative, slash-separated path.
// Paths arrive from an agent, so resolution is against the bound root and never
// the process's working directory, and one that climbs out of the root is
// refused rather than read.
//
// The name climbing out is only half of it: a path can leave the root without
// ever looking like it, through a directory link the repository itself contains.
// That is repository content steering a read rather than an argument doing it,
// and it is exactly the crossing an agent reviewing untrusted content must not be
// talked into, so the directories on the way are followed and checked too.
func (s *server) resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("path is required")
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.root, p)
	}
	abs = filepath.Clean(abs)

	roots := s.roots()
	rel, inside := relInside(roots, abs)
	if !inside {
		return "", fmt.Errorf("%s is outside the repository this server serves", p)
	}
	// The last component is deliberately left unfollowed: a link there is content
	// git records as content, and forgerepo compares it as the path it names. The
	// root has no such component — it is the boundary rather than a step towards
	// it — so the walk starts at the root itself instead of above it, where
	// nothing is ever contained.
	dir := filepath.Dir(abs)
	if rel == "." {
		dir = abs
	}
	for {
		resolved, err := filepath.EvalSymlinks(dir)
		if err == nil {
			if _, ok := relInside(roots, resolved); !ok {
				return "", fmt.Errorf("%s leads outside the repository this server serves through a link, so it is not read", p)
			}
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			// Nothing on the way exists, so nothing was followed.
			break
		}
		dir = parent
	}
	return rel, nil
}

// roots are the forms of the bound root a contained path may be relative to: the
// root as it was given, and the root with its own links resolved — a repository
// under a linked directory is still the repository this server serves.
func (s *server) roots() []string {
	roots := []string{s.root}
	if resolved, err := filepath.EvalSymlinks(s.root); err == nil && resolved != s.root {
		roots = append(roots, resolved)
	}
	return roots
}

// relInside reports an absolute path relative to whichever root contains it,
// slash-separated, and whether any of them did.
func relInside(roots []string, abs string) (string, bool) {
	for _, root := range roots {
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return filepath.ToSlash(rel), true
	}
	return "", false
}

// noArgs is the input of a tool that takes none.
type noArgs struct{}

// comparison echoes the two sides a semantic answer was computed from, named as
// the caller would name them, so an answer can never be read against the wrong
// pair.
type comparison struct {
	Base string `json:"base" jsonschema:"the base side actually compared"`
	Head string `json:"head" jsonschema:"the head side actually compared"`
}
