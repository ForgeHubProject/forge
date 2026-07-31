package mcpserver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// mergeHandlerScript is a handler for ".merge" that implements the two calls the
// resolution loop needs and nothing else it depends on: merge reports a fixed
// pair of conflicts, and apply-choices reports back which of them the caller
// asked to take from theirs — encoded into the blob it returns, so a test can
// assert that the choices reached the handler rather than only that a file was
// written. The blobs it is handed are ignored: what is being exercised is the
// loop, not any format.
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
  cat >/dev/null
  printf '{"blob":"%s","conflicts":[{"path":"alpha","ours":"a-ours","theirs":"a-theirs"},{"path":"beta","ours":"b-ours","theirs":"b-theirs"}]}\n' "$(printf 'merged' | base64)"
  ;;
apply-choices)
  body=$(cat)
  taken=""
  case "$body" in *'"alpha"'*) taken="${taken}alpha," ;; esac
  case "$body" in *'"beta"'*) taken="${taken}beta," ;; esac
  printf '{"blob":"%s"}\n' "$(printf 'merged+%s' "$taken" | base64)"
  ;;
*)
  echo "unknown subcommand: $1" >&2
  exit 1
  ;;
esac
`

// noApplyHandlerScript is a handler for ".noapply" that merges and reports a
// conflict but does not implement apply-choices — the handler most of the
// ecosystem is, since the call is optional. Taking anything from theirs has to
// fail against it, and say so.
const noApplyHandlerScript = `#!/bin/sh
case "$1" in
merge)
  cat >/dev/null
  printf '{"blob":"%s","conflicts":[{"path":"gamma","ours":"g-ours","theirs":"g-theirs"}]}\n' "$(printf 'merged' | base64)"
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

// newMergeRepo builds a repository stopped in the middle of a merge, with three
// unmerged paths: one whose handler can apply choices, one whose handler cannot,
// and one no handler claims at all.
func newMergeRepo(t *testing.T) *server {
	t.Helper()
	root := newRepo(t)

	plugins := filepath.Join(os.Getenv("HOME"), ".forge", "plugins")
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-merge"), mergeHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-merge.json"),
		`{"id":"unit-merge","build":"`+mergeBuild+`","source":"https://example.invalid/manifest.toml","formats":[".merge"]}`, 0644)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-noapply"), noApplyHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-noapply.json"),
		`{"id":"unit-noapply","build":"nobuild","source":"https://example.invalid/manifest.toml","formats":[".noapply"]}`, 0644)
	writeFileT(t, filepath.Join(root, ".forge", "formats"), ".unit\n.merge\n.noapply\n!.ignored\n", 0644)

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
		writeFileT(t, filepath.Join(root, "stuck.noapply"), side.content, 0644)
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
	if len(files) != 3 {
		t.Fatalf("expected three unmerged paths, got %d: %+v", len(files), out.Files)
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
	if !out.Truncated.Truncated || out.Truncated.Returned != 1 || out.Truncated.Total != 3 {
		t.Fatalf("a cap must report itself with true totals: %+v", out.Truncated)
	}
	if !strings.Contains(out.Truncated.Hint, `after="asset.merge"`) {
		t.Fatalf("the hint must name the cursor that continues the listing: %q", out.Truncated.Hint)
	}

	_, rest, err := s.conflicts(ctx, nil, conflictsIn{After: "asset.merge"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Files) != 2 || rest.Files[0].Path != "plain.txt" {
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

func TestResolveConflictAppliesChoicesAndWritesTheResult(t *testing.T) {
	s := newMergeRepo(t)
	ctx := context.Background()

	_, out, err := s.resolveConflict(ctx, nil, resolveConflictIn{
		Path: "asset.merge",
		Choices: []conflictChoice{
			{Path: "alpha", Take: "theirs"},
			{Path: "beta", Take: "ours"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.HandlerID != "unit-merge" || out.HandlerBuild != mergeBuild {
		t.Fatalf("every semantic payload carries handler and build: %+v", out)
	}
	// The blob the handler returned encodes the paths it was told to take, so this
	// asserts the choices reached it and not merely that something was written.
	if got := readT(t, s.root, "asset.merge"); got != "merged+alpha," {
		t.Fatalf("working tree holds %q, want the result of taking alpha from theirs", got)
	}
	if len(out.Applied) != 2 || out.Applied[0].Take != "theirs" || out.Applied[1].Take != "ours" {
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

	// Idempotent, and re-decidable: the same call gives the same bytes, and a
	// different call overwrites its own result rather than compounding it.
	if _, _, err := s.resolveConflict(ctx, nil, resolveConflictIn{Path: "asset.merge", Choices: []conflictChoice{
		{Path: "alpha", Take: "ours"}, {Path: "beta", Take: "ours"},
	}}); err != nil {
		t.Fatal(err)
	}
	if got := readT(t, s.root, "asset.merge"); got != "merged" {
		t.Fatalf("an all-ours resolution is the merged blob unchanged, got %q", got)
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

	// A handler that merges but cannot apply choices answers for an all-ours
	// resolution and refuses the rest, saying which it was.
	if _, _, err := s.resolveConflict(ctx, nil, resolveConflictIn{
		Path: "stuck.noapply", Choices: []conflictChoice{{Path: "gamma", Take: "ours"}},
	}); err != nil {
		t.Fatalf("all-ours needs no apply-choices call: %v", err)
	}
	_, _, err := s.resolveConflict(ctx, nil, resolveConflictIn{
		Path: "stuck.noapply", Choices: []conflictChoice{{Path: "gamma", Take: "theirs"}},
	})
	if err == nil || !strings.Contains(err.Error(), "unit-noapply") {
		t.Fatalf("expected the handler's own refusal, got: %v", err)
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
	if got := readT(t, s.root, "asset.merge"); got != "merged+alpha,beta," {
		t.Fatalf("both sides taken from theirs should show in the result, got %q", got)
	}

	// The other two are resolved the way a caller without a handler has to: edit
	// and stage.
	writeFileT(t, filepath.Join(s.root, "plain.txt"), "resolved\n", 0644)
	writeFileT(t, filepath.Join(s.root, "stuck.noapply"), "resolved", 0644)
	if _, _, err := s.add(ctx, nil, addIn{Paths: []string{"asset.merge", "plain.txt", "stuck.noapply"}}); err != nil {
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
