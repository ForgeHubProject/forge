package mcpserver

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// stubHandlerScript is a handler binary for ".unit" that ignores the blobs it is
// handed and reports a fixed two-level change tree, so a test can assert the
// shape of a response — pagination, drill-down, filtering — without depending on
// any real format. It also answers the protocol's optional info call, declaring
// one capability supported and one not.
//
// The tree holds five changes: two roots, two children under the first, one
// under the second; three modified, one removed, one added.
const stubHandlerScript = `#!/bin/sh
case "$1" in
info)
  printf '%s\n' '{"id":"unit-stub","version":"1.2.3","protocol":"1.0","formats":[".unit"],"capabilities":{"semanticCompare":true,"semanticMerge":false}}'
  ;;
match)
  echo true
  ;;
diff)
  cat >/dev/null
  printf '%s\n' '{"version":"1.0","format":"unit-stub","changes":[
    {"path":"groupA","kind":"modified","label":"group A","children":[
      {"path":"groupA.one","kind":"modified","label":"one","before":"1","after":"2"},
      {"path":"groupA.two","kind":"removed","label":"two","before":"x"}]},
    {"path":"groupB","kind":"modified","label":"group B","children":[
      {"path":"groupB.three","kind":"added","label":"three","after":"y"}]}]}'
  ;;
*)
  echo "unknown subcommand: $1" >&2
  exit 1
  ;;
esac
`

// silentHandlerScript is a handler that diffs but does not implement the
// protocol's optional info call, which is the case forge_handler_for must report
// as "nothing known" rather than as an absence of capabilities.
const silentHandlerScript = `#!/bin/sh
case "$1" in
diff)
  cat >/dev/null
  printf '%s\n' '{"version":"1.0","format":"unit-silent","changes":[]}'
  ;;
*)
  echo "unimplemented" >&2
  exit 1
  ;;
esac
`

const stubBuild = "20240115-abc1234"

func writeFileT(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// newRepo builds a repository the server can be pointed at: the stub handler
// installed under a temp HOME, one handled file and one plain file over two
// commits, an uncommitted third version of the handled file, and an untracked
// file. It deliberately does not chdir — the server resolves every path against
// the root it was given, and nothing here should depend on the process's
// directory.
func newRepo(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub handlers are POSIX shell scripts")
	}

	home := t.TempDir()
	t.Setenv("HOME", home)
	plugins := filepath.Join(home, ".forge", "plugins")
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-stub"), stubHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-stub.json"),
		`{"id":"unit-stub","build":"`+stubBuild+`","source":"https://example.invalid/manifest.toml","formats":[".unit"]}`, 0644)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-silent"), silentHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-silent.json"),
		`{"id":"unit-silent","build":"nobuild","source":"https://example.invalid/manifest.toml","formats":[".silent"]}`, 0644)
	writeFileT(t, filepath.Join(home, ".forge", "sources.list"),
		"official\thttps://example.invalid/manifest.toml\n", 0644)

	root := t.TempDir()
	gitT(t, root, "init", "-b", "main", root)
	gitT(t, root, "config", "user.email", "t@example.com")
	gitT(t, root, "config", "user.name", "t")

	writeFileT(t, filepath.Join(root, ".forge", "formats"), ".unit\n.silent\n!.ignored\n", 0644)
	writeFileT(t, filepath.Join(root, ".forge", "handlers"), `{"unit-stub":"`+stubBuild+`"}`, 0644)
	writeFileT(t, filepath.Join(root, "asset.unit"), "v1", 0644)
	writeFileT(t, filepath.Join(root, "quiet.silent"), "x", 0644)
	writeFileT(t, filepath.Join(root, "notes.txt"), "line1\n", 0644)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-m", "one")

	writeFileT(t, filepath.Join(root, "asset.unit"), "v2", 0644)
	writeFileT(t, filepath.Join(root, "added.unit"), "new", 0644)
	writeFileT(t, filepath.Join(root, "notes.txt"), "line1\nline2\n", 0644)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-m", "two")

	writeFileT(t, filepath.Join(root, "asset.unit"), "v3", 0644)
	writeFileT(t, filepath.Join(root, "untracked.txt"), "u", 0644)
	return root
}

func newServer(t *testing.T) *server {
	t.Helper()
	return &server{root: newRepo(t)}
}

func TestStatusTagsWhatIsSemanticallyAnswerable(t *testing.T) {
	s := newServer(t)

	_, out, err := s.status(context.Background(), nil, noArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Root != s.root || out.Branch != "main" || out.Detached {
		t.Fatalf("root/branch wrong: %+v", out)
	}
	if out.Clean {
		t.Fatal("a repository with an edited and an untracked file is not clean")
	}

	got := map[string]statusEntry{}
	for _, e := range out.Entries {
		got[e.Path] = e
	}
	asset, ok := got["asset.unit"]
	if !ok || asset.State != "modified" || asset.HandlerID != "unit-stub" || asset.Staged || !asset.Unstaged {
		t.Fatalf("asset.unit entry wrong: %+v", asset)
	}
	if untracked, ok := got["untracked.txt"]; !ok || untracked.State != "untracked" || untracked.HandlerID != "" {
		t.Fatalf("untracked.txt entry wrong: %+v", untracked)
	}
	// The text catch-all is not a handler as far as an agent is concerned: saying
	// otherwise would advertise a semantic answer that does not exist.
	if _, listed := got["notes.txt"]; listed {
		t.Fatalf("notes.txt is unchanged and must not be listed: %+v", out.Entries)
	}
}

func TestSemanticDiffRevisionSemanticsMatchTheCLI(t *testing.T) {
	s := newServer(t)

	cases := []struct {
		name, base, head   string
		wantBase, wantHead string
	}{
		{"no revisions", "", "", "HEAD", "the working tree"},
		{"base only", "HEAD~1", "", "HEAD~1", "the working tree"},
		{"base and head", "HEAD~1", "HEAD", "HEAD~1", "HEAD"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, out, err := s.semanticDiff(context.Background(), nil, semanticDiffIn{Path: "asset.unit", Base: c.base, Head: c.head})
			if err != nil {
				t.Fatal(err)
			}
			if out.Comparison.Base != c.wantBase || out.Comparison.Head != c.wantHead {
				t.Fatalf("comparison = %+v, want %s → %s", out.Comparison, c.wantBase, c.wantHead)
			}
			if out.HandlerID == nil || *out.HandlerID != "unit-stub" || out.HandlerBuild != stubBuild {
				t.Fatalf("every semantic answer carries handler and build; got %+v build %q", out.HandlerID, out.HandlerBuild)
			}
			if out.Summary == nil || out.Summary.Total != 5 || len(out.Summary.TopLevel) != 2 {
				t.Fatalf("summary wrong: %+v", out.Summary)
			}
			if out.Summary.ByKind["modified"] != 3 || out.Summary.ByKind["removed"] != 1 || out.Summary.ByKind["added"] != 1 {
				t.Fatalf("byKind wrong: %+v", out.Summary.ByKind)
			}
			if out.Truncated.Truncated || out.Truncated.Returned != 5 {
				t.Fatalf("an uncapped tree must not report truncation: %+v", out.Truncated)
			}
		})
	}

	if _, _, err := s.semanticDiff(context.Background(), nil, semanticDiffIn{Path: "asset.unit", Head: "HEAD"}); err == nil {
		t.Fatal("a head with no base names no comparison and must be refused")
	}
}

func TestSemanticDiffTruncationIsExplicit(t *testing.T) {
	s := newServer(t)

	_, out, err := s.semanticDiff(context.Background(), nil, semanticDiffIn{Path: "asset.unit", MaxChanges: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !out.Truncated.Truncated || out.Truncated.Returned != 1 || out.Truncated.Total != 5 {
		t.Fatalf("a cap must report itself with true totals: %+v", out.Truncated)
	}
	if len(out.Changes) != 1 {
		t.Fatalf("expected one change, got %d", len(out.Changes))
	}
	// The one node returned is a root whose children were withheld, so the hint
	// must name it as the path that drills deeper.
	if !strings.Contains(out.Truncated.Hint, "groupA") || !strings.Contains(out.Truncated.Hint, "at=") {
		t.Fatalf("hint must name the drill-down path: %q", out.Truncated.Hint)
	}
	if out.Changes[0].ChildCount != 2 || out.Changes[0].ChildrenReturned != 0 {
		t.Fatalf("a cut subtree must say so on the node: %+v", out.Changes[0])
	}
	if out.Summary.Total != 5 {
		t.Fatalf("the summary must still describe the whole tree: %+v", out.Summary)
	}
}

func TestSemanticDiffDrillsDownByPath(t *testing.T) {
	s := newServer(t)

	_, out, err := s.semanticDiff(context.Background(), nil, semanticDiffIn{Path: "asset.unit", At: "groupA"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Truncated.Total != 3 || len(out.Changes) != 3 {
		t.Fatalf("at=groupA is that subtree alone: %+v / %d changes", out.Truncated, len(out.Changes))
	}
	if out.Changes[0].Path != "groupA" || out.Changes[0].Depth != 0 || out.Changes[0].ChildrenReturned != 2 {
		t.Fatalf("the named change is the root of the response: %+v", out.Changes[0])
	}
	if out.Changes[1].Parent != "groupA" || out.Changes[1].Path != "groupA.one" || out.Changes[1].Depth != 1 {
		t.Fatalf("children keep their qualified paths and their parent: %+v", out.Changes[1])
	}
	for _, n := range out.Changes {
		if strings.HasPrefix(n.Path, "groupB") {
			t.Fatalf("a drill-down must not carry the other subtree: %+v", out.Changes)
		}
	}

	// A child path is as good a cursor as a root, since every path in a response
	// is a fully-qualified address.
	_, child, err := s.semanticDiff(context.Background(), nil, semanticDiffIn{Path: "asset.unit", At: "groupA.two"})
	if err != nil {
		t.Fatal(err)
	}
	if child.Truncated.Total != 1 || child.Changes[0].Kind != "removed" {
		t.Fatalf("at=groupA.two is one leaf: %+v", child.Changes)
	}

	// A path this comparison does not hold is answered, not guessed at.
	_, missing, err := s.semanticDiff(context.Background(), nil, semanticDiffIn{Path: "asset.unit", At: "groupZ"})
	if err != nil {
		t.Fatal(err)
	}
	if len(missing.Changes) != 0 || !strings.Contains(missing.Note, "groupZ") {
		t.Fatalf("an unknown at must be reported: %+v", missing)
	}
}

func TestSemanticDiffFiltersByKind(t *testing.T) {
	s := newServer(t)

	_, out, err := s.semanticDiff(context.Background(), nil, semanticDiffIn{Path: "asset.unit", Kinds: []string{"removed"}})
	if err != nil {
		t.Fatal(err)
	}
	// The removal plus the container it hangs under: dropping the container would
	// make the change unreachable and its address meaningless.
	if out.Summary.Total != 2 || out.Summary.ByKind["removed"] != 1 {
		t.Fatalf("kinds filter wrong: %+v", out.Summary)
	}
	var paths []string
	for _, n := range out.Changes {
		paths = append(paths, n.Path)
	}
	if strings.Join(paths, ",") != "groupA,groupA.two" {
		t.Fatalf("expected the removal and its parent, got %v", paths)
	}
}

func TestSemanticDiffFallsBackToTextAndSaysSo(t *testing.T) {
	s := newServer(t)

	_, out, err := s.semanticDiff(context.Background(), nil, semanticDiffIn{Path: "notes.txt", Base: "HEAD~1", Head: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if out.HandlerID != nil {
		t.Fatalf("a path no handler claims reports no handler: %v", *out.HandlerID)
	}
	if out.Fallback != "text" || !strings.Contains(out.TextDiff, "+line2") {
		t.Fatalf("expected git's own diff: %+v", out)
	}

	// The text fallback is capped as explicitly as a change tree is.
	_, capped, err := s.semanticDiff(context.Background(), nil, semanticDiffIn{Path: "notes.txt", Base: "HEAD~1", Head: "HEAD", MaxChanges: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !capped.Truncated.Truncated || capped.Truncated.Returned != 2 || capped.Truncated.Total <= 2 {
		t.Fatalf("a capped text diff must report itself: %+v", capped.Truncated)
	}
	if len(strings.Split(capped.TextDiff, "\n")) != 2 {
		t.Fatalf("expected two lines, got %q", capped.TextDiff)
	}
}

func TestPathsOutsideTheRepositoryAreRefused(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	for _, p := range []string{"../escape.unit", "sub/../../escape.unit", "/etc/passwd", ""} {
		if _, _, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: p}); err == nil {
			t.Errorf("forge_semantic_diff accepted %q", p)
		}
		if _, _, err := s.handlerFor(ctx, nil, handlerForIn{Path: p}); err == nil {
			t.Errorf("forge_handler_for accepted %q", p)
		}
		if _, _, err := s.show(ctx, nil, showIn{Ref: "HEAD", Path: p}); err == nil && p != "" {
			t.Errorf("forge_show accepted %q", p)
		}
	}
}

func TestShowReportsCommitAndPerFileShape(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	_, out, err := s.show(ctx, nil, showIn{Ref: "HEAD"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Commit.SHA == "" || out.Commit.Subject != "two" || out.Commit.Parent == "" || out.Commit.Author == "" || out.Commit.Date == "" {
		t.Fatalf("commit metadata incomplete: %+v", out.Commit)
	}
	if out.Comparison.Base != "HEAD^" || out.Comparison.Head != "HEAD" {
		t.Fatalf("show compares a commit against its first parent: %+v", out.Comparison)
	}

	files := map[string]showFile{}
	for _, f := range out.Files {
		files[f.Path] = f
	}
	asset := files["asset.unit"]
	if asset.HandlerID == nil || *asset.HandlerID != "unit-stub" || asset.HandlerBuild != stubBuild {
		t.Fatalf("a handled file carries handler and build: %+v", asset)
	}
	if asset.Summary == nil || asset.Summary.Total != 5 || len(asset.Changes) != 0 {
		t.Fatalf("without a path, a handled file gets its summary and no tree: %+v", asset)
	}
	if !strings.Contains(asset.Note, "forge_semantic_diff") {
		t.Fatalf("the summary should say where the tree is: %q", asset.Note)
	}
	notes := files["notes.txt"]
	if notes.HandlerID != nil || notes.TextSummary != "+1 -0" {
		t.Fatalf("a plain file gets git's counts: %+v", notes)
	}

	// A named path drills all the way in.
	_, narrowed, err := s.show(ctx, nil, showIn{Ref: "HEAD", Path: "asset.unit", MaxChanges: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(narrowed.Files) != 1 || len(narrowed.Files[0].Changes) != 2 {
		t.Fatalf("expected one file with two changes: %+v", narrowed.Files)
	}
	tr := narrowed.Files[0].Truncated
	if tr == nil || !tr.Truncated || tr.Total != 5 {
		t.Fatalf("a narrowed show truncates as explicitly as a diff: %+v", tr)
	}

	if _, _, err := s.show(ctx, nil, showIn{Ref: "no-such-rev"}); err == nil {
		t.Fatal("an unresolvable ref must be refused by name")
	}
}

func TestHandlerForReportsWhatTheHandlerDeclares(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	_, out, err := s.handlerFor(ctx, nil, handlerForIn{Path: "asset.unit"})
	if err != nil {
		t.Fatal(err)
	}
	if out.HandlerID != "unit-stub" || !out.Installed || !out.OptedIn || !out.Semantic {
		t.Fatalf("handler state wrong: %+v", out)
	}
	if out.Build != stubBuild || out.PinnedBuild != stubBuild || out.Source == "" {
		t.Fatalf("build provenance incomplete: %+v", out)
	}
	if out.Capability == nil {
		t.Fatal("the stub answers the info call, so its capabilities are knowable")
	}
	if out.Capability.SemanticCompare != "supported" || out.Capability.SemanticMerge != "unsupported" {
		t.Fatalf("capabilities must be reported as declared: %+v", out.Capability)
	}
	if out.Capability.Version != "1.2.3" || out.Capability.Protocol != "1.0" {
		t.Fatalf("info fields missing: %+v", out.Capability)
	}

	// A handler that does not answer the optional info call has said nothing —
	// which is not the same as declaring nothing supported.
	_, silent, err := s.handlerFor(ctx, nil, handlerForIn{Path: "quiet.silent"})
	if err != nil {
		t.Fatal(err)
	}
	if silent.HandlerID != "unit-silent" || silent.Capability != nil || !strings.Contains(silent.Note, "info") {
		t.Fatalf("a silent handler must be reported as unknown, not as unsupported: %+v", silent)
	}

	// A path no handler claims, and an extension the repository ignores.
	_, plain, err := s.handlerFor(ctx, nil, handlerForIn{Path: "notes.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.HandlerID != "" || plain.Semantic || plain.Note == "" {
		t.Fatalf("a path with no handler must say so: %+v", plain)
	}
	_, ignored, err := s.handlerFor(ctx, nil, handlerForIn{Path: "thing.ignored"})
	if err != nil {
		t.Fatal(err)
	}
	if !ignored.Ignored || ignored.Semantic {
		t.Fatalf("an ignored extension must be reported as ignored: %+v", ignored)
	}
}

func TestFormatsReportsOptInAndDrift(t *testing.T) {
	s := newServer(t)

	_, out, err := s.formats(context.Background(), nil, noArgs{})
	if err != nil {
		t.Fatal(err)
	}
	byExt := map[string]formatEntry{}
	for _, f := range out.Formats {
		byExt[f.Extension] = f
	}
	if unit := byExt[".unit"]; unit.State != "opted-in" || !unit.Installed || unit.HandlerID != "unit-stub" || unit.PinnedBuild != stubBuild {
		t.Fatalf(".unit entry wrong: %+v", unit)
	}
	if ignored := byExt[".ignored"]; ignored.State != "ignored" || ignored.Installed || ignored.HandlerID != "" {
		t.Fatalf(".ignored entry wrong: %+v", ignored)
	}
}

func TestSourceListStatesTheBoundary(t *testing.T) {
	s := newServer(t)

	_, out, err := s.sourceList(context.Background(), nil, noArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Sources) != 1 || out.Sources[0].Name != "official" {
		t.Fatalf("expected the configured source: %+v", out.Sources)
	}
	if out.Mutable {
		t.Fatal("this server cannot mutate the source list and must not say it can")
	}
	if !strings.Contains(out.Boundary, "#47") {
		t.Fatalf("the boundary should point at where it is argued: %q", out.Boundary)
	}
}

// Outside a repository there is nothing to resolve paths against, so the server
// refuses to start rather than answering from wherever it was launched.
func TestRunRefusesOutsideARepository(t *testing.T) {
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Run(); err == nil {
		t.Skip("the temp directory is itself inside a repository")
	}
	t.Chdir(dir)

	err := Run(context.Background())
	if err == nil || !strings.Contains(err.Error(), "must be started inside one") {
		t.Fatalf("expected a clean refusal, got: %v", err)
	}
}
