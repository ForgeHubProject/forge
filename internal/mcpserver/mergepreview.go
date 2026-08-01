package mcpserver

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/forgehubproject/forge/internal/handler"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type mergePreviewIn struct {
	Base     string `json:"base" jsonschema:"the revision that would be merged into — the side a merge calls \"ours\""`
	Head     string `json:"head" jsonschema:"the revision that would be merged in — the side a merge calls \"theirs\""`
	After    string `json:"after,omitempty" jsonschema:"a path from a previous response — the listing resumes after it, across both lists, which is how a merge touching more paths than the cap is walked"`
	MaxPaths int    `json:"max_paths,omitempty" jsonschema:"the whole response's cap: most paths to list across both lists, and the conflicts those paths share between them; defaults to 200"`
}

type mergePreviewOut struct {
	Comparison comparison         `json:"comparison" jsonschema:"the two revisions as the caller named them"`
	BaseCommit string             `json:"baseCommit" jsonschema:"the commit base resolved to"`
	HeadCommit string             `json:"headCommit" jsonschema:"the commit head resolved to"`
	MergeBase  string             `json:"mergeBase,omitempty" jsonschema:"their common ancestor, absent when they have none"`
	Relation   string             `json:"relation" jsonschema:"identical, contained (head is already in base), fast-forward (base is already in head), diverged, or unrelated (no common ancestor)"`
	Mergeable  string             `json:"mergeable" jsonschema:"clean, conflicts, or unknown — unknown when something in this merge could not be previewed here, which is never the same as clean"`
	Files      []mergePreviewFile `json:"files" jsonschema:"one entry per path both sides changed that a handler claims here"`
	GitDecides []gitDecision      `json:"gitDecides" jsonschema:"the paths both sides changed that no handler claims: forge has no semantic answer for them and git, not forge, decides them"`
	Truncated  truncation         `json:"truncated" jsonschema:"how many of the paths both sides changed this response carries"`
	Note       string             `json:"note,omitempty"`
}

// mergePreviewFile is one path a handler claims, merged in memory. The conflicts
// are addressed exactly as forge_conflicts addresses them, because they are the
// same call answered against two commits instead of against the index.
type mergePreviewFile struct {
	Path         string             `json:"path"`
	HandlerID    string             `json:"handlerId" jsonschema:"the handler that merged this path"`
	HandlerBuild string             `json:"handlerBuild,omitempty" jsonschema:"build of the installed handler that produced this answer"`
	Clean        bool               `json:"clean" jsonschema:"true when the handler reconciled both sides with nothing left to decide"`
	Conflicts    []semanticConflict `json:"conflicts,omitempty" jsonschema:"the semantic units the handler could not reconcile"`
	Total        int                `json:"conflictCount" jsonschema:"how many conflicts this path has in total; more than the list holds means this response withheld some"`
	Truncated    *truncation        `json:"truncated,omitempty" jsonschema:"present when this path's conflict list was cut"`
	Note         string             `json:"note,omitempty"`
}

// gitDecision is one path forge has no semantic answer for. Saying which of the
// two it is matters: a path git would conflict on is a merge that stops, and a
// path nothing here could preview is not a promise that it would not.
type gitDecision struct {
	Path    string `json:"path"`
	Outcome string `json:"outcome" jsonschema:"clean or conflict as git's own merge reports it, or notPreviewed when git here could not be asked"`
	Note    string `json:"note,omitempty"`
}

// mergePreview answers whether two revisions would merge, before anything is
// merged. It is the local, semantic half of what a forge repository has been
// missing since conflict resolution shipped: there is a tool that decides a
// semantic conflict and none that shows one coming, and no peer server can offer
// this at all — the answer lives inside a file whose format git can only call
// binary.
//
// Nothing here writes. The three sides of every path are read out of history and
// merged in this process's memory; git's own merge, where it is asked, is asked
// with the objects it writes redirected outside this repository. The index, the
// working tree and the object store are exactly as they were when this returns,
// which is what lets the tool be annotated read-only and run without asking.
func (s *server) mergePreview(ctx context.Context, _ *mcp.CallToolRequest, in mergePreviewIn) (*mcp.CallToolResult, mergePreviewOut, error) {
	out := mergePreviewOut{
		Comparison: comparison{Base: strings.TrimSpace(in.Base), Head: strings.TrimSpace(in.Head)},
		Files:      []mergePreviewFile{},
		GitDecides: []gitDecision{},
		Mergeable:  "clean",
	}
	if out.Comparison.Base == "" || out.Comparison.Head == "" {
		return nil, out, errors.New("base and head are both required: a merge preview is about two revisions")
	}

	var err error
	if out.BaseCommit, err = forgerepo.ResolveRev(ctx, s.root, out.Comparison.Base); err != nil {
		return nil, out, err
	}
	if out.HeadCommit, err = forgerepo.ResolveRev(ctx, s.root, out.Comparison.Head); err != nil {
		return nil, out, err
	}

	if out.BaseCommit == out.HeadCommit {
		out.Relation, out.MergeBase = "identical", out.BaseCommit
		out.Note = fmt.Sprintf("%s and %s are the same commit, so there is nothing to merge", out.Comparison.Base, out.Comparison.Head)
		return nil, out, nil
	}

	ancestor, err := forgerepo.GitOutput(ctx, s.root, "merge-base", out.BaseCommit, out.HeadCommit)
	out.MergeBase = strings.TrimSpace(string(ancestor))
	if err != nil || out.MergeBase == "" {
		out.Relation, out.Mergeable = "unrelated", "unknown"
		out.Note = fmt.Sprintf("%s and %s share no commit, so there is no common ancestor to merge from and no three-way merge to preview. git itself refuses such a merge unless it is told to allow unrelated histories, and forge has no handler call that merges two sides without a base",
			out.Comparison.Base, out.Comparison.Head)
		return nil, out, nil
	}
	switch out.MergeBase {
	case out.HeadCommit:
		out.Relation = "contained"
		out.Note = fmt.Sprintf("%s is already an ancestor of %s, so merging it in changes nothing and cannot conflict", out.Comparison.Head, out.Comparison.Base)
		return nil, out, nil
	case out.BaseCommit:
		out.Relation = "fast-forward"
		out.Note = fmt.Sprintf("%s is an ancestor of %s, so merging %s into it moves it forward with no merge to compute and nothing to conflict", out.Comparison.Base, out.Comparison.Head, out.Comparison.Head)
		return nil, out, nil
	}
	out.Relation = "diverged"

	base := forgerepo.RevisionSource(out.MergeBase, out.MergeBase)
	ours := forgerepo.RevisionSource(out.Comparison.Base, out.BaseCommit)
	theirs := forgerepo.RevisionSource(out.Comparison.Head, out.HeadCommit)
	paths, err := s.bothSidesChanged(ctx, base, ours, theirs)
	if err != nil {
		return nil, out, err
	}
	if in.After != "" {
		after, err := s.resolve(in.After)
		if err != nil {
			return nil, out, err
		}
		rest, found := filesAfter(paths, after)
		if !found {
			out.Note = fmt.Sprintf("%q is not a path both sides changed, so there is nothing to continue from — pass a path from a previous response", in.After)
			return nil, out, nil
		}
		paths = rest
	}

	max := capOf(in.MaxPaths)
	out.Truncated = truncation{Truncated: len(paths) > max, Returned: len(paths), Total: len(paths)}
	if out.Truncated.Truncated {
		out.Truncated.Returned = max
		paths = paths[:max]
		out.Truncated.Hint = fmt.Sprintf("%d of %d paths changed on both sides listed; call again with after=%q for the next page, or raise max_paths. mergeable describes the paths listed here and not the ones withheld.",
			max, out.Truncated.Total, paths[len(paths)-1])
	}
	if len(paths) == 0 {
		switch {
		case in.After != "":
			out.Note = "that was the last path both sides changed, so nothing follows it"
		default:
			out.Note = "these two revisions changed no path in common, so there is nothing for a merge to reconcile"
		}
		return nil, out, nil
	}

	// The cap is the whole response's, not each path's, for the reason
	// forge_conflicts caps a file list the same way: a cap applied per path
	// multiplies by the list, and the list not being cut would report that as
	// complete.
	perPath := max / len(paths)
	if perPath < 1 {
		perPath = 1
	}

	reg := forgerepo.Registry(ctx, s.root)
	var unclaimed []string
	why := map[string]string{}
	for _, path := range paths {
		h, err := reg.Resolve(path)
		if err != nil || !forgerepo.IsBinaryHandler(h) {
			unclaimed = append(unclaimed, path)
			continue
		}
		entry, unmergeable := s.previewFile(ctx, h, path, base, ours, theirs, perPath)
		if unmergeable != "" {
			// A path a handler claims but cannot be handed three blobs for is not one
			// forge answers semantically, whatever its extension says. git decides it,
			// and listing it as previewed would be a guess wearing an answer's shape.
			unclaimed = append(unclaimed, path)
			why[path] = unmergeable
			continue
		}
		out.Files = append(out.Files, entry)
		switch {
		case entry.Total > 0:
			out.Mergeable = "conflicts"
		case !entry.Clean && out.Mergeable == "clean":
			out.Mergeable = "unknown"
		}
	}

	if len(unclaimed) > 0 {
		conflicted, unasked := s.gitWouldConflict(ctx, out.BaseCommit, out.HeadCommit)
		// unclaimed keeps the order of paths, which is sorted, so the list below is
		// sorted too.
		for _, path := range unclaimed {
			d := gitDecision{Path: path, Outcome: "clean", Note: why[path]}
			switch {
			case unasked != "":
				d.Outcome = "notPreviewed"
				d.Note = joinNotes(d.Note, unasked)
				if out.Mergeable == "clean" {
					out.Mergeable = "unknown"
				}
			case conflicted[path]:
				d.Outcome = "conflict"
				out.Mergeable = "conflicts"
			}
			out.GitDecides = append(out.GitDecides, d)
		}
		out.Note = fmt.Sprintf("%d of the %d paths listed here %s no handler in this repository, so forge has no semantic answer for %s and git, not forge, decides %s; gitDecides carries what git's own merge reports for each.",
			len(out.GitDecides), len(paths), plural(len(out.GitDecides), "has", "have"),
			plural(len(out.GitDecides), "it", "them"), plural(len(out.GitDecides), "it", "them"))
	}
	return nil, out, nil
}

// bothSidesChanged lists the paths each side changed since the ancestor and
// keeps the ones that appear on both, sorted. A path only one side changed needs
// no merge and no preview: whichever side changed it is what the merge takes.
func (s *server) bothSidesChanged(ctx context.Context, base, ours, theirs forgerepo.Source) ([]string, error) {
	changedOurs, err := forgerepo.ChangedPaths(ctx, s.root, base, ours, nil)
	if err != nil {
		return nil, err
	}
	changedTheirs, err := forgerepo.ChangedPaths(ctx, s.root, base, theirs, nil)
	if err != nil {
		return nil, err
	}
	onOurs := make(map[string]bool, len(changedOurs))
	for _, p := range changedOurs {
		onOurs[p] = true
	}
	var both []string
	for _, p := range changedTheirs {
		if onOurs[p] {
			both = append(both, p)
		}
	}
	sort.Strings(both)
	return both, nil
}

// previewFile merges one path's three sides in memory. The second return is why
// this path has no semantic preview at all — empty when it has one.
func (s *server) previewFile(ctx context.Context, h handler.ForgeHandler, path string, base, ours, theirs forgerepo.Source, max int) (mergePreviewFile, string) {
	id := forgerepo.HandlerFormat(h)
	entry := mergePreviewFile{Path: path, HandlerID: id, HandlerBuild: fhr.InstalledHandlerBuild(id)}

	stages, why := s.previewStages(ctx, path, base, ours, theirs)
	if why != "" {
		return entry, why
	}
	// The merged blob is discarded on purpose. A preview says whether these two
	// sides reconcile; the result of reconciling them belongs to a merge, and a
	// merge is not what this is.
	_, ci, err := h.Merge(stages.base, stages.ours, stages.theirs)
	if err != nil {
		// The handler's own words, not a paraphrase: "merge is not supported" and
		// "this file defeated me" are different answers and a caller acts on them
		// differently.
		entry.Note = fmt.Sprintf("handler %q did not merge this path (%v), so whether these two sides reconcile is unknown here rather than clean", id, err)
		return entry, ""
	}
	if ci == nil || len(ci.Conflicts) == 0 {
		entry.Clean = true
		return entry, ""
	}

	entry.Total = len(ci.Conflicts)
	shown := ci.Conflicts
	if len(shown) > max {
		shown = shown[:max]
		entry.Truncated = &truncation{
			Truncated: true,
			Returned:  len(shown),
			Total:     entry.Total,
			Hint: fmt.Sprintf("%d of %d conflicts in this path returned; raise max_paths, or call again with after set to the path before it so this one has more of the cap to itself.",
				len(shown), entry.Total),
		}
	}
	for _, c := range shown {
		entry.Conflicts = append(entry.Conflicts, semanticConflict{Path: c.Path, Ours: c.Ours, Theirs: c.Theirs})
	}
	return entry, ""
}

// previewStages reads one path's three sides out of history: the common
// ancestor, the side merged into, and the side merged in. It is the read
// forge_conflicts makes against the index's three stages, made against commits
// instead — and it is only a read.
//
// An ancestor holding nothing is not a refusal. Both sides adding the same path
// is a real merge with an empty base, which is exactly what a handler is handed
// for it. A side holding nothing, or holding something that is not a file, is:
// that is a disagreement about whether the path exists at all, and no handler
// merges content that is not there.
func (s *server) previewStages(ctx context.Context, path string, base, ours, theirs forgerepo.Source) (indexStages, string) {
	var st indexStages
	for _, side := range []struct {
		src  forgerepo.Source
		into *[]byte
		role string
	}{{ours, &st.ours, "merged into"}, {theirs, &st.theirs, "merged in"}} {
		if forgerepo.SourceEntry(ctx, s.root, side.src, path) != "blob" {
			return st, fmt.Sprintf("%s, the side being %s, holds no file at this path, so the two sides disagree about whether it exists rather than about what is inside it — which is git's to decide and not a handler's", side.src.Name, side.role)
		}
		blob, err := forgerepo.GitOutput(ctx, s.root, "show", side.src.Rev+":"+path)
		if err != nil {
			return st, fmt.Sprintf("%s, the side being %s, could not be read at this path (%v), so nothing was merged for it", side.src.Name, side.role, err)
		}
		*side.into = blob
	}
	if forgerepo.SourceEntry(ctx, s.root, base, path) == "blob" {
		st.base, _ = forgerepo.GitOutput(ctx, s.root, "show", base.Rev+":"+path)
	}
	return st, ""
}

// gitWouldConflict asks git which paths its own merge of these two commits comes
// out conflicted on, for the paths forge has no semantic answer for. The second
// return is why git could not be asked — empty when it was.
//
// git's real merge writes the objects it produces, and this tool writes nothing,
// so the objects go to a directory outside the repository and the repository's
// own object store is added back as a read-only alternate. The merged tree is
// then thrown away with that directory: what is wanted from it is the list of
// paths it could not settle, and nothing else.
//
// This does not go through forgerepo.GitOutput, and that is deliberate: a merge
// that conflicts exits non-zero with the answer on stdout, so the one contract
// GitOutput keeps — a non-zero exit is a failure carrying git's message — is the
// wrong one to read this through.
func (s *server) gitWouldConflict(ctx context.Context, base, head string) (map[string]bool, string) {
	objects, err := os.MkdirTemp("", "forge-merge-preview-")
	if err != nil {
		return nil, fmt.Sprintf("git's own merge was not run: it writes what it merges, and there was nowhere outside this repository to put that (%v)", err)
	}
	defer os.RemoveAll(objects)

	alternates := s.objectsDir(ctx)
	if existing := os.Getenv("GIT_ALTERNATE_OBJECT_DIRECTORIES"); existing != "" {
		alternates += string(filepath.ListSeparator) + existing
	}
	c := exec.CommandContext(ctx, "git", "merge-tree", "--write-tree", "--name-only", "-z", base, head)
	c.Dir = s.root
	c.Env = append(os.Environ(), "GIT_OBJECT_DIRECTORY="+objects, "GIT_ALTERNATE_OBJECT_DIRECTORIES="+alternates)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()

	// Exit 1 is a conflicted merge, which is an answer. Anything else is git
	// declining to give one — an installed git too old for this form of the
	// command among them, which is the case this fallback exists for.
	var exitErr *exec.ExitError
	if err != nil && (!errors.As(err, &exitErr) || exitErr.ExitCode() != 1) {
		why := strings.TrimSpace(stderr.String())
		if why == "" {
			why = err.Error()
		}
		return nil, fmt.Sprintf("git here did not answer whether its own merge of these two revisions conflicts (%s), so this path is reported as it stands rather than guessed at", firstLine(why))
	}

	// <merged tree object id> NUL <conflicted path> NUL ... NUL NUL <messages>
	fields := strings.Split(string(out), "\x00")
	conflicted := map[string]bool{}
	for _, path := range fields[1:] {
		if path == "" {
			break
		}
		conflicted[path] = true
	}
	return conflicted, ""
}

// objectsDir is where this repository keeps its objects, which the merge above
// reads through as an alternate while writing somewhere else entirely.
func (s *server) objectsDir(ctx context.Context) string {
	out, err := forgerepo.GitOutput(ctx, s.root, "rev-parse", "--git-path", "objects")
	dir := strings.TrimSpace(string(out))
	if err != nil || dir == "" {
		return filepath.Join(s.root, ".git", "objects")
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(s.root, dir)
	}
	return dir
}

// firstLine keeps a git refusal to the sentence that says what it refused, so a
// usage block does not become the note.
func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

// joinNotes keeps both halves of a note that has two reasons to exist.
func joinNotes(a, b string) string {
	if a == "" {
		return b
	}
	return a + " " + b
}
