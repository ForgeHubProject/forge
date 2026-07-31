package mcpserver

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mergeOursSide is the shell a fixture handler uses to answer a merge the way a
// real one does: the side it was handed as ours is what its result holds, named
// in the blob so a test can tell which of the two it was given. That is the
// property every resolution here is built on — a merge keeps ours wherever it
// could not reconcile — and the only way to assert it is a handler whose output
// says which side that was.
const mergeOursSide = `ours=$(sed 's/.*"ours":"\([^"]*\)".*/\1/' | base64 -d)`

// mergeHandlerScript is a handler for ".merge" implementing the protocol's
// match/diff/merge and nothing else — there is nothing else for a handler binary
// to implement. Its merge reports a fixed pair of conflicts. The blobs it is
// handed are otherwise ignored: what is being exercised is the loop, not any
// format.
const mergeHandlerScript = `#!/bin/sh
case "$1" in
info)
  printf '%s\n' '{"id":"unit-merge","version":"1.0.0","protocol":"1.0","formats":[".merge"],"capabilities":{"semanticCompare":true,"semanticMerge":true}}'
  ;;
diff)
  cat >/dev/null
  printf '%s\n' '{"version":"1.0","format":"unit-merge","changes":[]}'
  ;;
merge)
  ` + mergeOursSide + `
  printf '{"blob":"%s","conflicts":[{"path":"alpha","ours":"a-ours","theirs":"a-theirs"},{"path":"beta","ours":"b-ours","theirs":"b-theirs"}]}\n' "$(printf 'merged:%s' "$ours" | base64)"
  ;;
*)
  echo "unknown subcommand: $1" >&2
  exit 1
  ;;
esac
`

// mergeOnlyHandlerScript is a handler for ".mergeonly" implementing merge and
// nothing else — no info, no diff. Every side of a conflict has to be reachable
// against it, because merge is the only call the protocol has that decides one.
const mergeOnlyHandlerScript = `#!/bin/sh
case "$1" in
merge)
  ` + mergeOursSide + `
  printf '{"blob":"%s","conflicts":[{"path":"gamma","ours":"g-ours","theirs":"g-theirs"}]}\n' "$(printf 'merged:%s' "$ours" | base64)"
  ;;
*)
  echo "unimplemented" >&2
  exit 1
  ;;
esac
`

// lopsidedHandlerScript is a handler for ".lopsided" that reports a conflict
// merging one way and none the other. Taking theirs is the merge run from the
// other side, so a handler that answers a different question there cannot be
// read as having decided the conflict that was named.
const lopsidedHandlerScript = `#!/bin/sh
case "$1" in
merge)
  ` + mergeOursSide + `
  if [ "$ours" = "OURS" ]; then
    printf '{"blob":"%s","conflicts":[{"path":"delta","ours":"d-ours","theirs":"d-theirs"}]}\n' "$(printf 'merged:%s' "$ours" | base64)"
  else
    printf '{"blob":"%s"}\n' "$(printf 'merged:%s' "$ours" | base64)"
  fi
  ;;
*)
  echo "unimplemented" >&2
  exit 1
  ;;
esac
`

const mergeBuild = "20240220-def5678"

// gitAllowT runs git where the command is expected to fail — the merge that
// stops on a conflict is the fixture, not an error.
func gitAllowT(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	_ = c.Run()
}

// newMergeRepo builds a repository stopped in the middle of a merge, with four
// unmerged paths: one whose handler implements the whole protocol, one whose
// handler implements only merge, one whose handler answers a different question
// with the sides exchanged, and one no handler claims at all.
func newMergeRepo(t *testing.T) *server {
	t.Helper()
	root := newRepo(t)

	plugins := filepath.Join(os.Getenv("HOME"), ".forge", "plugins")
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-merge"), mergeHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-merge.json"),
		`{"id":"unit-merge","build":"`+mergeBuild+`","source":"https://example.invalid/manifest.toml","formats":[".merge"]}`, 0644)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-mergeonly"), mergeOnlyHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-mergeonly.json"),
		`{"id":"unit-mergeonly","build":"nobuild","source":"https://example.invalid/manifest.toml","formats":[".mergeonly"]}`, 0644)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-lopsided"), lopsidedHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-lopsided.json"),
		`{"id":"unit-lopsided","build":"nobuild","source":"https://example.invalid/manifest.toml","formats":[".lopsided"]}`, 0644)
	writeFileT(t, filepath.Join(root, ".forge", "formats"), ".unit\n.merge\n.mergeonly\n.lopsided\n!.ignored\n", 0644)

	// Start from a clean tree so the only changes under test are the ones below.
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-m", "fixture base")

	for _, side := range []struct{ branch, content string }{{"incoming", "THEIRS"}, {"main", "OURS"}} {
		if side.branch == "incoming" {
			gitT(t, root, "checkout", "-q", "-b", "incoming")
		} else {
			gitT(t, root, "checkout", "-q", "main")
		}
		writeFileT(t, filepath.Join(root, "asset.merge"), side.content, 0644)
		writeFileT(t, filepath.Join(root, "minimal.mergeonly"), side.content, 0644)
		writeFileT(t, filepath.Join(root, "tilted.lopsided"), side.content, 0644)
		writeFileT(t, filepath.Join(root, "plain.txt"), side.content+"\n", 0644)
		gitT(t, root, "add", "-A")
		gitT(t, root, "commit", "-m", side.branch)
	}

	gitAllowT(t, root, "merge", "--no-edit", "incoming")
	return &server{root: root}
}

func TestConflictsReportsWhatTheHandlerCouldNotReconcile(t *testing.T) {
	s := newMergeRepo(t)

	_, out, err := s.conflicts(context.Background(), nil, conflictsIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Merging || out.MergeHead == "" {
		t.Fatalf("a stopped merge must be reported as one: %+v", out)
	}
	files := map[string]conflictFile{}
	for _, f := range out.Files {
		files[f.Path] = f
	}
	if len(files) != 4 {
		t.Fatalf("expected four unmerged paths, got %d: %+v", len(files), out.Files)
	}

	asset := files["asset.merge"]
	if asset.HandlerID == nil || *asset.HandlerID != "unit-merge" || asset.HandlerBuild != mergeBuild {
		t.Fatalf("every semantic payload carries handler and build: %+v", asset)
	}
	if asset.Total != 2 || len(asset.Conflicts) != 2 {
		t.Fatalf("expected the handler's two conflicts: %+v", asset)
	}
	if asset.Conflicts[0].Path != "alpha" || asset.Conflicts[0].Ours != "a-ours" || asset.Conflicts[0].Theirs != "a-theirs" {
		t.Fatalf("conflict shape wrong: %+v", asset.Conflicts[0])
	}
	if asset.Truncated != nil {
		t.Fatalf("an uncapped file must not report truncation: %+v", asset.Truncated)
	}

	// A path no handler claims is listed rather than hidden: the caller still has
	// to resolve it, and what it needs to know is that forge cannot help.
	plain := files["plain.txt"]
	if plain.HandlerID != nil || len(plain.Conflicts) != 0 || !strings.Contains(plain.Note, "no handler claims") {
		t.Fatalf("plain.txt entry wrong: %+v", plain)
	}

	// Reading conflicts writes nothing, so the merge is exactly where it was.
	if !s.merging(context.Background()) {
		t.Fatal("forge_conflicts must not disturb the merge it reports on")
	}
}

func TestConflictsTruncationIsExplicit(t *testing.T) {
	s := newMergeRepo(t)
	ctx := context.Background()

	_, out, err := s.conflicts(ctx, nil, conflictsIn{MaxConflicts: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Truncated.Truncated || out.Truncated.Returned != 1 || out.Truncated.Total != 4 {
		t.Fatalf("a cap must report itself with true totals: %+v", out.Truncated)
	}
	if !strings.Contains(out.Truncated.Hint, `after="asset.merge"`) {
		t.Fatalf("the hint must name the cursor that continues the listing: %q", out.Truncated.Hint)
	}

	_, rest, err := s.conflicts(ctx, nil, conflictsIn{After: "asset.merge"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Files) != 3 || rest.Files[0].Path != "minimal.mergeonly" {
		t.Fatalf("after must continue the listing: %+v", rest.Files)
	}

	// A file's own conflicts are capped by its share of the response, and a file
	// that was cut says so with the call that gives it the whole cap.
	_, one, err := s.conflicts(ctx, nil, conflictsIn{Path: "asset.merge", MaxConflicts: 1})
	if err != nil {
		t.Fatal(err)
	}
	entry := one.Files[0]
	if entry.Truncated == nil || entry.Total != 2 || len(entry.Conflicts) != 1 {
		t.Fatalf("a cut conflict list must say so: %+v", entry)
	}
	if !strings.Contains(entry.Truncated.Hint, `path="asset.merge"`) {
		t.Fatalf("the hint must name a call that reaches the rest: %q", entry.Truncated.Hint)
	}
}

// Both sides are reachable against a handler binary, and neither needs a call
// the subprocess protocol does not have: ours is the merge as it stands, theirs
// is the same merge run from the other side. The fixture handler names the side
// it was handed as ours in its result, so these assertions are about which merge
// was actually run and not merely that a file was written.
func TestResolveConflictWritesTheSideThatWasChosen(t *testing.T) {
	s := newMergeRepo(t)
	ctx := context.Background()

	_, out, err := s.resolveConflict(ctx, nil, resolveConflictIn{
		Path: "asset.merge",
		Choices: []conflictChoice{
			{Path: "alpha", Take: "theirs"},
			{Path: "beta", Take: "theirs"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.HandlerID != "unit-merge" || out.HandlerBuild != mergeBuild {
		t.Fatalf("every semantic payload carries handler and build: %+v", out)
	}
	if got := readT(t, s.root, "asset.merge"); got != "merged:THEIRS" {
		t.Fatalf("working tree holds %q, want the merge run from the side being merged in", got)
	}
	if len(out.Applied) != 2 || out.Applied[0].Take != "theirs" || out.Applied[1].Take != "theirs" {
		t.Fatalf("the response must echo what was applied: %+v", out.Applied)
	}

	// Nothing is staged, which is what leaves the pre-image recoverable — the
	// index still holds all three sides — and what makes the call repeatable.
	if out.Staged {
		t.Fatal("forge_resolve_conflict must not stage")
	}
	unmerged, err := s.unmergedPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(unmerged, "asset.merge") {
		t.Fatal("the index must still hold the conflicted stages until the file is staged")
	}

	// Re-decidable: the other side overwrites the first answer rather than
	// compounding it, and it is the merge unchanged.
	if _, _, err := s.resolveConflict(ctx, nil, resolveConflictIn{Path: "asset.merge", Choices: []conflictChoice{
		{Path: "alpha", Take: "ours"}, {Path: "beta", Take: "ours"},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := readT(t, s.root, "asset.merge"); got != "merged:OURS" {
		t.Fatalf("an all-ours resolution is the merged blob unchanged, got %q", got)
	}

	// A handler that implements merge and nothing else answers for both sides,
	// which is the whole point of building them out of merge: every handler in
	// the ecosystem has that call and none has another.
	for _, take := range []string{"ours", "theirs"} {
		if _, _, err := s.resolveConflict(ctx, nil, resolveConflictIn{
			Path: "minimal.mergeonly", Choices: []conflictChoice{{Path: "gamma", Take: take}},
		}); err != nil {
			t.Fatalf("taking %q from a merge-only handler: %v", take, err)
		}
		if got, want := readT(t, s.root, "minimal.mergeonly"), "merged:"+strings.ToUpper(take); got != want {
			t.Fatalf("taking %q wrote %q, want %q", take, got, want)
		}
	}
}

// A file needing one unit from each side cannot be built out of merge, and no
// handler binary offers anything else. It is refused whole rather than written
// half-right, and the refusal says what is available instead.
func TestResolveConflictRefusesAMixOfSides(t *testing.T) {
	s := newMergeRepo(t)
	ctx := context.Background()

	before := readT(t, s.root, "asset.merge")
	_, _, err := s.resolveConflict(ctx, nil, resolveConflictIn{
		Path: "asset.merge",
		Choices: []conflictChoice{
			{Path: "alpha", Take: "theirs"},
			{Path: "beta", Take: "ours"},
		},
	})
	if err == nil {
		t.Fatal("a mix of sides in one file must be refused")
	}
	for _, want := range []string{"one side per file", "1 of this file's 2 conflicts"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the refusal must say %q: %v", want, err)
		}
	}
	if got := readT(t, s.root, "asset.merge"); got != before {
		t.Fatalf("a refused resolution must write nothing: %q", got)
	}
}

// Taking theirs is the merge run from the other side. A handler that reports
// different conflicts there was asked a different question, so its answer cannot
// be read as having decided the conflict that was named.
func TestResolveConflictRefusesAHandlerThatAnswersADifferentQuestion(t *testing.T) {
	s := newMergeRepo(t)
	ctx := context.Background()

	// Ours needs only the merge that was already run, so it is unaffected.
	if _, _, err := s.resolveConflict(ctx, nil, resolveConflictIn{
		Path: "tilted.lopsided", Choices: []conflictChoice{{Path: "delta", Take: "ours"}},
	}); err != nil {
		t.Fatalf("ours needs only the merge that was already run: %v", err)
	}
	before := readT(t, s.root, "tilted.lopsided")
	if before != "merged:OURS" {
		t.Fatalf("tilted.lopsided holds %q", before)
	}

	_, _, err := s.resolveConflict(ctx, nil, resolveConflictIn{
		Path: "tilted.lopsided", Choices: []conflictChoice{{Path: "delta", Take: "theirs"}},
	})
	if err == nil {
		t.Fatal("a handler that disagrees with itself must not have its result written as a resolution")
	}
	if !strings.Contains(err.Error(), "unit-lopsided") || !strings.Contains(err.Error(), "delta") {
		t.Fatalf("the refusal must name the handler and what it disagreed about: %v", err)
	}
	if got := readT(t, s.root, "tilted.lopsided"); got != before {
		t.Fatalf("a refused resolution must not overwrite what is there: %q", got)
	}
}

func TestResolveConflictRefusesWhatItCannotHonestlyDecide(t *testing.T) {
	s := newMergeRepo(t)
	ctx := context.Background()

	cases := []struct {
		name string
		in   resolveConflictIn
		want string
	}{
		{"a conflict left undecided", resolveConflictIn{Path: "asset.merge", Choices: []conflictChoice{{Path: "alpha", Take: "ours"}}}, "beta"},
		{"no choices at all", resolveConflictIn{Path: "asset.merge"}, "every conflict must be decided"},
		{"a conflict that does not exist", resolveConflictIn{Path: "asset.merge", Choices: []conflictChoice{{Path: "nope", Take: "ours"}}}, "no conflict at"},
		{"a side that is neither", resolveConflictIn{Path: "asset.merge", Choices: []conflictChoice{{Path: "alpha", Take: "mine"}, {Path: "beta", Take: "ours"}}}, `"ours" or "theirs"`},
		{"a path no handler claims", resolveConflictIn{Path: "plain.txt"}, "no handler claims"},
		{"a path that is not unmerged", resolveConflictIn{Path: "notes.txt"}, "not unmerged"},
		{"a path outside the repository", resolveConflictIn{Path: "../escape"}, "outside the repository"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, _, err := s.resolveConflict(ctx, nil, c.in)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("expected a refusal naming %q, got: %v", c.want, err)
			}
		})
	}
}

// The whole loop, in the order an agent runs it: read the conflicts, decide
// them, stage, commit.
func TestTheResolutionLoopCompletesAMerge(t *testing.T) {
	s := newMergeRepo(t)
	ctx := context.Background()

	_, listed, err := s.conflicts(ctx, nil, conflictsIn{Path: "asset.merge"})
	if err != nil {
		t.Fatal(err)
	}
	var choices []conflictChoice
	for _, c := range listed.Files[0].Conflicts {
		choices = append(choices, conflictChoice{Path: c.Path, Take: "theirs"})
	}
	if _, _, err := s.resolveConflict(ctx, nil, resolveConflictIn{Path: "asset.merge", Choices: choices}); err != nil {
		t.Fatal(err)
	}
	if got := readT(t, s.root, "asset.merge"); got != "merged:THEIRS" {
		t.Fatalf("every conflict taken from theirs is the merge from that side, got %q", got)
	}

	// The rest are resolved the way a caller without a handler has to: edit and
	// stage.
	for _, p := range []string{"plain.txt", "minimal.mergeonly", "tilted.lopsided"} {
		writeFileT(t, filepath.Join(s.root, p), "resolved\n", 0644)
	}
	if _, _, err := s.add(ctx, nil, addIn{Paths: []string{"asset.merge", "plain.txt", "minimal.mergeonly", "tilted.lopsided"}}); err != nil {
		t.Fatal(err)
	}
	left, err := s.unmergedPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(left) != 0 {
		t.Fatalf("staging a resolved file marks it resolved: %v", left)
	}

	_, done, err := s.commit(ctx, nil, commitIn{Message: "resolve the merge"})
	if err != nil {
		t.Fatal(err)
	}
	if done.SHA == "" || done.Subject != "resolve the merge" || done.Branch != "main" {
		t.Fatalf("commit = %+v", done)
	}
	if s.merging(ctx) {
		t.Fatal("committing every resolved path concludes the merge")
	}
}

func TestAddStagesPathsAndCannotBeTalkedIntoAFlag(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	// A file whose name begins with a dash is a path, not an option: git is told
	// so with "--", the way the reference server does it.
	writeFileT(t, filepath.Join(s.root, "-dashed.txt"), "x", 0644)
	_, out, err := s.add(ctx, nil, addIn{Paths: []string{"asset.unit", "-dashed.txt"}})
	if err != nil {
		t.Fatal(err)
	}
	staged := map[string]statusEntry{}
	for _, e := range out.Entries {
		staged[e.Path] = e
	}
	if e := staged["asset.unit"]; !e.Staged || e.HandlerID != "unit-stub" {
		t.Fatalf("asset.unit entry wrong: %+v", e)
	}
	if e, ok := staged["-dashed.txt"]; !ok || !e.Staged {
		t.Fatalf("a dash-leading path must stage as itself: %+v", out.Entries)
	}

	if _, _, err := s.add(ctx, nil, addIn{}); err == nil {
		t.Fatal("staging nothing answers no question and must be refused")
	}
	if _, _, err := s.add(ctx, nil, addIn{Paths: []string{"../outside"}}); err == nil {
		t.Fatal("a path outside the repository must be refused")
	}
}

func TestCommitRecordsOnlyWhatIsStaged(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	if _, _, err := s.commit(ctx, nil, commitIn{Message: "  "}); err == nil {
		t.Fatal("a commit needs a message")
	}
	if _, _, err := s.add(ctx, nil, addIn{Paths: []string{"asset.unit"}}); err != nil {
		t.Fatal(err)
	}
	_, out, err := s.commit(ctx, nil, commitIn{Message: "stage one file"})
	if err != nil {
		t.Fatal(err)
	}
	if out.SHA == "" || out.Subject != "stage one file" {
		t.Fatalf("commit = %+v", out)
	}

	// The untracked file was never staged, so the commit did not take it.
	_, status, err := s.status(ctx, nil, noArgs{})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range status.Entries {
		if e.Path == "untracked.txt" {
			found = e.State == "untracked"
		}
	}
	if !found {
		t.Fatalf("commit must not stage on the way: %+v", status.Entries)
	}
}

func TestCreateBranchCreatesWithoutSwitching(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	_, out, err := s.createBranch(ctx, nil, createBranchIn{Name: "feature", Base: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Name != "feature" || out.Base != "HEAD~1" || out.Commit == "" {
		t.Fatalf("createBranch = %+v", out)
	}
	_, status, err := s.status(ctx, nil, noArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if status.Branch != "main" {
		t.Fatalf("creating a branch must not check it out, HEAD is on %q", status.Branch)
	}
	if _, _, err := s.createBranch(ctx, nil, createBranchIn{Name: "feature"}); err == nil {
		t.Fatal("a branch that exists must not be moved by this tool")
	}
	if _, _, err := s.createBranch(ctx, nil, createBranchIn{Name: "later", Base: "no-such-revision"}); err == nil {
		t.Fatal("a base git cannot resolve must be refused")
	}
}

// git's own refusal is the protection, and it has to reach the caller intact —
// with the working tree exactly as it was.
func TestCheckoutRefusesRatherThanClobbering(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	// A branch whose version of the edited file differs, so checking it out would
	// have to overwrite work that is not committed.
	gitT(t, s.root, "branch", "other", "HEAD~1")
	dirty := readT(t, s.root, "asset.unit")

	_, _, err := s.checkout(ctx, nil, checkoutIn{Target: "other"})
	if err == nil {
		t.Fatal("checking out over an uncommitted change must fail")
	}
	if !strings.Contains(err.Error(), "asset.unit") {
		t.Fatalf("git's own refusal should reach the caller: %v", err)
	}
	if got := readT(t, s.root, "asset.unit"); got != dirty {
		t.Fatalf("the working tree changed under a refused checkout: %q", got)
	}
	_, status, err := s.status(ctx, nil, noArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if status.Branch != "main" {
		t.Fatalf("a refused checkout must leave HEAD alone, it is on %q", status.Branch)
	}

	// A target that names a file rather than a revision is `git checkout <path>`,
	// which restores it from the index and discards the edit. It is refused here.
	if _, _, err := s.checkout(ctx, nil, checkoutIn{Target: "asset.unit"}); err == nil {
		t.Fatal("a path target must be refused, not restored")
	}
	if got := readT(t, s.root, "asset.unit"); got != dirty {
		t.Fatalf("a refused checkout must not restore anything: %q", got)
	}
	if _, _, err := s.checkout(ctx, nil, checkoutIn{Target: "--force"}); err == nil {
		t.Fatal("a dash-leading target must be refused rather than passed to git as an option")
	}

	// With the change put away, the same checkout works.
	if _, _, err := s.add(ctx, nil, addIn{Paths: []string{"asset.unit"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.commit(ctx, nil, commitIn{Message: "keep it"}); err != nil {
		t.Fatal(err)
	}
	_, out, err := s.checkout(ctx, nil, checkoutIn{Target: "other"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Branch != "other" || out.Detached {
		t.Fatalf("checkout = %+v", out)
	}
}

func TestResetUnstagesAndLeavesTheWorkingTreeAlone(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	before := readT(t, s.root, "asset.unit")
	if _, _, err := s.add(ctx, nil, addIn{Paths: []string{"asset.unit", "untracked.txt"}}); err != nil {
		t.Fatal(err)
	}

	_, out, err := s.reset(ctx, nil, resetIn{Paths: []string{"asset.unit"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Unstaged) != 1 || out.Unstaged[0] != "asset.unit" {
		t.Fatalf("reset = %+v", out)
	}
	if got := readT(t, s.root, "asset.unit"); got != before {
		t.Fatalf("reset must never touch a file's contents: %q, want %q", got, before)
	}
	entries := map[string]statusEntry{}
	for _, e := range out.Entries {
		entries[e.Path] = e
	}
	if e := entries["asset.unit"]; e.Staged || !e.Unstaged {
		t.Fatalf("asset.unit should be unstaged and still changed: %+v", e)
	}

	// The whole index, with every file still exactly as it was.
	_, whole, err := s.reset(ctx, nil, resetIn{})
	if err != nil {
		t.Fatal(err)
	}
	if len(whole.Unstaged) != 0 {
		t.Fatalf("a whole-index reset names no paths: %+v", whole)
	}
	if got := readT(t, s.root, "asset.unit"); got != before {
		t.Fatalf("a whole-index reset must never touch a file's contents: %q", got)
	}
	if got := readT(t, s.root, "untracked.txt"); got != "u" {
		t.Fatalf("a file unstaged back to untracked must still be on disk: %q", got)
	}
}

// An unmerged path has nothing staged to take back, and a path-scoped reset of
// one replaces every side the index holds with HEAD's. The merge would carry on
// with that path looking settled, and the commit at the end of it would record a
// merge one of whose sides was never in it — silently, since nothing after the
// reset has anything left to report. It is refused instead.
func TestResetRefusesAnUnmergedPathRatherThanForgettingASide(t *testing.T) {
	s := newMergeRepo(t)
	ctx := context.Background()

	before := readT(t, s.root, "asset.merge")
	for _, in := range []resetIn{
		{Paths: []string{"asset.merge"}},
		{Paths: []string{"."}},
		{Paths: []string{"notes.txt", "plain.txt"}},
	} {
		_, _, err := s.reset(ctx, nil, in)
		if err == nil {
			t.Fatalf("reset %v during a merge must be refused", in.Paths)
		}
		if !strings.Contains(err.Error(), "unmerged") || !strings.Contains(err.Error(), "forge_resolve_conflict") {
			t.Fatalf("the refusal must say why and where to go instead: %v", err)
		}
	}

	// Refused whole: the index still holds every side, the merge is still in
	// progress, and the file is untouched.
	unmerged, err := s.unmergedPaths(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(unmerged) != 4 {
		t.Fatalf("a refused reset must leave the index as it was: %v", unmerged)
	}
	if !s.merging(ctx) {
		t.Fatal("a refused reset must leave the merge in progress")
	}
	if got := readT(t, s.root, "asset.merge"); got != before {
		t.Fatalf("reset must never touch a file's contents: %q", got)
	}

	// A path that is not unmerged is still resettable during a merge.
	writeFileT(t, filepath.Join(s.root, "notes.txt"), "edited\n", 0644)
	if _, _, err := s.add(ctx, nil, addIn{Paths: []string{"notes.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, out, err := s.reset(ctx, nil, resetIn{Paths: []string{"notes.txt"}}); err != nil {
		t.Fatalf("an ordinary staged path is not what this refusal is about: %v", err)
	} else if len(out.Unstaged) != 1 {
		t.Fatalf("reset = %+v", out)
	}

	// The whole-index reset during a merge is a different thing and stays
	// available: it ends the merge outright rather than leaving one behind that
	// looks finished, and the response says so.
	_, whole, err := s.reset(ctx, nil, resetIn{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(whole.Note, "cleared the merge in progress") {
		t.Fatalf("a whole-index reset that ended a merge must say so: %+v", whole)
	}
	if s.merging(ctx) {
		t.Fatal("a whole-index reset ends the merge")
	}
	if got := readT(t, s.root, "asset.merge"); got != before {
		t.Fatalf("no reset touches a file's contents: %q", got)
	}
}

// newPathspecMergeRepo stops a merge on two paths that no handler claims — one
// at the root, one under a directory — so what is unmerged is unmerged for git's
// own reasons and the pathspec reaching it is the only thing under test.
func newPathspecMergeRepo(t *testing.T) *server {
	t.Helper()
	root := newRepo(t)

	writeFileT(t, filepath.Join(root, "asset.dat"), "base\n", 0644)
	writeFileT(t, filepath.Join(root, "nested", "asset.dat"), "base\n", 0644)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-m", "fixture base")

	for _, side := range []struct{ branch, content string }{{"incoming", "THEIRS\n"}, {"main", "OURS\n"}} {
		if side.branch == "incoming" {
			gitT(t, root, "checkout", "-q", "-b", "incoming")
		} else {
			gitT(t, root, "checkout", "-q", "main")
		}
		writeFileT(t, filepath.Join(root, "asset.dat"), side.content, 0644)
		writeFileT(t, filepath.Join(root, "nested", "asset.dat"), side.content, 0644)
		gitT(t, root, "add", "-A")
		gitT(t, root, "commit", "-m", side.branch)
	}

	gitAllowT(t, root, "merge", "--no-edit", "incoming")
	return &server{root: root}
}

// A pathspec is git's language rather than a file name, and git reset acts on
// every form of it. Matching those forms here instead of asking git would leave
// a hole shaped like whichever one the matching did not implement — and what
// goes through that hole is a merge committed without the side it was merging
// in, reported by nothing, since after the reset there is no longer anything
// unmerged for forge_conflicts or forge_status to notice.
func TestResetRefusesAnUnmergedPathReachedByAnyPathspecGitAccepts(t *testing.T) {
	s := newPathspecMergeRepo(t)
	ctx := context.Background()

	for _, paths := range [][]string{
		{"asset.dat"},
		{"."},
		{"*.dat"},
		{"asset.*"},
		{"ass*"},
		{"*"},
		{"asse?.dat"},
		{"[a]sset.dat"},
		{":(glob)*.dat"},
		{"nested"},
		{"nested/"},
		{"nested/*"},
		{"nested/*.dat"},
		{"notes.txt", "*.dat"},
	} {
		_, _, err := s.reset(ctx, nil, resetIn{Paths: paths})
		if err == nil {
			t.Fatalf("reset %v reaches an unmerged path and must be refused", paths)
		}
		if !strings.Contains(err.Error(), "unmerged") || !strings.Contains(err.Error(), "forge_resolve_conflict") {
			t.Fatalf("reset %v must say why and where to go instead: %v", paths, err)
		}
		// A refusal that let part of the reset through would leave fewer sides in
		// the index, and every form after it would be testing a repository that
		// had already lost one.
		unmerged, err := s.unmergedPaths(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if len(unmerged) != 2 || !s.merging(ctx) {
			t.Fatalf("after refusing %v the index must hold both paths' sides and the merge must still be on: %v", paths, unmerged)
		}
	}

	// The refusal is about what a pathspec reaches, not about a merge being under
	// way: a wildcard that reaches nothing unmerged is an ordinary reset.
	writeFileT(t, filepath.Join(s.root, "notes.txt"), "edited\n", 0644)
	if _, _, err := s.add(ctx, nil, addIn{Paths: []string{"notes.txt"}}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.reset(ctx, nil, resetIn{Paths: []string{"*.txt"}}); err != nil {
		t.Fatalf("a wildcard reaching nothing unmerged is an ordinary reset: %v", err)
	}
	staged, err := forgerepo.GitOutput(ctx, s.root, "diff", "--cached", "--name-only", "--", "notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(staged)) != "" {
		t.Fatalf("notes.txt was reached by *.txt and must have been unstaged: %q", staged)
	}
}

// git explains a refused commit; the explanation has to reach the caller. The
// most common one — nothing staged — git writes to stdout and leaves stderr
// empty, so a caller reading only stderr is handed an exit status and nothing to
// act on.
func TestCommitCarriesGitsOwnExplanation(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	writeFileT(t, filepath.Join(s.root, "asset.unit"), "changed but never staged", 0644)
	before, err := forgerepo.GitOutput(ctx, s.root, "rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = s.commit(ctx, nil, commitIn{Message: "nothing is staged"})
	if err == nil {
		t.Fatal("a commit with nothing staged must fail")
	}
	if !strings.Contains(err.Error(), "no changes added to commit") {
		t.Fatalf("git's own explanation must reach the caller, got: %v", err)
	}
	after, err := forgerepo.GitOutput(ctx, s.root, "rev-list", "--count", "HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatal("a refused commit must record nothing")
	}
}

func TestFormatsWritesAreRecordedInTheTrackedFile(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	_, added, err := s.formatsAdd(ctx, nil, formatsEditIn{Extension: "Widget"})
	if err != nil {
		t.Fatal(err)
	}
	if added.Extension != ".widget" || added.State != "opted-in" {
		t.Fatalf("formatsAdd = %+v", added)
	}
	if !forgerepo.LoadForgeFormats(s.root)[".widget"] {
		t.Fatal("the opt-in must be in the repository's format list")
	}
	if added.Installed || added.Semantic || !strings.Contains(added.Note, "inactive") {
		t.Fatalf("an extension with no handler is inactive and must say so: %+v", added)
	}

	// Ignoring flips it out of the opted-in list rather than leaving both.
	_, ignored, err := s.formatsIgnore(ctx, nil, formatsEditIn{Extension: ".widget"})
	if err != nil {
		t.Fatal(err)
	}
	if ignored.State != "ignored" {
		t.Fatalf("formatsIgnore = %+v", ignored)
	}
	if forgerepo.LoadForgeFormats(s.root)[".widget"] || !forgerepo.LoadIgnoredFormats(s.root)[".widget"] {
		t.Fatal("an ignore must replace the opt-in, not sit beside it")
	}

	// An ignore holds against an installed handler, which is what makes it a
	// decision rather than a description.
	if _, _, err := s.formatsIgnore(ctx, nil, formatsEditIn{Extension: ".unit"}); err != nil {
		t.Fatal(err)
	}
	_, unit, err := s.handlerFor(ctx, nil, handlerForIn{Path: "asset.unit"})
	if err != nil {
		t.Fatal(err)
	}
	if unit.Semantic {
		t.Fatalf("an ignored extension must not be answered semantically: %+v", unit)
	}

	for _, bad := range []string{"", "  ", ".", "a/b", "*.unit"} {
		if _, _, err := s.formatsAdd(ctx, nil, formatsEditIn{Extension: bad}); err == nil {
			t.Fatalf("%q is not an extension and must be refused", bad)
		}
	}
}

// The install path stops at the trust boundary rather than reaching around it.
func TestInstallRefusesWhatNoConfiguredSourceOffers(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	_, _, err := s.install(ctx, nil, installIn{Extension: ".widget"})
	if err == nil {
		t.Fatal("an extension no source offers must be refused")
	}
	if !strings.Contains(err.Error(), "#47") || !strings.Contains(err.Error(), "cannot add a source") {
		t.Fatalf("the refusal must name the boundary rather than a way around it: %v", err)
	}

	if _, _, err := s.install(ctx, nil, installIn{Extension: ".widget", Source: "nowhere"}); err == nil ||
		!strings.Contains(err.Error(), "no source named") {
		t.Fatalf("a source that is not configured must be refused by name: %v", err)
	}

	if err := os.Remove(filepath.Join(os.Getenv("HOME"), ".forge", "sources.list")); err != nil {
		t.Fatal(err)
	}
	_, _, err = s.install(ctx, nil, installIn{Extension: ".widget"})
	if err == nil || !strings.Contains(err.Error(), "no handler sources are configured") {
		t.Fatalf("with no sources at all the refusal should say so: %v", err)
	}
}

// forge_install is the only tool that reaches code written for a terminal, and
// under `forge mcp` this process's stdout is the protocol channel. One line of
// human text on it is a parse error at the client, which ends the session — and
// it ends it after the handler is already on disk, so the caller loses both the
// answer and the connection to a call that did in fact succeed. Nothing this
// server does may write there; the frames are the only thing that goes to
// stdout, and everything a human would read goes to stderr.
func TestInstallLeavesTheProtocolChannelAlone(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	var manifestURL string
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "manifest.toml") {
			fmt.Fprintf(w, "name = \"probe\"\n\n[formats]\n\".probe\" = { handler = \"unit-probe\", build = \"deadbee\" }\n\n[assets.handlers.\"unit-probe\"]\n%q = %q\n",
				fhr.PlatformKey(), strings.TrimSuffix(manifestURL, "manifest.toml")+"handler")
			return
		}
		fmt.Fprint(w, "#!/bin/sh\nexit 0\n")
	}))
	defer source.Close()
	manifestURL = source.URL + "/manifest.toml"
	writeFileT(t, filepath.Join(os.Getenv("HOME"), ".forge", "sources.list"), "probe\t"+manifestURL+"\n", 0644)

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = w
	_, out, installErr := s.install(ctx, nil, installIn{Extension: ".probe"})
	os.Stdout = saved
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	onStdout, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	r.Close()

	if installErr != nil {
		t.Fatal(installErr)
	}
	// A cached handler skips the download, and it is the download that used to
	// write: the assertion is only worth anything if that path ran.
	if !out.Downloaded || fhr.InstalledHandlerBinary("unit-probe") == "" {
		t.Fatalf("this install must have gone all the way to the download: %+v", out)
	}
	if len(onStdout) > 0 {
		t.Fatalf("forge_install wrote %q to stdout, which under `forge mcp` carries the protocol", onStdout)
	}
}

// The read-only filter reads the annotation and never the name. A tool named
// like a write but annotated read-only is served; one named like a read but
// annotated as a write is not — which is the assertion a hardcoded list of names
// cannot pass.
func TestReadOnlyFilterFollowsTheAnnotationNotTheName(t *testing.T) {
	restricted, full := &server{readOnly: true}, &server{}

	writeName := &mcp.Tool{Name: "forge_commit", Annotations: readOnly("named like a write, writes nothing")}
	readName := &mcp.Tool{Name: "forge_status", Annotations: writeTool("named like a read, writes", false, true, false)}
	unannotated := &mcp.Tool{Name: "forge_status"}

	if !restricted.serves(writeName) {
		t.Error("a read-only-annotated tool is served whatever it is called")
	}
	if restricted.serves(readName) {
		t.Error("a tool annotated as a write is withheld whatever it is called")
	}
	if restricted.serves(unannotated) {
		t.Error("an unannotated tool is a destructive one by the spec's defaults and must be withheld")
	}
	for _, tool := range []*mcp.Tool{writeName, readName, unannotated} {
		if !full.serves(tool) {
			t.Errorf("%s must be served when writes are on", tool.Name)
		}
	}
}

func readT(t *testing.T, root, path string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
