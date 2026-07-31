package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type addIn struct {
	Paths []string `json:"paths" jsonschema:"repository-relative paths to stage; a directory stages what changed under it, and \".\" stages everything"`
}

type addOut struct {
	Staged  []string      `json:"staged" jsonschema:"the paths as they were passed to git, resolved against the repository root"`
	Entries []statusEntry `json:"entries" jsonschema:"what the index and working tree hold for those paths now, in forge_status's shape"`
	Note    string        `json:"note,omitempty"`
}

// add stages paths. The pathspecs go after "--", so an argument is always a
// path: git reads a leading "-" as a flag otherwise, which turns a file name
// that came from a repository — or from a model reading one — into an option
// this server did not choose to pass.
func (s *server) add(ctx context.Context, _ *mcp.CallToolRequest, in addIn) (*mcp.CallToolResult, addOut, error) {
	out := addOut{Staged: []string{}, Entries: []statusEntry{}}

	paths, err := s.resolveAll(in.Paths)
	if err != nil {
		return nil, out, err
	}
	if _, err := forgerepo.GitOutput(ctx, s.root, append([]string{"add", "--"}, paths...)...); err != nil {
		return nil, out, err
	}
	out.Staged = paths

	entries, err := s.statusOf(ctx, paths)
	if err != nil {
		return nil, out, err
	}
	out.Entries = entries
	if len(entries) == 0 {
		out.Note = "nothing changed for those paths, so the index is as it was"
	}
	return nil, out, nil
}

type commitIn struct {
	Message string `json:"message" jsonschema:"the commit message; its first line is the subject"`
}

type commitOut struct {
	SHA     string `json:"sha" jsonschema:"the commit that was recorded"`
	Branch  string `json:"branch,omitempty" jsonschema:"the branch it was recorded on, absent when HEAD is detached"`
	Subject string `json:"subject"`
	Note    string `json:"note,omitempty"`
}

// commit records what is staged. It stages nothing on the way — an agent that
// meant to commit one file and committed the tree would have no way back that
// this server offers — and it never amends: the only commit this tool can affect
// is the one it creates.
func (s *server) commit(ctx context.Context, _ *mcp.CallToolRequest, in commitIn) (*mcp.CallToolResult, commitOut, error) {
	var out commitOut

	message := strings.TrimSpace(in.Message)
	if message == "" {
		return nil, out, errors.New("a commit needs a message")
	}
	// git's own refusals arrive here whole — nothing staged, no identity
	// configured, a hook that said no — and each is more use to a caller than any
	// summary of it this server could write.
	if _, err := forgerepo.GitOutput(ctx, s.root, "commit", "-m", message); err != nil {
		return nil, out, err
	}

	head, err := forgerepo.ResolveRev(ctx, s.root, "HEAD")
	if err != nil {
		return nil, out, err
	}
	info, err := s.commitInfo(ctx, head)
	if err != nil {
		return nil, out, err
	}
	out.SHA, out.Subject = info.SHA, info.Subject
	if branch, err := forgerepo.GitOutput(ctx, s.root, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		out.Branch = strings.TrimSpace(string(branch))
	}
	out.Note = "recorded on this machine only; nothing here pushes"
	return nil, out, nil
}

type createBranchIn struct {
	Name string `json:"name" jsonschema:"the branch to create"`
	Base string `json:"base,omitempty" jsonschema:"revision to start it at — anything git resolves; defaults to HEAD"`
}

type createBranchOut struct {
	Name   string `json:"name"`
	Base   string `json:"base" jsonschema:"the revision the branch was created at, as it was named"`
	Commit string `json:"commit" jsonschema:"the commit that revision resolved to"`
	Note   string `json:"note,omitempty"`
}

// createBranch creates a branch and leaves HEAD where it was. The name goes
// after "--" for the same reason a path does, and the base is resolved before
// the branch is made so a revision git will not accept is reported as that
// rather than as a branch failure.
func (s *server) createBranch(ctx context.Context, _ *mcp.CallToolRequest, in createBranchIn) (*mcp.CallToolResult, createBranchOut, error) {
	var out createBranchOut

	name := strings.TrimSpace(in.Name)
	if name == "" {
		return nil, out, errors.New("a branch needs a name")
	}
	base := strings.TrimSpace(in.Base)
	if base == "" {
		base = "HEAD"
	}
	commit, err := forgerepo.ResolveRev(ctx, s.root, base)
	if err != nil {
		return nil, out, err
	}
	if _, err := forgerepo.GitOutput(ctx, s.root, "branch", "--", name, commit); err != nil {
		return nil, out, err
	}
	out.Name, out.Base, out.Commit = name, base, commit
	out.Note = "created, not checked out — forge_checkout switches to it"
	return nil, out, nil
}

type checkoutIn struct {
	Target string `json:"target" jsonschema:"the branch or revision to check out; it must already exist here"`
}

type checkoutOut struct {
	Target   string `json:"target"`
	Branch   string `json:"branch,omitempty" jsonschema:"the branch now checked out, absent when the target was a commit"`
	Head     string `json:"head" jsonschema:"the commit HEAD points at now"`
	Detached bool   `json:"detached" jsonschema:"true when the target was a commit rather than a branch"`
	Note     string `json:"note,omitempty"`
}

// checkout switches the working tree, never by force.
//
// The target is resolved as a commit first, and the call ends with "--" so no
// pathspec can follow it. Both matter for the same reason: `git checkout <path>`
// is a different command that restores that path from the index, discarding
// whatever the working tree held — so a target that named a file rather than a
// revision would quietly destroy uncommitted work under a tool that promises the
// opposite. Refusing what does not resolve to a commit is what makes the promise
// true.
func (s *server) checkout(ctx context.Context, _ *mcp.CallToolRequest, in checkoutIn) (*mcp.CallToolResult, checkoutOut, error) {
	var out checkoutOut

	target := strings.TrimSpace(in.Target)
	switch {
	case target == "":
		return nil, out, errors.New("checkout needs a target")
	case strings.HasPrefix(target, "-"):
		return nil, out, fmt.Errorf("%q begins with a dash, so git would read it as an option rather than a revision", target)
	}
	if _, err := forgerepo.ResolveRev(ctx, s.root, target); err != nil {
		return nil, out, fmt.Errorf("%w\nthis tool checks out revisions only: it will not restore a file from the index, which is what git does with a target that names a path", err)
	}

	// No -f, no --force, and none is reachable from here: git's refusal to check
	// out over uncommitted work is the protection, so it is passed back untouched.
	if _, err := forgerepo.GitOutput(ctx, s.root, "checkout", target, "--"); err != nil {
		return nil, out, err
	}

	out.Target = target
	if head, err := forgerepo.ResolveRev(ctx, s.root, "HEAD"); err == nil {
		out.Head = head
	}
	if branch, err := forgerepo.GitOutput(ctx, s.root, "symbolic-ref", "--short", "-q", "HEAD"); err == nil {
		out.Branch = strings.TrimSpace(string(branch))
	}
	out.Detached = out.Branch == ""
	if out.Detached {
		out.Note = "HEAD is detached at this commit; commits made now belong to no branch until one is created here"
	}
	return nil, out, nil
}

type resetIn struct {
	Paths []string `json:"paths,omitempty" jsonschema:"repository-relative paths to unstage; omit to unstage everything"`
}

type resetOut struct {
	Unstaged []string      `json:"unstaged" jsonschema:"the paths unstaged, empty when the whole index was reset"`
	Entries  []statusEntry `json:"entries" jsonschema:"what the index and working tree hold now, in forge_status's shape"`
	Note     string        `json:"note,omitempty"`
}

// reset unstages, and does only that.
//
// git reset touches the working tree only when it is asked to, and it is not
// asked to here: no --hard, no --merge, no revision to reset onto — the arguments
// that would make it destroy an edit are not constructible from this tool's
// input. What it does discard is an arrangement of the index, which nothing can
// reconstruct, and that is what its destructive annotation is about.
func (s *server) reset(ctx context.Context, _ *mcp.CallToolRequest, in resetIn) (*mcp.CallToolResult, resetOut, error) {
	out := resetOut{Unstaged: []string{}, Entries: []statusEntry{}}

	var paths []string
	var err error
	if len(in.Paths) > 0 {
		if paths, err = s.resolveAll(in.Paths); err != nil {
			return nil, out, err
		}
		// An unmerged path has nothing staged to take back: the index holds all
		// three sides of it, and a path-scoped reset replaces them with HEAD's one
		// version. The merge stays in progress, so what is left looks like a path
		// that was resolved — nothing is unmerged, forge_status is clean, and the
		// next commit records a merge whose incoming side was never in it. That is
		// the one thing this tool must not be able to do quietly.
		unmerged, err := s.unmergedPaths(ctx)
		if err != nil {
			return nil, out, err
		}
		if hit := coveredBy(unmerged, paths); len(hit) > 0 {
			return nil, out, fmt.Errorf("%s %s unmerged, so nothing is staged there to take back — this would replace the sides the index holds with the one HEAD has, and the side being merged in is recorded nowhere else. The merge would still be in progress and %s would look resolved. Decide %s with forge_resolve_conflict, or reset only what is staged",
				strings.Join(hit, ", "), plural(len(hit), "is", "are"), plural(len(hit), "it", "they"), plural(len(hit), "it", "them"))
		}
	}
	args := []string{"reset", "-q"}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	merging := s.merging(ctx)
	if _, err := forgerepo.GitOutput(ctx, s.root, args...); err != nil {
		return nil, out, err
	}
	out.Unstaged = paths

	if out.Entries, err = s.statusOf(ctx, paths); err != nil {
		return nil, out, err
	}
	switch {
	case merging && !s.merging(ctx):
		out.Note = "the whole index was reset, which also cleared the merge in progress: the working tree still holds every file as it was, but git no longer knows a merge was under way"
	case len(paths) == 0:
		out.Note = "the whole index was reset to HEAD; no file's contents changed"
	default:
		out.Note = "unstaged; those files still hold exactly the bytes they held"
	}
	return nil, out, nil
}

// coveredBy returns the candidates a pathspec argument reaches: one named
// outright, or one under a named directory. "." is the repository root, which
// reaches everything.
func coveredBy(candidates, pathspecs []string) []string {
	var hit []string
	for _, c := range candidates {
		for _, p := range pathspecs {
			if p == "." || c == p || strings.HasPrefix(c, p+"/") {
				hit = append(hit, c)
				break
			}
		}
	}
	return hit
}

// merging reports whether a merge is in progress, which a whole-index reset ends.
func (s *server) merging(ctx context.Context) bool {
	out, err := forgerepo.GitOutput(ctx, s.root, "rev-parse", "-q", "--verify", "MERGE_HEAD")
	return err == nil && strings.TrimSpace(string(out)) != ""
}

// resolveAll resolves every path argument against the bound root, refusing the
// whole call if any one of them points outside it: a call that staged three of
// four paths and failed on the fourth would leave an index nobody asked for.
func (s *server) resolveAll(paths []string) ([]string, error) {
	if len(paths) == 0 {
		return nil, errors.New("at least one path is required")
	}
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		rel, err := s.resolve(p)
		if err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, nil
}

// statusOf reports what git says about the given paths — all of them when none
// are named — in the shape forge_status returns, handler tag included, so a write
// tool's answer can be read with the same reader as a read tool's.
func (s *server) statusOf(ctx context.Context, paths []string) ([]statusEntry, error) {
	args := []string{"status", "--porcelain=v1", "-z"}
	if len(paths) > 0 {
		args = append(append(args, "--"), paths...)
	}
	raw, err := forgerepo.GitOutput(ctx, s.root, args...)
	if err != nil {
		return nil, err
	}
	entries := parsePorcelain(string(raw))
	if len(entries) == 0 {
		return []statusEntry{}, nil
	}
	reg := forgerepo.Registry(ctx, s.root)
	for i := range entries {
		entries[i].HandlerID = handlerID(reg, entries[i].Path)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}
