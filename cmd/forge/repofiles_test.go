package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/forgehubproject/forge/internal/fhr"
)

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestLoadForgeFormatsLegacyFallback(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge-formats"), "# comment\n.gltf\nglb\n")

	exts := loadForgeFormats(repo)
	if !exts[".gltf"] || !exts[".glb"] {
		t.Fatalf("expected .gltf and .glb from legacy file, got %v", exts)
	}
}

func TestLoadForgeFormatsPrefersForgeDir(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge-formats"), ".old\n")
	writeFileT(t, filepath.Join(repo, ".forge", "formats"), ".new\n")

	exts := loadForgeFormats(repo)
	if !exts[".new"] || exts[".old"] {
		t.Fatalf("expected .forge/formats to win over legacy file, got %v", exts)
	}
}

func TestAddToForgeFormatsMigratesLegacy(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge-formats"), ".gltf\n")

	if err := addToForgeFormats(repo, ".step"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(repo, ".forge-formats")); !os.IsNotExist(err) {
		t.Fatal("legacy .forge-formats should have been moved into .forge/")
	}
	exts := loadForgeFormats(repo)
	if !exts[".gltf"] || !exts[".step"] {
		t.Fatalf("expected migrated content plus new ext, got %v", exts)
	}
}

func TestAddToForgeFormatsCreatesFileInForgeDir(t *testing.T) {
	repo := t.TempDir()

	if err := addToForgeFormats(repo, ".gltf"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".forge", "formats")); err != nil {
		t.Fatalf("expected .forge/formats to be created: %v", err)
	}
	// Adding the same extension again must be a no-op.
	if err := addToForgeFormats(repo, ".gltf"); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(filepath.Join(repo, ".forge", "formats"))
	if string(data) != ".gltf\n" {
		t.Fatalf("expected single entry, got %q", string(data))
	}
}

func TestRemoveFromForgeFormats(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge-formats"), "# keep me\n.gltf\n.glb\n")

	if err := removeFromForgeFormats(repo, ".gltf"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".forge", "formats"))
	if err != nil {
		t.Fatalf("expected file at .forge/formats after migration: %v", err)
	}
	if string(data) != "# keep me\n.glb\n" {
		t.Fatalf("unexpected content after removal: %q", string(data))
	}

	if err := removeFromForgeFormats(repo, ".gltf"); err == nil {
		t.Fatal("expected error removing an extension that is not listed")
	}
}

func TestForgeFormatsIgnoreAndFlip(t *testing.T) {
	repo := t.TempDir()

	// Ignoring a fresh ext records it as ignored, not active.
	if err := ignoreInForgeFormats(repo, ".tif"); err != nil {
		t.Fatal(err)
	}
	if loadForgeFormats(repo)[".tif"] {
		t.Fatal(".tif should not be an active format after ignore")
	}
	if !loadIgnoredFormats(repo)[".tif"] {
		t.Fatal(".tif should be listed as ignored")
	}

	// add flips an ignored ext to included (no contradictory double entry).
	if err := addToForgeFormats(repo, ".tif"); err != nil {
		t.Fatal(err)
	}
	if !loadForgeFormats(repo)[".tif"] || loadIgnoredFormats(repo)[".tif"] {
		t.Fatalf("add should flip .tif to included, got active=%v ignored=%v",
			loadForgeFormats(repo)[".tif"], loadIgnoredFormats(repo)[".tif"])
	}
	data, _ := os.ReadFile(filepath.Join(repo, ".forge", "formats"))
	if string(data) != ".tif\n" {
		t.Fatalf("expected a single '.tif' entry after flip, got %q", string(data))
	}

	// ignore flips it back.
	if err := ignoreInForgeFormats(repo, ".tif"); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(filepath.Join(repo, ".forge", "formats"))
	if string(data) != "!.tif\n" {
		t.Fatalf("expected '!.tif' after re-ignore, got %q", string(data))
	}
}

func TestForgeFormatsIgnorePreservesIncludedAndComments(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge", "formats"), "# assets\n.gltf\n.glb\n")

	if err := ignoreInForgeFormats(repo, ".tif"); err != nil {
		t.Fatal(err)
	}
	active := loadForgeFormats(repo)
	if !active[".gltf"] || !active[".glb"] || active[".tif"] {
		t.Fatalf("unexpected active set: %v", active)
	}
	// removeFromForgeFormats clears an ignored entry too.
	if err := removeFromForgeFormats(repo, ".tif"); err != nil {
		t.Fatal(err)
	}
	if loadIgnoredFormats(repo)[".tif"] {
		t.Fatal(".tif should be gone after remove")
	}
}

func TestGitUntrackedFilesRespectsGitignoreAndCollapsesDirs(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	writeFileT(t, filepath.Join(repo, ".gitignore"), "ignored.log\nbuild/\n")
	writeFileT(t, filepath.Join(repo, "README.md"), "hi")
	git("add", "README.md", ".gitignore")
	git("commit", "-m", "init")

	writeFileT(t, filepath.Join(repo, "ignored.log"), "noise") // gitignored → excluded
	writeFileT(t, filepath.Join(repo, "notes.txt"), "x")       // untracked file → shown
	writeFileT(t, filepath.Join(repo, "build", "a.o"), "x")    // ignored dir → excluded
	writeFileT(t, filepath.Join(repo, "out", "x.bin"), "x")    // untracked dir → collapses

	set := map[string]bool{}
	for _, p := range gitUntrackedFiles(repo) {
		set[p] = true
	}
	if set["ignored.log"] || set["build/"] {
		t.Fatalf("gitignored entries leaked: %v", set)
	}
	if !set["notes.txt"] {
		t.Fatalf("expected notes.txt in untracked, got %v", set)
	}
	if !set["out/"] {
		t.Fatalf("expected wholly-untracked dir collapsed to out/, got %v", set)
	}
}

func TestResolveSourceSelectors(t *testing.T) {
	sources := []fhr.Source{{Name: "a"}, {Name: "b"}, {Name: "c"}}

	got, err := resolveSourceSelectors(sources, []string{"1", "c"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatalf("index+name mix: got %v", got)
	}

	// Duplicate selectors (index and name for the same source) collapse to one.
	got, _ = resolveSourceSelectors(sources, []string{"2", "b"})
	if len(got) != 1 || got[0] != "b" {
		t.Fatalf("expected dedup to single 'b', got %v", got)
	}

	if _, err := resolveSourceSelectors(sources, []string{"9"}); err == nil {
		t.Fatal("expected out-of-range index to error")
	}
	if _, err := resolveSourceSelectors(sources, []string{"nope"}); err == nil {
		t.Fatal("expected unknown name to error")
	}
}

func TestForgeHandlersRoundtripAndMigration(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge-handlers"), `{"gltf-scene":"20240115-abc1234"}`)

	// Legacy lockfile is readable in place.
	m := loadForgeHandlers(repo)
	if pin := m["gltf-scene"]; pin == nil || *pin != "20240115-abc1234" {
		t.Fatalf("expected pinned build from legacy lockfile, got %v", m)
	}

	// Saving migrates to .forge/handlers.
	build := "20240201-def5678"
	m["step-cad"] = &build
	if err := saveForgeHandlers(repo, m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".forge-handlers")); !os.IsNotExist(err) {
		t.Fatal("legacy .forge-handlers should have been moved into .forge/")
	}

	got := loadForgeHandlers(repo)
	if pin := got["step-cad"]; pin == nil || *pin != build {
		t.Fatalf("expected roundtripped lockfile, got %v", got)
	}
	if pin := got["gltf-scene"]; pin == nil || *pin != "20240115-abc1234" {
		t.Fatalf("expected preserved legacy entry, got %v", got)
	}
}
