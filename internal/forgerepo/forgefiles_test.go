package forgerepo

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
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

	exts := LoadForgeFormats(repo)
	if !exts[".gltf"] || !exts[".glb"] {
		t.Fatalf("expected .gltf and .glb from legacy file, got %v", exts)
	}
}

func TestLoadForgeFormatsPrefersForgeDir(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge-formats"), ".old\n")
	writeFileT(t, filepath.Join(repo, ".forge", "formats"), ".new\n")

	exts := LoadForgeFormats(repo)
	if !exts[".new"] || exts[".old"] {
		t.Fatalf("expected .forge/formats to win over legacy file, got %v", exts)
	}
}

func TestAddToForgeFormatsMigratesLegacy(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge-formats"), ".gltf\n")

	if err := AddToForgeFormats(repo, ".step"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(filepath.Join(repo, ".forge-formats")); !os.IsNotExist(err) {
		t.Fatal("legacy .forge-formats should have been moved into .forge/")
	}
	exts := LoadForgeFormats(repo)
	if !exts[".gltf"] || !exts[".step"] {
		t.Fatalf("expected migrated content plus new ext, got %v", exts)
	}
}

func TestAddToForgeFormatsCreatesFileInForgeDir(t *testing.T) {
	repo := t.TempDir()

	if err := AddToForgeFormats(repo, ".gltf"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".forge", "formats")); err != nil {
		t.Fatalf("expected .forge/formats to be created: %v", err)
	}
	// Adding the same extension again must be a no-op.
	if err := AddToForgeFormats(repo, ".gltf"); err != nil {
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

	if err := RemoveFromForgeFormats(repo, ".gltf"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(repo, ".forge", "formats"))
	if err != nil {
		t.Fatalf("expected file at .forge/formats after migration: %v", err)
	}
	if string(data) != "# keep me\n.glb\n" {
		t.Fatalf("unexpected content after removal: %q", string(data))
	}

	if err := RemoveFromForgeFormats(repo, ".gltf"); err == nil {
		t.Fatal("expected error removing an extension that is not listed")
	}
}

func TestForgeFormatsIgnoreAndFlip(t *testing.T) {
	repo := t.TempDir()

	// Ignoring a fresh ext records it as ignored, not active.
	if err := IgnoreInForgeFormats(repo, ".tif"); err != nil {
		t.Fatal(err)
	}
	if LoadForgeFormats(repo)[".tif"] {
		t.Fatal(".tif should not be an active format after ignore")
	}
	if !LoadIgnoredFormats(repo)[".tif"] {
		t.Fatal(".tif should be listed as ignored")
	}

	// add flips an ignored ext to included (no contradictory double entry).
	if err := AddToForgeFormats(repo, ".tif"); err != nil {
		t.Fatal(err)
	}
	if !LoadForgeFormats(repo)[".tif"] || LoadIgnoredFormats(repo)[".tif"] {
		t.Fatalf("add should flip .tif to included, got active=%v ignored=%v",
			LoadForgeFormats(repo)[".tif"], LoadIgnoredFormats(repo)[".tif"])
	}
	data, _ := os.ReadFile(filepath.Join(repo, ".forge", "formats"))
	if string(data) != ".tif\n" {
		t.Fatalf("expected a single '.tif' entry after flip, got %q", string(data))
	}

	// ignore flips it back.
	if err := IgnoreInForgeFormats(repo, ".tif"); err != nil {
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

	if err := IgnoreInForgeFormats(repo, ".tif"); err != nil {
		t.Fatal(err)
	}
	active := LoadForgeFormats(repo)
	if !active[".gltf"] || !active[".glb"] || active[".tif"] {
		t.Fatalf("unexpected active set: %v", active)
	}
	// RemoveFromForgeFormats clears an ignored entry too.
	if err := RemoveFromForgeFormats(repo, ".tif"); err != nil {
		t.Fatal(err)
	}
	if LoadIgnoredFormats(repo)[".tif"] {
		t.Fatal(".tif should be gone after remove")
	}
}

// The legacy-layout note is remembered in package state, and these files are
// read on the path of every command and every request. One process answering
// several requests at once reads them from several goroutines, so the remembering
// must be safe: a plain map written from two of them aborts the process, taking
// a whole session with it and returning no error to anyone. Run with -race to
// see the read and the write; without it, the runtime's own concurrent-write
// check is what fires.
func TestPerRepoFilesReadSafelyFromManyGoroutines(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge-formats"), ".unit\n!.ignored\n")
	writeFileT(t, filepath.Join(repo, ".forge-handlers"), `{"unit-stub":"20240115-abc1234"}`)

	// The note fires once per process, and the first burst of readers is the
	// window in which several of them all find it unsaid.
	legacyFileWarned.Delete(".forge-formats")
	legacyFileWarned.Delete(".forge-handlers")

	// Released together, so the readers reach the note at the same moment rather
	// than in the order they were started.
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if !LoadForgeFormats(repo)[".unit"] {
				t.Error("a concurrent read must still see the legacy format list")
			}
			if !LoadIgnoredFormats(repo)[".ignored"] {
				t.Error("a concurrent read must still see the legacy ignore list")
			}
			if pin := LoadForgeHandlers(repo)["unit-stub"]; pin == nil {
				t.Error("a concurrent read must still see the legacy lockfile")
			}
		}()
	}
	close(start)
	wg.Wait()
}

func TestForgeHandlersRoundtripAndMigration(t *testing.T) {
	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge-handlers"), `{"gltf-scene":"20240115-abc1234"}`)

	// Legacy lockfile is readable in place.
	m := LoadForgeHandlers(repo)
	if pin := m["gltf-scene"]; pin == nil || *pin != "20240115-abc1234" {
		t.Fatalf("expected pinned build from legacy lockfile, got %v", m)
	}

	// Saving migrates to .forge/handlers.
	build := "20240201-def5678"
	m["step-cad"] = &build
	if err := SaveForgeHandlers(repo, m); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(repo, ".forge-handlers")); !os.IsNotExist(err) {
		t.Fatal("legacy .forge-handlers should have been moved into .forge/")
	}

	got := LoadForgeHandlers(repo)
	if pin := got["step-cad"]; pin == nil || *pin != build {
		t.Fatalf("expected roundtripped lockfile, got %v", got)
	}
	if pin := got["gltf-scene"]; pin == nil || *pin != "20240115-abc1234" {
		t.Fatalf("expected preserved legacy entry, got %v", got)
	}
}
