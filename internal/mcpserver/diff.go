package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type semanticDiffIn struct {
	Path       string   `json:"path" jsonschema:"repository-relative path of the file to compare"`
	Base       string   `json:"base,omitempty" jsonschema:"base revision — anything git resolves (sha, branch, tag, HEAD~2); omit to compare against HEAD"`
	Head       string   `json:"head,omitempty" jsonschema:"head revision; omit to compare base against the working tree"`
	At         string   `json:"at,omitempty" jsonschema:"a change path from a previous response — returns that subtree alone"`
	Kinds      []string `json:"kinds,omitempty" jsonschema:"keep only these kinds: added, removed, modified (containers on the way to a match are kept)"`
	MaxChanges int      `json:"max_changes,omitempty" jsonschema:"most changes to return; defaults to 200, and what is withheld is always reported"`
}

type semanticDiffOut struct {
	Path         string       `json:"path" jsonschema:"the path compared, relative to the repository root"`
	HandlerID    *string      `json:"handlerId" jsonschema:"the handler that produced this answer, or null when no handler claims the path"`
	HandlerBuild string       `json:"handlerBuild,omitempty" jsonschema:"build of the installed handler that produced this answer"`
	Comparison   comparison   `json:"comparison"`
	Fallback     string       `json:"fallback,omitempty" jsonschema:"\"text\" when there is no handler and git's own diff was returned instead"`
	Summary      *diffSummary `json:"summary,omitempty" jsonschema:"shape of the change tree this response describes, after any at and kinds filters"`
	Changes      []changeNode `json:"changes,omitempty" jsonschema:"the change tree, depth-first, capped at max_changes"`
	TextDiff     string       `json:"textDiff,omitempty" jsonschema:"git's own text diff, for a path no handler claims"`
	Truncated    truncation   `json:"truncated"`
	Note         string       `json:"note,omitempty" jsonschema:"anything about this answer a caller would otherwise have to infer"`
}

// semanticDiff compares one path across two sides through the same function the
// CLI uses, so an agent and a terminal get the same answer for the same
// arguments.
func (s *server) semanticDiff(_ context.Context, _ *mcp.CallToolRequest, in semanticDiffIn) (*mcp.CallToolResult, semanticDiffOut, error) {
	var out semanticDiffOut

	path, err := s.resolve(in.Path)
	if err != nil {
		return nil, out, err
	}
	base, head, err := s.sources(in.Base, in.Head)
	if err != nil {
		return nil, out, err
	}
	out.Path = path
	out.Comparison = comparison{Base: base.Name, Head: head.Name}

	max := capOf(in.MaxChanges)
	fc, err := forgerepo.CompareFile(s.root, forgerepo.Registry(s.root), path, base, head)
	if err != nil {
		return nil, out, err
	}

	switch {
	case !fc.BaseFound && !fc.HeadFound:
		out.Note = fmt.Sprintf("%s is in neither %s nor %s", path, base, head)
		return nil, out, nil

	case !fc.Semantic:
		// Either no handler claims the extension in this repository, or a side
		// holds something other than a blob there — a submodule's pointer — which
		// no handler can be handed either way.
		out.Fallback = "text"
		out.Note = "no handler produced this answer; it is git's own text diff, capped the same way. forge_handler_for says why."
		out.TextDiff, out.Truncated, err = s.textDiff(base, head, path, max)
		if err != nil {
			return nil, out, err
		}
		return nil, out, nil
	}

	id := fc.HandlerID
	out.HandlerID = &id
	out.HandlerBuild = fhr.InstalledHandlerBuild(id)

	changes := fc.Diff.Changes
	if in.At != "" {
		changes = subtreeAt(changes, "", in.At)
		if len(changes) == 0 {
			out.Note = fmt.Sprintf("this comparison has no change at %q — pass a path from a previous response's changes or summary.topLevel", in.At)
			empty := summarize(nil, max)
			out.Summary = &empty
			return nil, out, nil
		}
	}
	if want, ok := kindSet(in.Kinds); ok {
		changes = filterKinds(changes, want)
	}

	summary := summarize(changes, max)
	out.Summary = &summary
	out.Changes, out.Truncated = renderTree(changes, max)
	if summary.Total == 0 {
		out.Note = "the handler found no semantic change here"
	}
	return nil, out, nil
}

// sources maps the base and head parameters onto the two sides to compare,
// through the CLI's own mapping so the revision semantics cannot drift: neither
// is the working tree against HEAD, base alone is the working tree against that
// revision, both is revision to revision.
func (s *server) sources(base, head string) (forgerepo.Source, forgerepo.Source, error) {
	switch {
	case base == "" && head == "":
		return forgerepo.DiffSources(s.root, nil)
	case base != "" && head == "":
		return forgerepo.DiffSources(s.root, []string{base})
	case base != "":
		return forgerepo.DiffSources(s.root, []string{base, head})
	default:
		return forgerepo.Source{}, forgerepo.Source{}, errors.New(
			`"head" needs a "base": with neither, the comparison is the working tree against HEAD; with base alone, the working tree against base`)
	}
}

// textDiff returns git's own diff for a path no handler claims, capped in lines
// — the unit a text diff has — and reported as explicitly as a change tree is.
func (s *server) textDiff(base, head forgerepo.Source, path string, max int) (string, truncation, error) {
	out, err := forgerepo.GitOutput(s.root, forgerepo.GitDiffArgs(base, head, nil, []string{path})...)
	if err != nil {
		return "", truncation{}, err
	}
	body := strings.TrimRight(string(out), "\n")
	if body == "" {
		return "", truncation{}, nil
	}
	lines := strings.Split(body, "\n")
	t := truncation{Truncated: len(lines) > max, Returned: len(lines), Total: len(lines)}
	if t.Truncated {
		t.Returned = max
		lines = lines[:max]
		t.Hint = fmt.Sprintf("%d of %d diff lines returned; raise max_changes for more. Text diffs have no subtree to drill into.", max, t.Total)
	}
	return strings.Join(lines, "\n"), t, nil
}
