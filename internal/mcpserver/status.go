package mcpserver

import (
	"context"
	"sort"
	"strings"

	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/forgehubproject/forge/internal/handler"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type statusOut struct {
	Root     string        `json:"root" jsonschema:"absolute path of the repository this server serves; every path parameter is resolved against it"`
	Branch   string        `json:"branch,omitempty" jsonschema:"the branch checked out, absent when HEAD is detached"`
	Head     string        `json:"head,omitempty" jsonschema:"commit HEAD points at, absent in a repository with no commits"`
	Detached bool          `json:"detached" jsonschema:"true when HEAD names a commit rather than a branch"`
	Clean    bool          `json:"clean" jsonschema:"true when nothing is changed or untracked"`
	Entries  []statusEntry `json:"entries" jsonschema:"one entry per changed or untracked path"`
}

type statusEntry struct {
	Path      string `json:"path" jsonschema:"repository-relative path"`
	State     string `json:"state" jsonschema:"modified, added, deleted, renamed, copied, typechange, untracked or unmerged"`
	Staged    bool   `json:"staged" jsonschema:"true when the index holds a change to this path"`
	Unstaged  bool   `json:"unstaged" jsonschema:"true when the working tree holds a change the index does not"`
	HandlerID string `json:"handlerId,omitempty" jsonschema:"the handler that claims this path — its presence is what makes the path answerable by forge_semantic_diff; absent when the repository has not opted the extension in or no handler is installed"`
}

// status reports the working tree the way forge status does, minus the colours:
// what changed, and which of it a handler can explain.
func (s *server) status(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, statusOut, error) {
	out := statusOut{Root: s.root, Entries: []statusEntry{}}

	if head, err := forgerepo.GitOutput(ctx, s.root, "rev-parse", "HEAD"); err == nil {
		out.Head = strings.TrimSpace(string(head))
	}
	if branch, err := forgerepo.GitOutput(ctx, s.root, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		out.Branch = strings.TrimSpace(string(branch))
	}
	out.Detached = out.Branch == "" && out.Head != ""

	// git's own porcelain, rather than go-git's status, for the same reason forge
	// status defers to it for untracked files: the whole ignore stack, and git's
	// collapsing of a wholly-untracked directory to one entry — which is also what
	// keeps this response bounded in a tree full of new files.
	raw, err := forgerepo.GitOutput(ctx, s.root, "status", "--porcelain=v1", "-z")
	if err != nil {
		return nil, out, err
	}

	reg := forgerepo.Registry(ctx, s.root)
	entries := parsePorcelain(string(raw))
	for i := range entries {
		entries[i].HandlerID = handlerID(reg, entries[i].Path)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if len(entries) > 0 {
		out.Entries = entries
	}
	out.Clean = len(entries) == 0
	return nil, out, nil
}

// parsePorcelain reads `git status --porcelain=v1 -z` records. A rename or copy
// spends two NUL-terminated fields — the new path then the old — so the reader
// consumes the second rather than reading it as a path of its own.
func parsePorcelain(raw string) []statusEntry {
	fields := strings.Split(raw, "\x00")
	var entries []statusEntry
	for i := 0; i < len(fields); i++ {
		record := fields[i]
		if len(record) < 4 {
			continue
		}
		x, y := record[0], record[1]
		path := record[3:]
		if x == 'R' || x == 'C' || y == 'R' || y == 'C' {
			i++ // the source half of the rename or copy
		}
		entries = append(entries, statusEntry{
			Path:     path,
			State:    stateWord(x, y),
			Staged:   x != ' ' && x != '?',
			Unstaged: y != ' ' && y != '?',
		})
	}
	return entries
}

// stateWord names what happened to a path, from git's two status codes. The
// unstaged side wins where both say something, as it is the more recent.
func stateWord(x, y byte) string {
	if x == '?' || y == '?' {
		return "untracked"
	}
	if x == 'U' || y == 'U' || (x == 'A' && y == 'A') || (x == 'D' && y == 'D') {
		return "unmerged"
	}
	code := y
	if code == ' ' {
		code = x
	}
	switch code {
	case 'A':
		return "added"
	case 'D':
		return "deleted"
	case 'R':
		return "renamed"
	case 'C':
		return "copied"
	case 'T':
		return "typechange"
	case 'M':
		return "modified"
	default:
		return "modified"
	}
}

// handlerID names the handler that claims a path, or "" where the answer would
// be git's own text diff. The text catch-all is deliberately not reported as a
// handler: reporting it would tell a caller a path is semantically answerable
// when it is not.
func handlerID(reg *handler.Registry, path string) string {
	if strings.HasSuffix(path, "/") {
		return "" // git collapses a wholly-untracked directory; no per-file handler applies
	}
	h, err := reg.Resolve(path)
	if err != nil || !forgerepo.IsBinaryHandler(h) {
		return ""
	}
	return forgerepo.HandlerFormat(h)
}
