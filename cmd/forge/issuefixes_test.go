package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// ── issue #29: forge clone of an empty remote ─────────────────────────────────

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestCloneEmptyRemoteInitializesLocalRepo(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "empty.git")
	gitT(t, tmp, "init", "--bare", bare)

	dest := filepath.Join(tmp, "clone")
	cmd := cloneCmd()
	cmd.SetArgs([]string{bare, dest})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("clone of an empty remote should succeed like git, got: %v", err)
	}

	repo, err := gogit.PlainOpen(dest)
	if err != nil {
		t.Fatalf("expected an initialized repo at %s: %v", dest, err)
	}
	remote, err := repo.Remote("origin")
	if err != nil {
		t.Fatalf("expected an `origin` remote to be configured: %v", err)
	}
	if urls := remote.Config().URLs; len(urls) == 0 || urls[0] != bare {
		t.Fatalf("origin should point at the clone URL, got %v", urls)
	}
}

func TestCloneNonEmptyRemoteStillWorks(t *testing.T) {
	tmp := t.TempDir()
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, tmp, "init", "-b", "main", work)
	gitT(t, work, "config", "user.email", "t@example.com")
	gitT(t, work, "config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, work, "add", "README.md")
	gitT(t, work, "commit", "-m", "init")
	bare := filepath.Join(tmp, "src.git")
	gitT(t, tmp, "clone", "--bare", work, bare)

	dest := filepath.Join(tmp, "clone")
	cmd := cloneCmd()
	cmd.SetArgs([]string{bare, dest})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("clone of a non-empty remote regressed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dest, "README.md")); err != nil {
		t.Fatalf("expected checked-out content: %v", err)
	}
}

// ── issue #30: forge config delegates to git ──────────────────────────────────

func TestRootHasConfigPassthrough(t *testing.T) {
	for _, c := range rootCmd().Commands() {
		if c.Name() == "config" {
			if !c.DisableFlagParsing {
				t.Fatal("config must forward all flags to git (DisableFlagParsing)")
			}
			return
		}
	}
	t.Fatal("rootCmd should register a `config` git pass-through")
}

// ── issue #34: drift detection + the discoverable reconcile command ───────────

func TestMissingHandlerExtsFlagsUninstalledFormats(t *testing.T) {
	// Point HOME at an empty dir so no globally installed handlers leak in.
	t.Setenv("HOME", t.TempDir())

	repo := t.TempDir()
	writeFileT(t, filepath.Join(repo, ".forge", "formats"), ".gltf\n.step\n!.tif\n")

	missing := missingHandlerExts(repo)
	if len(missing) != 2 || missing[0] != ".gltf" || missing[1] != ".step" {
		t.Fatalf("expected sorted missing [.gltf .step] (ignored ext excluded), got %v", missing)
	}

	// No formats listed → no drift to report.
	if got := missingHandlerExts(t.TempDir()); got != nil {
		t.Fatalf("expected nil for a repo with no .forge/formats, got %v", got)
	}
}

func TestFormatsInstallIsRegisteredReconcile(t *testing.T) {
	var install, update bool
	for _, c := range formatsCmd().Commands() {
		switch c.Name() {
		case "install":
			install = true
		case "update":
			update = true
		}
	}
	if !install || !update {
		t.Fatalf("formats should expose both install (reconcile) and update, got install=%v update=%v", install, update)
	}
}
