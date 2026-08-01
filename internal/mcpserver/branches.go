package mcpserver

import (
	"context"
	"fmt"
	"strings"

	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// refFormat is one ref record: NUL-separated fields, one record per line. A ref
// name cannot hold a newline and a subject is one line by construction, so the
// two separators can never be confused for content.
//
// The starred fields are what an annotated tag needs: %(objectname) there is the
// tag object, and the commit a caller would pass to a revision parameter is
// %(*objectname). A branch leaves them empty.
const refFormat = "%(refname)%00%(refname:short)%00%(objectname)%00%(*objectname)%00%(contents:subject)%00%(*contents:subject)%00%(HEAD)"

type branchesIn struct {
	Tags    bool   `json:"tags,omitempty" jsonschema:"include tags"`
	Remotes bool   `json:"remotes,omitempty" jsonschema:"include remote-tracking refs — what the last fetch left on this machine; nothing here reaches the network"`
	After   string `json:"after,omitempty" jsonschema:"a full ref name from a previous response's ref field — the listing resumes after it, which is how a repository with more refs than the cap is walked"`
	MaxRefs int    `json:"max_refs,omitempty" jsonschema:"most refs to return across every list in the response; defaults to 200"`
}

type branchesOut struct {
	Current   string     `json:"current,omitempty" jsonschema:"the branch checked out, absent when HEAD is detached or the repository has no commits yet"`
	Detached  bool       `json:"detached" jsonschema:"true when HEAD names a commit rather than a branch"`
	Head      string     `json:"head,omitempty" jsonschema:"commit HEAD points at, absent in a repository with no commits"`
	Branches  []refEntry `json:"branches" jsonschema:"local branches, ordered by ref name"`
	Tags      []refEntry `json:"tags" jsonschema:"tags, ordered by ref name; empty unless tags was asked for"`
	Remotes   []refEntry `json:"remotes" jsonschema:"remote-tracking refs, ordered by ref name; empty unless remotes was asked for"`
	Truncated truncation `json:"truncated" jsonschema:"how many of the refs that matched this response carries"`
	Note      string     `json:"note,omitempty"`
}

// refEntry is one ref as a revision an agent can pass on: the name a revision
// parameter takes, the commit behind it, and enough of that commit to tell it
// apart from the others.
type refEntry struct {
	Name    string `json:"name" jsonschema:"the short name, which is what a revision parameter takes"`
	Ref     string `json:"ref" jsonschema:"the full ref name — what \"after\" continues from"`
	SHA     string `json:"sha" jsonschema:"the commit at the tip; for an annotated tag, the commit it points at rather than the tag object"`
	Subject string `json:"subject" jsonschema:"subject of that commit"`
	Current bool   `json:"current" jsonschema:"true for the branch HEAD is on"`
}

// branches lists what this repository has to offer a revision parameter. It is
// the other half of the gap forge_log fills: every revision this server takes
// assumes the caller knows what exists, and nothing else here enumerates it.
//
// Remote-tracking refs are a local read. They say what the last fetch left, not
// what the remote holds now, and nothing here goes and finds out — this server
// reaches the network for exactly one thing, and it is not this.
func (s *server) branches(ctx context.Context, _ *mcp.CallToolRequest, in branchesIn) (*mcp.CallToolResult, branchesOut, error) {
	out := branchesOut{Branches: []refEntry{}, Tags: []refEntry{}, Remotes: []refEntry{}}

	if head, err := forgerepo.GitOutput(ctx, s.root, "rev-parse", "-q", "--verify", "HEAD"); err == nil {
		out.Head = strings.TrimSpace(string(head))
	}
	branch, _ := forgerepo.GitOutput(ctx, s.root, "symbolic-ref", "--short", "-q", "HEAD")
	unborn := strings.TrimSpace(string(branch))
	if out.Head != "" {
		out.Current = unborn
	}
	out.Detached = out.Current == "" && out.Head != ""

	patterns := []string{"refs/heads"}
	if in.Tags {
		patterns = append(patterns, "refs/tags")
	}
	if in.Remotes {
		patterns = append(patterns, "refs/remotes")
	}
	// One invocation, sorted by ref name, so two calls with the same arguments
	// return the same sequence and "after" means the same thing in both.
	args := append([]string{"for-each-ref", "--sort=refname", "--format=" + refFormat}, patterns...)
	raw, err := forgerepo.GitOutput(ctx, s.root, args...)
	if err != nil {
		return nil, out, err
	}
	refs := parseRefRecords(string(raw))

	if in.After != "" {
		rest, found := filesAfter(refNames(refs), in.After)
		if !found {
			out.Note = fmt.Sprintf("%q is not a ref this call lists, so there is nothing to continue from — pass a ref from a previous response's ref field, with the same tags and remotes arguments", in.After)
			return nil, out, nil
		}
		refs = refs[len(refs)-len(rest):]
	}

	max := capOf(in.MaxRefs)
	out.Truncated = truncation{Truncated: len(refs) > max, Returned: len(refs), Total: len(refs)}
	if out.Truncated.Truncated {
		out.Truncated.Returned = max
		refs = refs[:max]
		out.Truncated.Hint = fmt.Sprintf("%d of %d refs returned; call again with after=%q for the next page, or raise max_refs.",
			max, out.Truncated.Total, refs[len(refs)-1].Ref)
	}

	for _, r := range refs {
		switch {
		case strings.HasPrefix(r.Ref, "refs/tags/"):
			out.Tags = append(out.Tags, r)
		case strings.HasPrefix(r.Ref, "refs/remotes/"):
			out.Remotes = append(out.Remotes, r)
		default:
			out.Branches = append(out.Branches, r)
		}
	}
	switch {
	case len(refs) > 0:
	case out.Head == "" && unborn != "":
		out.Note = fmt.Sprintf("this repository has no commits yet, so it has no branches: HEAD points at %s, which will exist once something is committed", unborn)
	case in.After != "":
		out.Note = fmt.Sprintf("%q is the last ref this call lists, so nothing follows it", in.After)
	default:
		out.Note = "this repository has no refs of the kinds asked for"
	}
	return nil, out, nil
}

// parseRefRecords reads what refFormat produced, preferring the dereferenced
// commit of an annotated tag over the tag object: the sha reported here is one a
// revision parameter can be handed, and a tag object is not a commit.
func parseRefRecords(raw string) []refEntry {
	var refs []refEntry
	for _, record := range strings.Split(raw, "\n") {
		fields := strings.Split(record, "\x00")
		if len(fields) < 7 || fields[0] == "" {
			continue
		}
		entry := refEntry{Name: fields[1], Ref: fields[0], SHA: fields[2], Subject: fields[4], Current: fields[6] == "*"}
		if fields[3] != "" {
			entry.SHA, entry.Subject = fields[3], fields[5]
		}
		refs = append(refs, entry)
	}
	return refs
}

// refNames is the ref sequence as the plain list "after" is resolved against.
func refNames(refs []refEntry) []string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.Ref)
	}
	return names
}
