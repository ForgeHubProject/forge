package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
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

// wideRoots is how many roots wideHandlerScript reports: more than one page at
// the cap the paging tests use, and more than one page again after that, so a
// test can walk a level wider than any single response. It reaches the handler
// through the environment, since the protocol's diff call carries only blobs.
const wideRoots = 12

// wideHandlerScript is a handler for ".wide" that reports a flat row of changes
// with nothing under them — the shape that has no subtree to drill into, so the
// only way through it is to page along the level itself.
const wideHandlerScript = `#!/bin/sh
case "$1" in
diff)
  cat >/dev/null
  printf '{"version":"1.0","format":"unit-wide","changes":['
  i=0
  while [ "$i" -lt "$FORGE_TEST_WIDE_ROOTS" ]; do
    if [ "$i" -gt 0 ]; then printf ','; fi
    printf '{"path":"root%02d","kind":"modified"}' "$i"
    i=$((i + 1))
  done
  printf ']}\n'
  ;;
*)
  echo "unimplemented" >&2
  exit 1
  ;;
esac
`

// hangHandlerScript is a handler that never answers. It records its own pid and
// then becomes the sleep — exec, so the pid the test watches is the process forge
// spawned — which is how a test can tell whether a cancelled call took its
// handler with it or left it running.
const hangHandlerScript = `#!/bin/sh
case "$1" in
diff)
  cat >/dev/null
  echo $$ > "$FORGE_TEST_HANG_PID"
  exec sleep 120
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
	t.Setenv("FORGE_TEST_WIDE_ROOTS", strconv.Itoa(wideRoots))
	plugins := filepath.Join(home, ".forge", "plugins")
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-stub"), stubHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-stub.json"),
		`{"id":"unit-stub","build":"`+stubBuild+`","source":"https://example.invalid/manifest.toml","formats":[".unit"]}`, 0644)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-silent"), silentHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-silent.json"),
		`{"id":"unit-silent","build":"nobuild","source":"https://example.invalid/manifest.toml","formats":[".silent"]}`, 0644)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-wide"), wideHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-wide.json"),
		`{"id":"unit-wide","build":"nobuild","source":"https://example.invalid/manifest.toml","formats":[".wide"]}`, 0644)
	writeFileT(t, filepath.Join(home, ".forge", "sources.list"),
		"official\thttps://example.invalid/manifest.toml\n", 0644)

	root := t.TempDir()
	gitT(t, root, "init", "-b", "main", root)
	gitT(t, root, "config", "user.email", "t@example.com")
	gitT(t, root, "config", "user.name", "t")

	writeFileT(t, filepath.Join(root, ".forge", "formats"), ".unit\n.silent\n.wide\n!.ignored\n", 0644)
	writeFileT(t, filepath.Join(root, ".forge", "handlers"), `{"unit-stub":"`+stubBuild+`"}`, 0644)
	writeFileT(t, filepath.Join(root, "asset.unit"), "v1", 0644)
	writeFileT(t, filepath.Join(root, "quiet.silent"), "x", 0644)
	writeFileT(t, filepath.Join(root, "level.wide"), "x", 0644)
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

// newServerNoFormats serves a repository that lists no formats at all — the state
// of every repository that has not run forge formats add, since forge init writes
// no list. The handlers stay installed: what changes is only what the repository
// says about them, which is nothing.
func newServerNoFormats(t *testing.T) *server {
	t.Helper()
	root := newRepo(t)
	if err := os.Remove(filepath.Join(root, ".forge", "formats")); err != nil {
		t.Fatal(err)
	}
	return &server{root: root}
}

// newServerLegacyLayout serves a repository using the root-level names forge
// still reads, which is the layout whose deprecation note is remembered in
// package state and so the one a concurrent caller can collide over.
func newServerLegacyLayout(t *testing.T) *server {
	t.Helper()
	root := newRepo(t)
	for _, f := range [][2]string{{".forge/formats", ".forge-formats"}, {".forge/handlers", ".forge-handlers"}} {
		if err := os.Rename(filepath.Join(root, filepath.FromSlash(f[0])), filepath.Join(root, f[1])); err != nil {
			t.Fatal(err)
		}
	}
	return &server{root: root}
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

// A repository that lists no formats is the common case, not an edge: forge init
// writes no list, and an empty list filters nothing, so every installed handler
// answers there. What the two reporting tools say about such a repository has to
// be what forge_semantic_diff then does, or an agent that asks first is talked
// out of the one call that would have worked.
func TestReportingAgreesWithTheAnswerWhenNoFormatsAreListed(t *testing.T) {
	s := newServerNoFormats(t)
	ctx := context.Background()

	_, formats, err := s.formats(ctx, nil, noArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if formats.OptInList {
		t.Fatal("a repository with no format list must say so")
	}
	byExt := map[string]formatEntry{}
	for _, f := range formats.Formats {
		byExt[f.Extension] = f
	}
	unit, listed := byExt[".unit"]
	if !listed {
		t.Fatalf("an installed handler's extension is what forge can be asked about here: %+v", formats)
	}
	if unit.State != "unlisted" || !unit.Installed || !unit.Semantic || unit.HandlerID != "unit-stub" {
		t.Fatalf(".unit entry wrong: %+v", unit)
	}
	if strings.Contains(formats.Note, "every path falls back") {
		t.Fatalf("the note claims a fallback that does not happen: %q", formats.Note)
	}

	_, handler, err := s.handlerFor(ctx, nil, handlerForIn{Path: "asset.unit"})
	if err != nil {
		t.Fatal(err)
	}
	if handler.OptedIn || !handler.Semantic {
		t.Fatalf("an unlisted extension with an installed handler is still answered: %+v", handler)
	}
	if strings.Contains(handler.Note, "fall back to text") {
		t.Fatalf("the note contradicts semanticDiffAvailable: %q", handler.Note)
	}

	// The reports are checked against the answer itself, which is the only thing
	// that makes them true or false.
	_, diff, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "asset.unit"})
	if err != nil {
		t.Fatal(err)
	}
	if diff.HandlerID == nil || *diff.HandlerID != "unit-stub" || diff.Fallback != "" || diff.Summary.Total != 5 {
		t.Fatalf("a repository that lists nothing still gets a semantic answer: %+v", diff)
	}

	// A machine with no handler installed at all is the case that really has
	// nothing to answer, and it is reported differently.
	empty := &server{root: newRepo(t)}
	if err := os.RemoveAll(filepath.Join(os.Getenv("HOME"), ".forge", "plugins")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(empty.root, ".forge", "formats")); err != nil {
		t.Fatal(err)
	}
	_, none, err := empty.formats(ctx, nil, noArgs{})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Formats) != 0 || !strings.Contains(none.Note, "no handler is installed") {
		t.Fatalf("with nothing installed there is nothing to answer, and that is what to say: %+v", none)
	}
}

// forge_show hands the caller a next step for every file it summarises. A root
// commit is compared against nothing, and nothing is not a revision, so the step
// has to name the comparison the way forge_semantic_diff will accept it —
// otherwise the one commit whose whole content is new is the one commit whose
// change tree cannot be reached.
func TestShowOnARootCommitNamesAStepThatWorks(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	_, out, err := s.show(ctx, nil, showIn{Ref: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Comparison.Base != "nothing" || out.Comparison.Head != "HEAD~1" {
		t.Fatalf("a root commit is compared against nothing: %+v", out.Comparison)
	}
	var note string
	for _, f := range out.Files {
		if f.Path == "asset.unit" {
			note = f.Note
		}
	}
	if !strings.Contains(note, `base_empty=true`) || !strings.Contains(note, `head="HEAD~1"`) {
		t.Fatalf("the next step must name the empty side as forge_semantic_diff names it: %q", note)
	}
	if strings.Contains(note, `base="nothing"`) {
		t.Fatalf("no revision is called nothing, so no call can pass it: %q", note)
	}

	// The step the response gave, made as an actual call.
	_, diff, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "asset.unit", BaseEmpty: true, Head: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if diff.Comparison.Base != "nothing" || diff.Comparison.Head != "HEAD~1" {
		t.Fatalf("comparison must echo the empty side: %+v", diff.Comparison)
	}
	if diff.HandlerID == nil || diff.Summary == nil || diff.Summary.Total != 5 {
		t.Fatalf("a root commit's change tree must be reachable: %+v", diff)
	}

	// A path with no handler takes the same route: git can diff against nothing.
	_, text, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "notes.txt", BaseEmpty: true, Head: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if text.Fallback != "text" || !strings.Contains(text.TextDiff, "+line1") {
		t.Fatalf("expected git's own diff against the empty side: %+v", text)
	}

	// The parameter says what it means and refuses what it cannot mean.
	if _, _, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "asset.unit", BaseEmpty: true}); err == nil {
		t.Fatal("base_empty needs a head")
	}
	if _, _, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "asset.unit", BaseEmpty: true, Base: "HEAD~1", Head: "HEAD"}); err == nil {
		t.Fatal("base_empty and base name two different base sides and must not be accepted together")
	}
}

// A cap with no way past it is a cap that hides changes: when the budget goes on
// siblings there is no subtree to drill into, and raising the cap means asking
// for the whole tree — the thing the cap exists to prevent. Every page must be
// reachable from the one before it, and the pages must not overlap or skip.
func TestSemanticDiffPagesThroughALevelWiderThanTheCap(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	const perPage = 5
	seen := []string{}
	after := ""
	for page := 0; ; page++ {
		if page > wideRoots {
			t.Fatal("paging did not terminate")
		}
		_, out, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "level.wide", After: after, MaxChanges: perPage})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range out.Changes {
			seen = append(seen, n.Path)
		}
		if !out.Truncated.Truncated {
			break
		}
		if !strings.Contains(out.Truncated.Hint, "after=") {
			t.Fatalf("a level cut short must name the cursor that continues it: %q", out.Truncated.Hint)
		}
		after = out.Changes[len(out.Changes)-1].Path
	}

	if len(seen) != wideRoots {
		t.Fatalf("paging reached %d of %d changes: %v", len(seen), wideRoots, seen)
	}
	for i, path := range seen {
		if want := fmt.Sprintf("root%02d", i); path != want {
			t.Fatalf("pages must not overlap or skip: change %d is %q, want %q", i, path, want)
		}
	}

	// The summary of a page describes that page's own tail, so the totals shrink
	// as the walk proceeds rather than describing a tree the response is not.
	_, first, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "level.wide", MaxChanges: perPage})
	if err != nil {
		t.Fatal(err)
	}
	if first.Summary.Total != wideRoots || first.Summary.TopLevelWithheld != wideRoots-perPage {
		t.Fatalf("the first page describes the whole level: %+v", first.Summary)
	}
	_, second, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "level.wide", After: "root04", MaxChanges: perPage})
	if err != nil {
		t.Fatal(err)
	}
	if second.Summary.Total != wideRoots-perPage || second.Changes[0].Path != "root05" {
		t.Fatalf("a page describes what follows its cursor: %+v / %+v", second.Summary, second.Changes)
	}

	// The end of a level says so rather than looking like an empty comparison.
	_, last, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "level.wide", After: fmt.Sprintf("root%02d", wideRoots-1)})
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Changes) != 0 || !strings.Contains(last.Note, "last change at its level") {
		t.Fatalf("the end of a level must be reported: %+v", last)
	}
	_, unknown, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "level.wide", After: "rootZZ"})
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown.Changes) != 0 || !strings.Contains(unknown.Note, "nothing to continue from") {
		t.Fatalf("a cursor this comparison does not hold must be reported: %+v", unknown)
	}
}

// A level below the roots is paged the same way, and the paths in a resumed
// response stay the fully-qualified addresses they were.
func TestSemanticDiffPagesAnInnerLevel(t *testing.T) {
	s := newServer(t)

	_, out, err := s.semanticDiff(context.Background(), nil, semanticDiffIn{Path: "asset.unit", After: "groupA.one"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Changes) != 1 || out.Changes[0].Path != "groupA.two" {
		t.Fatalf("expected the sibling that follows groupA.one: %+v", out.Changes)
	}
	if out.Changes[0].Depth != 0 || out.Changes[0].Parent != "groupA" {
		t.Fatalf("a resumed page names the level it resumed under: %+v", out.Changes[0])
	}
	if out.Summary.Total != 1 || out.Summary.TopLevel[0].Path != "groupA.two" {
		t.Fatalf("the summary describes the page: %+v", out.Summary)
	}
}

// forge_show caps its file list too, and the files past the cap have no other
// tool to name them: the listing is the only place they appear.
func TestShowPagesThroughItsFileList(t *testing.T) {
	s := newServer(t)
	ctx := context.Background()

	_, all, err := s.show(ctx, nil, showIn{Ref: "HEAD~1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(all.Files) < 3 {
		t.Fatalf("the root commit should hold several files: %+v", all.Files)
	}

	var walked []string
	after := ""
	for page := 0; ; page++ {
		if page > len(all.Files) {
			t.Fatal("paging did not terminate")
		}
		_, out, err := s.show(ctx, nil, showIn{Ref: "HEAD~1", After: after, MaxChanges: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range out.Files {
			walked = append(walked, f.Path)
		}
		if !out.Truncated.Truncated {
			break
		}
		if !strings.Contains(out.Truncated.Hint, "after=") {
			t.Fatalf("a cut file list must name the cursor that continues it: %q", out.Truncated.Hint)
		}
		after = out.Files[len(out.Files)-1].Path
	}

	var want []string
	for _, f := range all.Files {
		want = append(want, f.Path)
	}
	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Fatalf("paging the file list gave %v, want %v", walked, want)
	}

	_, end, err := s.show(ctx, nil, showIn{Ref: "HEAD~1", After: want[len(want)-1]})
	if err != nil {
		t.Fatal(err)
	}
	if len(end.Files) != 0 || !strings.Contains(end.Note, "last file") {
		t.Fatalf("the end of the listing must be reported: %+v", end)
	}
}

// Every call runs under a deadline, because a handler is a native executable this
// server does not control and a server, unlike a command, does not end.
func TestEveryToolCallRunsUnderADeadline(t *testing.T) {
	s := &server{root: t.TempDir(), stopping: context.Background()}

	var deadline time.Time
	h := bounded(s, func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, noArgs, error) {
		var ok bool
		if deadline, ok = ctx.Deadline(); !ok {
			t.Error("a tool call must run under a deadline")
		}
		return nil, noArgs{}, nil
	})
	if _, _, err := h(context.Background(), nil, noArgs{}); err != nil {
		t.Fatal(err)
	}
	if left := time.Until(deadline); left <= 0 || left > toolTimeout {
		t.Fatalf("deadline is %v away, want at most %v", left, toolTimeout)
	}

	// A call that spends its deadline is reported as abandoned, rather than as
	// whatever error its killed subprocess produced on the way out.
	expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	slow := bounded(s, func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, noArgs, error) {
		return nil, noArgs{}, errors.New("signal: killed")
	})
	_, _, err := slow(expired, nil, noArgs{})
	if err == nil || !strings.Contains(err.Error(), "abandoned") {
		t.Fatalf("expected the deadline to be named in the error, got %v", err)
	}

	// A server shutting down has to reach the calls in flight itself: the SDK
	// derives a call's context from the connection, not from the context the
	// server was run with, so a call left holding a handler would otherwise keep
	// the shutdown waiting for it.
	stopping, stopNow := context.WithCancel(context.Background())
	held := bounded(&server{root: s.root, stopping: stopping}, func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, noArgs, error) {
		<-ctx.Done()
		return nil, noArgs{}, ctx.Err()
	})
	returned := make(chan error, 1)
	go func() {
		_, _, err := held(context.Background(), nil, noArgs{})
		returned <- err
	}()
	stopNow()
	select {
	case err := <-returned:
		if err == nil {
			t.Fatal("a call cut short by shutdown did not report it")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("shutdown must reach a call in flight")
	}
}

// A cancelled call must return, and the handler it started must go with it. The
// alternative is what a client cannot recover from: a request that never answers
// and a subprocess that outlives the session it was spawned for.
func TestACancelledCallTakesItsHandlerWithIt(t *testing.T) {
	s := newServer(t)

	pidFile := filepath.Join(t.TempDir(), "handler.pid")
	t.Setenv("FORGE_TEST_HANG_PID", pidFile)
	plugins := filepath.Join(os.Getenv("HOME"), ".forge", "plugins")
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-hang"), hangHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-hang.json"),
		`{"id":"unit-hang","build":"nobuild","source":"https://example.invalid/manifest.toml","formats":[".hang"]}`, 0644)
	writeFileT(t, filepath.Join(s.root, ".forge", "formats"), ".unit\n.silent\n.wide\n.hang\n!.ignored\n", 0644)
	writeFileT(t, filepath.Join(s.root, "stuck.hang"), "x", 0644)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, _, err := s.semanticDiff(ctx, nil, semanticDiffIn{Path: "stuck.hang"})
		done <- err
	}()

	pid := waitForHandlerPID(t, pidFile)
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled call must not report an answer it never got")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("a cancelled call must return rather than wait its handler out")
	}
	waitForHandlerGone(t, pid)
}

// waitForHandlerPID waits for the hanging handler to record the process forge
// spawned for it.
func waitForHandlerPID(t *testing.T, pidFile string) int {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the handler never started")
	return 0
}

// waitForHandlerGone reports whether the handler process is gone. It is a direct
// child of the test binary, so a killed one is reaped by the wait inside the
// handler call and its pid stops resolving.
func waitForHandlerGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("handler %d outlived the call that started it", pid)
}
