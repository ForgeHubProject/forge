package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type showIn struct {
	Ref        string `json:"ref" jsonschema:"the commit to show — anything git resolves (sha, branch, tag, HEAD~2)"`
	Path       string `json:"path,omitempty" jsonschema:"narrow to one file and return its change tree, not just its summary; a directory narrows the listing but returns no tree, since one tree per file under it is the response the cap exists to prevent"`
	After      string `json:"after,omitempty" jsonschema:"a path from a previous response's files — the listing resumes after it, which is how a commit that changed more files than the cap is walked"`
	MaxChanges int    `json:"max_changes,omitempty" jsonschema:"the whole response's cap, not each file's: most files to list, most changes to return for a named path, and the roots the listed files share between them; defaults to 200"`
}

type showOut struct {
	Commit     commitInfo `json:"commit"`
	Comparison comparison `json:"comparison"`
	Files      []showFile `json:"files" jsonschema:"one entry per file the commit changed"`
	Truncated  truncation `json:"truncated" jsonschema:"how much of the file list this response carries"`
	Note       string     `json:"note,omitempty"`
}

type commitInfo struct {
	SHA     string `json:"sha"`
	Author  string `json:"author"`
	Date    string `json:"date" jsonschema:"author date, ISO 8601"`
	Subject string `json:"subject"`
	Parent  string `json:"parent,omitempty" jsonschema:"first parent, absent for a root commit"`
}

type showFile struct {
	Path         string       `json:"path"`
	HandlerID    *string      `json:"handlerId" jsonschema:"the handler that claims this path, or null when none does"`
	HandlerBuild string       `json:"handlerBuild,omitempty" jsonschema:"build of the installed handler that produced this entry"`
	Summary      *diffSummary `json:"summary,omitempty" jsonschema:"shape of this file's change tree, for a file with a handler"`
	Changes      []changeNode `json:"changes,omitempty" jsonschema:"the change tree, present only when the call named this path"`
	Truncated    *truncation  `json:"truncated,omitempty" jsonschema:"present alongside changes"`
	TextSummary  string       `json:"textSummary,omitempty" jsonschema:"git's own line counts, for a file no handler claims"`
	Note         string       `json:"note,omitempty"`
}

// show reports what one commit changed, per file, against its first parent —
// the same comparison forge show renders, in the same order git lists it.
func (s *server) show(ctx context.Context, _ *mcp.CallToolRequest, in showIn) (*mcp.CallToolResult, showOut, error) {
	var out showOut

	if strings.TrimSpace(in.Ref) == "" {
		return nil, out, fmt.Errorf("ref is required")
	}
	commit, err := forgerepo.ResolveRev(ctx, s.root, in.Ref)
	if err != nil {
		return nil, out, err
	}

	var paths []string
	if in.Path != "" {
		path, err := s.resolve(in.Path)
		if err != nil {
			return nil, out, err
		}
		paths = []string{path}
	}

	head := forgerepo.RevisionSource(in.Ref, commit)
	base := forgerepo.EmptySource()
	if parent, err := forgerepo.ResolveRev(ctx, s.root, commit+"^1"); err == nil {
		base = forgerepo.RevisionSource(in.Ref+"^", parent)
	}
	out.Comparison = comparison{Base: base.Name, Head: head.Name}
	if out.Commit, err = s.commitInfo(ctx, commit); err != nil {
		return nil, out, err
	}

	files, err := forgerepo.ChangedPaths(ctx, s.root, base, head, paths)
	if err != nil {
		return nil, out, err
	}
	if in.After != "" {
		after, err := s.resolve(in.After)
		if err != nil {
			return nil, out, err
		}
		rest, found := filesAfter(files, after)
		if !found {
			out.Note = fmt.Sprintf("this commit changed nothing at %q, so there is nothing to continue from — pass a path from a previous response's files", in.After)
			out.Files = []showFile{}
			return nil, out, nil
		}
		files = rest
	}
	max := capOf(in.MaxChanges)
	out.Truncated = truncation{Truncated: len(files) > max, Returned: len(files), Total: len(files)}
	if out.Truncated.Truncated {
		out.Truncated.Returned = max
		files = files[:max]
		out.Truncated.Hint = fmt.Sprintf("%d of %d changed files listed; call again with after=%q for the next page, pass path to ask about one file, or raise max_changes.",
			max, out.Truncated.Total, files[len(files)-1])
	}
	if len(files) == 0 {
		switch {
		case in.After != "":
			out.Note = fmt.Sprintf("%q is the last file this commit changed, so nothing follows it", in.After)
		case len(paths) == 0:
			out.Note = "this commit changed no files"
		default:
			out.Note = "this commit changed nothing under the given path"
		}
		out.Files = []showFile{}
		return nil, out, nil
	}

	// max_changes is the whole response's cap, not each file's. Capping every file
	// on its own multiplies the cap by the file list — two hundred files each
	// naming two hundred roots is exactly the response the cap exists to prevent,
	// and the file list not being cut would report it as complete — so the roots a
	// summary may name are the cap divided among the files being listed. A file
	// whose share cannot hold its roots still reports how many it has, and its note
	// names the call that returns them.
	perFile := max / len(files)
	if perFile < 1 {
		perFile = 1
	}

	reg := forgerepo.Registry(ctx, s.root)
	out.Files = make([]showFile, 0, len(files))
	shared := false
	for _, path := range files {
		entry := showFile{Path: path}
		fc, err := forgerepo.CompareFile(ctx, s.root, reg, path, base, head)
		switch {
		case err != nil:
			// One file forge cannot read does not cost the rest of the commit —
			// the same choice the CLI makes, with the reason carried per file.
			entry.Note = err.Error()
		case !fc.BaseFound && !fc.HeadFound:
			entry.Note = fmt.Sprintf("in neither %s nor %s", base, head)
		case !fc.Semantic:
			entry.TextSummary = forgerepo.TextChangeSummary(ctx, s.root, base, head, path)
			entry.Note = "no handler claims this path; these are git's line counts"
		default:
			id := fc.HandlerID
			entry.HandlerID = &id
			entry.HandlerBuild = fhr.InstalledHandlerBuild(id)
			summary := summarize(fc.Diff.Changes, "", perFile)
			entry.Summary = &summary
			shared = shared || summary.TopLevelWithheld > 0
			// The tree comes back for the file the call named, and only for it: a
			// directory is a valid pathspec, and one whole tree per file under it is
			// the same multiplication in another shape.
			if len(paths) == 1 && path == paths[0] {
				nodes, t, c := renderTree(fc.Diff.Changes, "", max)
				if t.Truncated {
					t.Hint = showTreeHint(t, c, base, head, path)
				}
				entry.Changes, entry.Truncated = nodes, &t
			} else if summary.Total > 0 {
				entry.Note = nextStep(base, head, path)
			}
		}
		out.Files = append(out.Files, entry)
	}
	if shared && out.Note == "" {
		out.Note = fmt.Sprintf("summary.topLevel is capped across this whole response, not per file: the %d files listed share max_changes=%d, so a file's topLevelWithheld is what its share could not name. Each such file's note names the call that returns its own tree.",
			len(files), max)
	}
	return nil, out, nil
}

// showTreeHint says what a capped change tree withheld in forge_show's own
// vocabulary. The tree's cursors are not forge_show's: this tool has no "at" at
// all, and its "after" names a file in the listing, so handing those back would
// give a caller one instruction the schema rejects and one that is silently read
// as a file path. The hint names the tool the cursors belong to, and the call
// that reaches this file there.
func showTreeHint(t truncation, c cursors, base, head forgerepo.Source, path string) string {
	h := fmt.Sprintf("%d of %d changes in this file returned. forge_show has no cursor into a change tree — call %s to page it",
		t.Returned, t.Total, semanticDiffCall(base, head, path))
	if listed, more := c.atPaths(); listed != "" {
		h += fmt.Sprintf(", passing at=<one of the paths this response withheld: %s", listed)
		if more > 0 {
			h += fmt.Sprintf(", and %d more", more)
		}
		h += ">"
	}
	if calls := c.afterCalls(); len(calls) > 0 {
		h += ", or " + strings.Join(calls, ", then ")
	}
	return h + ". max_changes raises the cap here too."
}

// filesAfter returns the changed files that follow one path in the listing — the
// page "after" asks for — and whether the listing holds that path at all.
func filesAfter(files []string, after string) ([]string, bool) {
	for i, p := range files {
		if p == after {
			return files[i+1:], true
		}
	}
	return nil, false
}

// semanticDiffCall names the forge_semantic_diff call that answers for one file
// in this comparison. A root commit's base side is nothing, and nothing is not a
// revision: naming it as one would hand the caller an instruction that can only
// fail, so that comparison is named the way forge_semantic_diff accepts it.
func semanticDiffCall(base, head forgerepo.Source, path string) string {
	if base.Kind == forgerepo.SourceEmpty {
		return fmt.Sprintf("forge_semantic_diff with base_empty=true head=%q path=%q", head.Name, path)
	}
	return fmt.Sprintf("forge_semantic_diff with base=%q head=%q path=%q", base.Name, head.Name, path)
}

// nextStep is that call, offered to a file this response summarised but did not
// open.
func nextStep(base, head forgerepo.Source, path string) string {
	return "call " + semanticDiffCall(base, head, path) + " for this file's change tree"
}

// commitInfo reads the commit's own header from git, one record, so the fields
// arrive already separated rather than parsed back out of a formatted block.
func (s *server) commitInfo(ctx context.Context, commit string) (commitInfo, error) {
	out, err := forgerepo.GitOutput(ctx, s.root, "show", "--no-patch", "--format=%H%x00%an <%ae>%x00%aI%x00%s%x00%P", commit)
	if err != nil {
		return commitInfo{}, err
	}
	fields := strings.Split(strings.TrimRight(string(out), "\n"), "\x00")
	if len(fields) < 5 {
		return commitInfo{SHA: commit}, nil
	}
	info := commitInfo{SHA: fields[0], Author: fields[1], Date: fields[2], Subject: fields[3]}
	if parents := strings.Fields(fields[4]); len(parents) > 0 {
		info.Parent = parents[0]
	}
	return info, nil
}
