package mcpserver

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// defaultMaxCommits caps a history listing when the caller names no cap. It is
// far below defaultMaxChanges because a commit is not a change node: each one
// carries its own path list, and thirty of those is already the largest answer
// an agent reads in one turn.
const defaultMaxCommits = 30

// logFormat is one commit record, NUL-separated and led by a NUL of its own.
// That leading separator is what makes the records parseable at all: with
// --name-only the paths follow the format block as further NUL-separated fields,
// so something has to say where one commit ends and the next begins, and an empty
// field is the one thing a path can never be. Reading a 40-character field as a
// sha instead would mistake a file named like one for a new record.
const logFormat = "%x00%H%x00%P%x00%an <%ae>%x00%aI%x00%s"

type logIn struct {
	Ref        string `json:"ref,omitempty" jsonschema:"the revision to walk back from — anything git resolves (sha, branch, tag, HEAD~2); defaults to HEAD"`
	Path       string `json:"path,omitempty" jsonschema:"list only the commits that changed this path, as git simplifies history for one"`
	MaxCommits int    `json:"max_commits,omitempty" jsonschema:"most commits to return; defaults to 30"`
}

type logOut struct {
	Ref       string      `json:"ref" jsonschema:"the revision that was walked, as the caller named it"`
	Commit    string      `json:"commit" jsonschema:"the commit that revision resolved to, so a ref that moves between two pages cannot make them disagree about what was walked"`
	Commits   []logCommit `json:"commits" jsonschema:"newest first, in git's own order"`
	Truncated truncation  `json:"truncated" jsonschema:"how much of this history the response carries"`
	Note      string      `json:"note,omitempty"`
}

// logCommit is one commit as navigation: enough to decide whether to ask about
// it, and the marking that says which of it forge can answer semantically.
type logCommit struct {
	SHA          string        `json:"sha"`
	Subject      string        `json:"subject"`
	Author       string        `json:"author"`
	Date         string        `json:"date" jsonschema:"author date, ISO 8601"`
	Parents      []string      `json:"parents" jsonschema:"every parent, first parent first; more than one is a merge"`
	Paths        []string      `json:"paths" jsonschema:"the files this commit changed against its first parent; empty for a merge, which git lists no files for"`
	HandledPaths []handledPath `json:"handledPaths" jsonschema:"the subset of paths a handler claims in this repository — the ones forge_show and forge_semantic_diff can answer semantically for this commit"`
}

// handledPath is one path a handler claims, carrying the handler that claims it
// and the build installed here, as every semantic payload this server returns
// does.
type handledPath struct {
	Path         string `json:"path"`
	HandlerID    string `json:"handlerId" jsonschema:"the handler that claims this path here"`
	HandlerBuild string `json:"handlerBuild,omitempty" jsonschema:"build of the installed handler that would answer for it"`
}

// log walks history as navigation rather than as content: what exists to ask
// about, and which of it forge has a semantic answer for. Every revision
// parameter this server takes assumes the caller already knows what exists, and
// in a client with no shell there is nothing else here that says.
//
// The listing is one git invocation for any number of commits. Asking git once
// per commit is what makes a history tool slow enough to be unusable, and the
// marking below needs no second read: it is the same registry resolution
// forge_status and forge_show use, so a path this reports as handled is one the
// semantic tools will really answer for.
func (s *server) log(ctx context.Context, _ *mcp.CallToolRequest, in logIn) (*mcp.CallToolResult, logOut, error) {
	out := logOut{Ref: strings.TrimSpace(in.Ref), Commits: []logCommit{}}
	if out.Ref == "" {
		out.Ref = "HEAD"
	}
	commit, err := forgerepo.ResolveRev(ctx, s.root, out.Ref)
	if err != nil {
		return nil, out, err
	}
	out.Commit = commit

	var pathspecs []string
	if in.Path != "" {
		path, err := s.resolve(in.Path)
		if err != nil {
			return nil, out, err
		}
		pathspecs = []string{path}
	}

	// The count is asked for separately because the listing is capped: a page of
	// thirty says nothing about whether there are thirty-one, and truncation here
	// has to be as explicit as it is everywhere else. It is one call for the whole
	// response, not one per commit.
	total, err := s.commitCount(ctx, commit, pathspecs)
	if err != nil {
		return nil, out, err
	}

	max := in.MaxCommits
	if max <= 0 {
		max = defaultMaxCommits
	}
	args := []string{"log", "-z", "--name-only", "--format=" + logFormat, "--max-count=" + strconv.Itoa(max), commit}
	if len(pathspecs) > 0 {
		args = append(append(args, "--"), pathspecs...)
	}
	raw, err := forgerepo.GitOutput(ctx, s.root, args...)
	if err != nil {
		return nil, out, err
	}

	reg := forgerepo.Registry(ctx, s.root)
	builds := map[string]string{}
	out.Commits = parseLogRecords(string(raw))
	merges := false
	for i := range out.Commits {
		c := &out.Commits[i]
		merges = merges || (len(c.Parents) > 1 && len(c.Paths) == 0)
		for _, p := range c.Paths {
			id := handlerID(reg, p)
			if id == "" {
				continue
			}
			build, known := builds[id]
			if !known {
				build = fhr.InstalledHandlerBuild(id)
				builds[id] = build
			}
			c.HandledPaths = append(c.HandledPaths, handledPath{Path: p, HandlerID: id, HandlerBuild: build})
		}
	}

	out.Truncated = truncation{Truncated: total > len(out.Commits), Returned: len(out.Commits), Total: total}
	if out.Truncated.Truncated {
		out.Truncated.Hint = s.logHint(out.Truncated, out.Commits[len(out.Commits)-1])
	}
	switch {
	case len(out.Commits) > 0 && merges:
		out.Note = "a merge commit is listed with no paths: git reports the files of a commit against its first parent, and for a merge that is nothing. forge_show reads one against its first parent the same way."
	case len(out.Commits) > 0:
	case len(pathspecs) > 0:
		out.Note = fmt.Sprintf("no commit reachable from %s changed %s", out.Ref, pathspecs[0])
	default:
		out.Note = fmt.Sprintf("no commit is reachable from %s", out.Ref)
	}
	return nil, out, nil
}

// logHint names the cursor that continues a capped listing. It is the first
// parent of the last commit returned, which is where this page stopped — so the
// next call resumes from the commit after it rather than from a count the caller
// has to keep. A last commit with no parent is a root, and nothing continues from
// one, so the hint says that instead of naming a revision that cannot resolve.
func (s *server) logHint(t truncation, last logCommit) string {
	if len(last.Parents) == 0 {
		return fmt.Sprintf("%d of %d commits returned, and the last of them is a root commit — nothing continues from it, so raise max_commits to reach the rest of this history.",
			t.Returned, t.Total)
	}
	return fmt.Sprintf("%d of %d commits returned; call again with ref=%q — the first parent of the last commit here — for the page that continues from it, or raise max_commits.",
		t.Returned, t.Total, last.SHA+"^")
}

// commitCount is how many commits the listing would have returned uncapped, so
// truncation can report a total rather than a page.
func (s *server) commitCount(ctx context.Context, commit string, pathspecs []string) (int, error) {
	args := []string{"rev-list", "--count", commit}
	if len(pathspecs) > 0 {
		args = append(append(args, "--"), pathspecs...)
	}
	out, err := forgerepo.GitOutput(ctx, s.root, args...)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(out)))
}

// parseLogRecords reads the records logFormat produces. Each begins with an
// empty field; the five that follow are positional, so a root commit's empty
// parent list and an empty subject are read as themselves rather than as the
// start of the next commit. Everything after them, up to the next empty field, is
// a path — the first carrying the newline git puts between a commit and its file
// list.
func parseLogRecords(raw string) []logCommit {
	fields := strings.Split(raw, "\x00")
	commits := []logCommit{}
	for i := 0; i < len(fields); {
		if fields[i] != "" {
			i++
			continue
		}
		if i+5 >= len(fields) {
			break
		}
		c := logCommit{
			SHA:          fields[i+1],
			Parents:      strings.Fields(fields[i+2]),
			Author:       fields[i+3],
			Date:         fields[i+4],
			Subject:      fields[i+5],
			Paths:        []string{},
			HandledPaths: []handledPath{},
		}
		if c.Parents == nil {
			// A required property of the output schema: nil would cross the wire as
			// null where the schema promises an array.
			c.Parents = []string{}
		}
		i += 6
		for ; i < len(fields) && fields[i] != ""; i++ {
			c.Paths = append(c.Paths, strings.TrimPrefix(fields[i], "\n"))
		}
		commits = append(commits, c)
	}
	return commits
}
