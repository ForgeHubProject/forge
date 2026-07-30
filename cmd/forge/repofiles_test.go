package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

func TestDiscoverRepoExtensions(t *testing.T) {
	repo := t.TempDir()
	git := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = repo
		if err := c.Run(); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	git("init")
	for _, f := range []string{"a.glb", "b.GLTF", "sub/c.glb", "readme.md", "Makefile", ".gitignore"} {
		writeFileT(t, filepath.Join(repo, f), "x")
	}
	git("add", "-A")

	got, err := discoverRepoExtensions(repo)
	if err != nil {
		t.Fatal(err)
	}
	// distinct + lower-cased; extension-less (Makefile) and dotfiles (.gitignore) excluded.
	if strings.Join(got, ",") != ".glb,.gltf,.md" {
		t.Fatalf("unexpected extensions: %v", got)
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
