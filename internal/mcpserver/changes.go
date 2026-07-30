package mcpserver

import (
	"fmt"
	"strings"

	"github.com/forgehubproject/forge/internal/handler"
)

// changeNode is one change out of a handler's tree, addressed by its
// fully-qualified path.
//
// The tree is delivered as a flat depth-first sequence rather than nested JSON
// for two reasons: a self-referential Go type has no JSON schema the SDK can
// infer, and flattening makes the cap countable — one node is one change, so
// returned and total mean the same thing at every depth. depth and parent
// rebuild the nesting exactly, and childrenReturned says where a subtree was
// cut instead of leaving it to be inferred.
type changeNode struct {
	Path             string `json:"path" jsonschema:"fully-qualified semantic address of this change; pass it back as \"at\" to get its subtree"`
	Kind             string `json:"kind" jsonschema:"added, removed or modified"`
	Label            string `json:"label,omitempty" jsonschema:"the handler's display name for the change, where it gives one"`
	Depth            int    `json:"depth" jsonschema:"0 for a root of this response, 1 for its children, and so on"`
	Parent           string `json:"parent,omitempty" jsonschema:"path of the change this one hangs under; absent at depth 0"`
	Before           any    `json:"before,omitempty" jsonschema:"the value on the base side; absent for an addition"`
	After            any    `json:"after,omitempty" jsonschema:"the value on the head side; absent for a removal"`
	ChildCount       int    `json:"childCount" jsonschema:"how many children this change has in the handler's tree"`
	ChildrenReturned int    `json:"childrenReturned" jsonschema:"how many of those children this response carries; fewer than childCount means the rest are reachable with at=path"`
}

// diffSummary is what a caller reads before deciding what to look at: how much
// changed, of what kinds, and which roots hold it.
type diffSummary struct {
	Total            int            `json:"total" jsonschema:"changes in the tree this response describes, counted at every depth"`
	ByKind           map[string]int `json:"byKind" jsonschema:"how many of those changes were additions, removals and modifications"`
	TopLevel         []topLevel     `json:"topLevel" jsonschema:"the roots of the tree, each with the number of children under it"`
	TopLevelWithheld int            `json:"topLevelWithheld,omitempty" jsonschema:"roots the list above omits, if any; raise max_changes or narrow with kinds to reach them"`
}

// topLevel is one root of a change tree, named so a caller can drill into it.
type topLevel struct {
	Path       string `json:"path" jsonschema:"pass this back as \"at\" to get this root's subtree alone"`
	Label      string `json:"label,omitempty" jsonschema:"the handler's display name for the root, where it gives one"`
	Kind       string `json:"kind" jsonschema:"added, removed or modified"`
	ChildCount int    `json:"childCount" jsonschema:"changes directly under this root"`
}

// truncation is the part of every capped response that makes the cap visible.
// Silence here would let a caller read a partial change set as a complete one —
// the failure the whole shape exists to prevent.
type truncation struct {
	Truncated bool   `json:"truncated" jsonschema:"true when this response carries less than everything that matched"`
	Returned  int    `json:"returned" jsonschema:"how much this response carries"`
	Total     int    `json:"total" jsonschema:"how much matched in total"`
	Hint      string `json:"hint,omitempty" jsonschema:"how to reach what was withheld, naming the paths to pass as \"at\""`
}

// hintPathLimit caps how many drill-down paths a hint names. The hint exists to
// give a caller its next move, not to become the payload it was meant to avoid.
const hintPathLimit = 8

// qualify returns a change's fully-qualified path. Handlers are expected to emit
// child paths already qualified (docs/structured-diff.md), but one that emits a
// bare field name gets its parent prepended rather than being handed back as a
// root-looking address that "at" could never match.
func qualify(parent, path string) string {
	switch {
	case parent == "" || path == "":
		return path
	case strings.HasPrefix(path, parent):
		return path
	default:
		return parent + "." + path
	}
}

// subtreeAt returns the changes at one fully-qualified path — the drill-down
// "at" asks for. Each match comes back with its own path rewritten to that
// qualified address, so every path in the response means what it meant in the
// response the caller took "at" from.
func subtreeAt(changes []handler.DiffChange, parent, at string) []handler.DiffChange {
	var found []handler.DiffChange
	for _, c := range changes {
		q := qualify(parent, c.Path)
		if q == at {
			c.Path = q
			found = append(found, c)
			continue
		}
		found = append(found, subtreeAt(c.Children, q, at)...)
	}
	return found
}

// filterKinds keeps the changes of the kinds asked for, and the containers on
// the way down to them: a removal three levels deep is only reachable if its
// parents survive the filter. A container whose own kind matches is kept whole,
// since that is the change being reported.
func filterKinds(changes []handler.DiffChange, want map[string]bool) []handler.DiffChange {
	var out []handler.DiffChange
	for _, c := range changes {
		if want[strings.ToLower(string(c.Kind))] {
			out = append(out, c)
			continue
		}
		if kids := filterKinds(c.Children, want); len(kids) > 0 {
			c.Children = kids
			out = append(out, c)
		}
	}
	return out
}

// countChanges counts every node in a tree, at every depth — the number
// truncation reports as the total.
func countChanges(changes []handler.DiffChange) int {
	n := 0
	for _, c := range changes {
		n += 1 + countChanges(c.Children)
	}
	return n
}

// countByKind counts every node in a tree by kind.
func countByKind(changes []handler.DiffChange) map[string]int {
	out := map[string]int{}
	var walk func([]handler.DiffChange)
	walk = func(cs []handler.DiffChange) {
		for _, c := range cs {
			kind := string(c.Kind)
			if kind == "" {
				kind = "unspecified"
			}
			out[kind]++
			walk(c.Children)
		}
	}
	walk(changes)
	return out
}

// flatten walks a tree depth-first, emitting at most budget nodes and recording
// on each node how many of its children the response carries — so a cut is
// visible at the exact place it happened rather than only in a total. It returns
// how many nodes it emitted at this level, which is its parent's
// childrenReturned.
func flatten(changes []handler.DiffChange, parent string, depth int, budget *int, out *[]changeNode) int {
	emitted := 0
	for _, c := range changes {
		if *budget <= 0 {
			break
		}
		path := qualify(parent, c.Path)
		*budget--
		emitted++
		at := len(*out)
		*out = append(*out, changeNode{
			Path:       path,
			Kind:       string(c.Kind),
			Label:      c.Label,
			Depth:      depth,
			Parent:     parent,
			Before:     c.Before,
			After:      c.After,
			ChildCount: len(c.Children),
		})
		(*out)[at].ChildrenReturned = flatten(c.Children, path, depth+1, budget, out)
	}
	return emitted
}

// summarize describes a whole tree, capping the list of roots at the same budget
// the tree itself gets and saying how many roots that left out.
func summarize(changes []handler.DiffChange, max int) diffSummary {
	// TopLevel is built empty rather than left nil: it is a required property of
	// the tool's output schema, and a nil slice would cross the wire as null.
	s := diffSummary{Total: countChanges(changes), ByKind: countByKind(changes), TopLevel: []topLevel{}}
	for i, c := range changes {
		if i >= max {
			s.TopLevelWithheld = len(changes) - i
			break
		}
		s.TopLevel = append(s.TopLevel, topLevel{
			Path:       qualify("", c.Path),
			Label:      c.Label,
			Kind:       string(c.Kind),
			ChildCount: len(c.Children),
		})
	}
	return s
}

// renderTree turns a filtered tree into the capped node sequence and the
// truncation record that describes it.
func renderTree(changes []handler.DiffChange, max int) ([]changeNode, truncation) {
	total := countChanges(changes)
	budget := max
	var nodes []changeNode
	flatten(changes, "", 0, &budget, &nodes)
	return nodes, truncationOf(nodes, len(nodes), total)
}

// truncationOf builds the truncation record for a rendered tree, naming the
// paths that drill into what was withheld.
func truncationOf(nodes []changeNode, returned, total int) truncation {
	t := truncation{Truncated: returned < total, Returned: returned, Total: total}
	if !t.Truncated {
		return t
	}

	var deeper []string
	for _, n := range nodes {
		if n.ChildrenReturned < n.ChildCount {
			deeper = append(deeper, n.Path)
		}
	}
	switch {
	case len(deeper) == 0:
		t.Hint = fmt.Sprintf("%d of %d changes returned; the rest are further roots — raise max_changes, or narrow with kinds.", returned, total)
	default:
		listed := deeper
		more := 0
		if len(listed) > hintPathLimit {
			more = len(listed) - hintPathLimit
			listed = listed[:hintPathLimit]
		}
		t.Hint = fmt.Sprintf("%d of %d changes returned; these paths hold changes this response withheld: %s",
			returned, total, strings.Join(listed, ", "))
		if more > 0 {
			t.Hint += fmt.Sprintf(" (and %d more)", more)
		}
		t.Hint += ". Call again with at=<one of those paths>, narrow with kinds, or raise max_changes."
	}
	return t
}

// capOf reads the caller's max_changes, treating anything at or below zero as
// "no preference" rather than "nothing", since a response of nothing answers no
// question.
func capOf(max int) int {
	if max <= 0 {
		return defaultMaxChanges
	}
	return max
}

// kindSet normalizes a kinds filter, and reports whether the caller gave one.
func kindSet(kinds []string) (map[string]bool, bool) {
	if len(kinds) == 0 {
		return nil, false
	}
	want := make(map[string]bool, len(kinds))
	for _, k := range kinds {
		if k = strings.ToLower(strings.TrimSpace(k)); k != "" {
			want[k] = true
		}
	}
	return want, len(want) > 0
}
