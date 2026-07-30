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
	"path/filepath"
	"strings"

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
  "at" and narrow with "kinds".

  Truncation is always explicit. A capped response carries truncated, returned,
  total and a hint naming the paths that drill deeper. A response that does not
  say it was truncated was not truncated.

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
func Run(ctx context.Context) error {
	root, err := repoRoot()
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "forge mcp: serving %s on stdio\n", root)
	return New(root).Run(ctx, &mcp.StdioTransport{})
}

// repoRoot resolves the one repository this server answers for. A server
// outside a repository has nothing to resolve paths against, so it refuses to
// start rather than answering from whatever directory it happens to be in.
func repoRoot() (string, error) {
	out, err := forgerepo.GitOutput("", "rev-parse", "--show-toplevel")
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
}

// New builds the MCP server for one repository root.
func New(root string) *mcp.Server {
	s := &server{root: root}
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
	}, s.status)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "forge_semantic_diff",
		Annotations: readOnly("Semantic diff of one file"),
		Description: `What changed inside one file, as the handler that claims it reports it. This is
the capability nothing else here has: for a file with a handler, plain git can
only say the bytes differ.

Revisions work exactly as the CLI's do. Neither base nor head compares the
working tree against HEAD; base alone compares the working tree against base;
base and head compare one revision against the other. comparison echoes what was
actually compared.

Nothing is complete by default. The response carries a summary (counts by kind
and the roots of the change tree, each with its child count), a depth-first slice
of the tree capped at max_changes, and truncated{returned,total,hint}. Drill in
by passing a path from a previous response as "at"; narrow with kinds; or raise
max_changes. Every node's path is a stable address you can pass back as "at".

A path no handler claims is not an error: handlerId comes back null, fallback is
"text", and git's own text diff is returned under the same explicit cap.

Cannot answer anything about a file's raw bytes, and writes nothing.`,
	}, s.semanticDiff)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "forge_show",
		Annotations: readOnly("What a commit changed"),
		Description: `What one commit changed, file by file, semantically.

Answers: the commit's own metadata (sha, author, date, subject, first parent),
and one entry per changed file — the same summary shape forge_semantic_diff
returns for a file with a handler, git's line counts for a file without one.

The comparison is the commit against its first parent, or against nothing for a
root commit. Pass path to narrow to a single file and get its change tree,
capped at max_changes with the same explicit truncation. For any other pair of
revisions, use forge_semantic_diff with base and head.`,
	}, s.show)

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
tools will fall back to text for.`,
	}, s.handlerFor)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "forge_formats",
		Annotations: readOnly("Formats configured for this repository"),
		Description: `Which file extensions this repository has opted into, which it has deliberately
ignored, and the handler state of each: id, whether it is installed, the
installed build, and the build the repository pins.

Answers "what can forge be asked about here" before any path is named. An
extension listed with no installed handler is inactive — semantic tools fall
back to text for it until a human installs one.

Changes nothing: adding, ignoring, or installing a format is a terminal command.`,
	}, s.formats)

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
	}, s.sourceList)

	return srv
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
