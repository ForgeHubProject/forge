// Package mcpserver serves one forge repository to an MCP client over stdio,
// so an agent can ask the semantic questions that were previously reachable
// only from a terminal or a rendered page (issue #45).
//
// Every tool is read-only. The repository root is resolved once, at startup,
// and every path parameter is resolved against it; the server never changes
// directory and never writes to the repository, the handler configuration, or
// the source list.
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
// talking to.
const version = "1.0.0"

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

// instructions is what a client shows its model about this server. It states
// the three rules the tool shapes follow and the one boundary the server will
// not cross, so an agent neither expects completeness it will not get nor
// wastes turns probing for a tool that does not exist.
const instructions = `forge answers semantic questions about files whose format has a handler: what
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
  subtree that was cut, "after" for the rest of a level. Both take a path from
  the response being read, so a capped response is never a dead end. A response
  that does not say it was truncated was not truncated.

Trust boundary. Handlers are native executables resolved from the source list,
and that list is managed by a human at a terminal — deliberately, because
everything downstream of it (resolution, pinned builds, installs) is mechanical
once a source is trusted. This server reads the list and never mutates it.
There is no tool here that adds or removes a source and there will not be one
in this form: an agent that can perform the consenting act can be talked into
it by the very repository content it is reviewing, which turns a human decision
into a prompt-injection target (see issue #47). Report what is configured and
leave the terminal command to the human.

Every tool is read-only. Nothing here commits, pushes, merges, installs, or
edits a file.`

// Run resolves the repository the server will serve and then serves it on
// stdio. stdout belongs to the protocol from that point on: anything a human
// should read goes to stderr, which is why the failure to find a repository is
// returned rather than printed.
//
// A signal ends the session, and ending it cancels the calls in flight, which
// kills the handler subprocesses they are waiting on. The wait at the end is for
// that killing: it is asynchronous, and a process that has already returned from
// here cannot kill anything. A process killed outright still leaves a handler
// running — nothing in it gets to run at all.
func Run(ctx context.Context) error {
	stopping, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	root, err := repoRoot(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "forge mcp: serving %s on stdio\n", root)

	s := &server{root: root, stopping: stopping}
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
	// stopping is cancelled when the server is shutting down. The SDK derives a
	// call's context from the connection and not from the context Run was given,
	// so this is how shutdown reaches the calls in flight; without it a stopping
	// server waits out every handler it is holding.
	stopping context.Context
	// calls counts the tool calls in flight, so a server on its way out can wait
	// for them: what they are doing as they end is killing their subprocesses.
	calls sync.WaitGroup
}

// New builds the MCP server for one repository root.
func New(root string) *mcp.Server {
	return (&server{root: root, stopping: context.Background()}).mcpServer()
}

// mcpServer registers the tools this repository is served through.
func (s *server) mcpServer() *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "forge", Version: version, Title: "forge — semantic version control"},
		&mcp.ServerOptions{Instructions: instructions},
	)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "forge_status",
		Annotations: readOnly("Working tree status, with handlers"),
		Description: `What the working tree holds right now, with the handler that claims each changed file.

Answers: the repository root, the branch (or detached commit) checked out, every
changed or untracked path, whether the change is staged, and — where the
repository has opted the extension in and the handler is installed — the handler
id that makes that path answerable by forge_semantic_diff.

Does not answer what changed inside any file, and does not list unchanged files.`,
	}, bounded(s, s.status))

	mcp.AddTool(srv, &mcp.Tool{
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
	}, bounded(s, s.semanticDiff))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "forge_show",
		Annotations: readOnly("What a commit changed"),
		Description: `What one commit changed, file by file, semantically.

Answers: the commit's own metadata (sha, author, date, subject, first parent),
and one entry per changed file — the same summary shape forge_semantic_diff
returns for a file with a handler, git's line counts for a file without one.

The comparison is the commit against its first parent, or against nothing for a
root commit. Pass path to narrow to a single file and get its change tree, capped
at max_changes with the same explicit truncation; pass after with a path from a
previous response to continue a file list the cap cut short. For any other pair of
revisions, use forge_semantic_diff with base and head.`,
	}, bounded(s, s.show))

	mcp.AddTool(srv, &mcp.Tool{
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
would really do with the path: in a repository that lists no formats an empty
opt-in list filters nothing, so an unlisted extension is still answered.`,
	}, bounded(s, s.handlerFor))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "forge_formats",
		Annotations: readOnly("Formats configured for this repository"),
		Description: `Which file extensions this repository has opted into, which it has deliberately
ignored, and the handler state of each: id, whether it is installed, the
installed build, the build the repository pins, and semanticDiffAvailable — what
forge_semantic_diff would really do with a path of that extension.

Answers "what can forge be asked about here" before any path is named. An
extension listed with no installed handler is inactive — semantic tools fall
back to text for it until a human installs one.

A repository that lists no formats has not opted out of everything, and optInList
comes back false to say so: an empty opt-in list filters nothing, so every
installed handler answers here and the extensions they claim are listed as
"unlisted".

Changes nothing: adding, ignoring, or installing a format is a terminal command.`,
	}, bounded(s, s.formats))

	mcp.AddTool(srv, &mcp.Tool{
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
	}, bounded(s, s.sourceList))

	return srv
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

// readOnly annotates a tool as one that cannot change anything. v1 has no write
// tools at all, and the reference git server annotates anyway, so a client that
// gates on the hint keeps gating correctly if a write tool ever appears.
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, Title: title}
}

// resolve turns a path parameter into a repo-relative, slash-separated path.
// Paths arrive from an agent, so resolution is against the bound root and never
// the process's working directory, and one that climbs out of the root is
// refused rather than read.
func (s *server) resolve(p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", errors.New("path is required")
	}
	abs := p
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(s.root, p)
	}
	abs = filepath.Clean(abs)

	roots := []string{s.root}
	if resolved, err := filepath.EvalSymlinks(s.root); err == nil && resolved != s.root {
		roots = append(roots, resolved)
	}
	for _, root := range roots {
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return filepath.ToSlash(rel), nil
	}
	return "", fmt.Errorf("%s is outside the repository this server serves", p)
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
