package gitrepo

import (
	"os"
	"path/filepath"
	"sort"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

func initRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := gogit.PlainInit(dir, false); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestOpen_NotARepo(t *testing.T) {
	if _, err := Open(t.TempDir()); err == nil {
		t.Fatal("expected error for non-repo dir")
	}
}

func TestOpen_DetectsDotGitFromSubdir(t *testing.T) {
	dir := initRepo(t)
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(sub); err != nil {
		t.Fatalf("Open from subdir: %v", err)
	}
}

func TestChangedFiles(t *testing.T) {
	dir := initRepo(t)
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("tracked.txt", "v1")

	g, _ := gogit.PlainOpen(dir)
	wt, _ := g.Worktree()
	if _, err := wt.Add("tracked.txt"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "t", Email: "t@example.com"}
	if _, err := wt.Commit("init", &gogit.CommitOptions{Author: sig}); err != nil {
		t.Fatal(err)
	}

	r, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.ChangedFiles()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("clean tree should have no changes, got %v", got)
	}

	write("tracked.txt", "v2") // modified
	write("new.txt", "x")      // untracked
	got, err = r.ChangedFiles()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(got)
	if len(got) != 2 || got[0] != "new.txt" || got[1] != "tracked.txt" {
		t.Fatalf("got %v, want [new.txt tracked.txt]", got)
	}
}
