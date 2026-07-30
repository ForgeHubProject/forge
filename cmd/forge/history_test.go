package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

// ── issue #46: semantic diffs across history ──────────────────────────────────

// stubHandlerScript is a handler binary that echoes back the two blobs it was
// handed, base64 as it received them, so a test can assert exactly which
// version of a file forge read without depending on any real format. An absent
// side arrives as an empty blob, which the stub reports as a wholly added or
// wholly removed file — the semantics real handlers give it.
const stubHandlerScript = `#!/bin/sh
input=$(cat)
base=$(printf '%s' "$input" | sed -n 's/.*"base":"\([^"]*\)".*/\1/p')
head=$(printf '%s' "$input" | sed -n 's/.*"head":"\([^"]*\)".*/\1/p')
kind=modified
if [ -z "$base" ]; then kind=added; fi
if [ -z "$head" ]; then kind=removed; fi
printf '{"version":"1.0","format":"unit-stub","changes":[{"path":"payload","kind":"%s","label":"payload","before":"%s","after":"%s"}]}\n' "$kind" "$base" "$head"
`

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

// installStubHandler puts a handler for ".unit" under a temp HOME, so the
// registry resolves it exactly as it resolves a downloaded one.
func installStubHandler(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	plugins := filepath.Join(home, ".forge", "plugins")
	if err := os.MkdirAll(plugins, 0755); err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(plugins, "forge-handler-unit-stub")
	if err := os.WriteFile(binary, []byte(stubHandlerScript), 0755); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, binary+".json", `{"id":"unit-stub","build":"test","formats":[".unit"]}`)
}

// newHistoryRepo builds a repo with two commits over one handled file and one
// plain file, plus a file added and a file deleted by the second commit, and
// leaves an uncommitted third version of the handled file in the working tree.
// It returns the repo root and chdirs into it, as the commands expect.
func newHistoryRepo(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub handler is a POSIX shell script")
	}
	installStubHandler(t)

	repo := t.TempDir()
	gitT(t, repo, "init", "-b", "main", repo)
	gitT(t, repo, "config", "user.email", "t@example.com")
	gitT(t, repo, "config", "user.name", "t")

	writeFileT(t, filepath.Join(repo, ".forge", "formats"), ".unit\n")
	writeFileT(t, filepath.Join(repo, "asset.unit"), "v1")
	writeFileT(t, filepath.Join(repo, "removed.unit"), "gone")
	writeFileT(t, filepath.Join(repo, "notes.txt"), "line1\n")
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-m", "one")

	writeFileT(t, filepath.Join(repo, "asset.unit"), "v2")
	writeFileT(t, filepath.Join(repo, "added.unit"), "new")
	writeFileT(t, filepath.Join(repo, "notes.txt"), "line1\nline2\n")
	if err := os.Remove(filepath.Join(repo, "removed.unit")); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "-A")
	gitT(t, repo, "commit", "-m", "two")

	writeFileT(t, filepath.Join(repo, "asset.unit"), "v3")

	t.Chdir(repo)
	return repo
}

// newEmptyRepo builds a repository with no commits: the handler is installed and
// one handled file is on disk, but there is no HEAD for anything to compare
// against. It returns the repo root and chdirs into it.
func newEmptyRepo(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stub handler is a POSIX shell script")
	}
	installStubHandler(t)

	repo := t.TempDir()
	gitT(t, repo, "init", "-b", "main", repo)
	writeFileT(t, filepath.Join(repo, ".forge", "formats"), ".unit\n")
	writeFileT(t, filepath.Join(repo, "asset.unit"), "v1")

	t.Chdir(repo)
	return repo
}

// runForge executes one command in the current directory and captures
// everything it writes, including the output of the git subprocesses it
// delegates to.
func runForge(t *testing.T, cmd *cobra.Command, args ...string) (string, error) {
	t.Helper()
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = f, f
	cmd.SetArgs(args)
	cmd.SetOut(f)
	cmd.SetErr(f)
	runErr := cmd.Execute()
	os.Stdout, os.Stderr = stdout, stderr
	f.Close()

	out, err := os.ReadFile(f.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(out), runErr
}

func mustContain(t *testing.T, out string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q; got:\n%s", w, out)
		}
	}
}

func mustNotContain(t *testing.T, out string, unwanted ...string) {
	t.Helper()
	for _, u := range unwanted {
		if strings.Contains(out, u) {
			t.Errorf("output should not contain %q; got:\n%s", u, out)
		}
	}
}

func TestDiffRevisionMatrix(t *testing.T) {
	newHistoryRepo(t)

	cases := []struct {
		name       string
		args       []string
		base, head string
	}{
		{"working tree against HEAD", []string{"asset.unit"}, "v2", "v3"},
		{"working tree against a revision", []string{"HEAD~1", "asset.unit"}, "v1", "v3"},
		{"revision to revision", []string{"HEAD~1", "HEAD", "asset.unit"}, "v1", "v2"},
		{"two-dot range", []string{"HEAD~1..HEAD", "asset.unit"}, "v1", "v2"},
		{"explicit path separator", []string{"HEAD~1", "HEAD", "--", "asset.unit"}, "v1", "v2"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, err := runForge(t, diffCmd(), c.args...)
			if err != nil {
				t.Fatalf("forge diff %v: %v\n%s", c.args, err, out)
			}
			mustContain(t, out, "payload", b64(c.base), b64(c.head))
		})
	}
}

func TestDiffFileAbsentAtOneSide(t *testing.T) {
	newHistoryRepo(t)

	// Added by the head revision: the handler sees an empty base, so the file
	// reads as wholly added.
	out, err := runForge(t, diffCmd(), "HEAD~1", "HEAD", "added.unit")
	if err != nil {
		t.Fatalf("forge diff: %v\n%s", err, out)
	}
	mustContain(t, out, "+ [payload] "+b64("new"))

	// Deleted by the head revision: an empty head, so wholly removed.
	out, err = runForge(t, diffCmd(), "HEAD~1", "HEAD", "removed.unit")
	if err != nil {
		t.Fatalf("forge diff: %v\n%s", err, out)
	}
	mustContain(t, out, "- [payload] "+b64("gone"))
}

// A path neither side knows is reported for that path alone: the rest of the
// command still runs.
func TestDiffFileAbsentAtBothSidesReportsAndContinues(t *testing.T) {
	newHistoryRepo(t)

	out, err := runForge(t, diffCmd(), "HEAD~1", "HEAD", "ghost.unit", "asset.unit")
	if err != nil {
		t.Fatalf("an unknown path must not fail the command: %v\n%s", err, out)
	}
	mustContain(t, out, "ghost.unit: not in HEAD~1 or HEAD", b64("v1"), b64("v2"))
}

func TestDiffRejectsPathsOutsideTheRepository(t *testing.T) {
	newHistoryRepo(t)

	for _, arg := range []string{"/etc/passwd", "../elsewhere.unit"} {
		out, err := runForge(t, diffCmd(), "--", arg)
		if err == nil {
			t.Fatalf("%s should be refused; got:\n%s", arg, out)
		}
		if !strings.Contains(err.Error(), "outside the repository") {
			t.Fatalf("unexpected error for %s: %v", arg, err)
		}
	}
}

// Every shape of unresolvable revision is refused by name, not just the ones
// carrying revision syntax or a bare object id: a misspelt branch read as a path
// would compare the wrong pair of sources and still exit zero.
func TestDiffBadRevisionNamesIt(t *testing.T) {
	repo := newHistoryRepo(t)
	gitT(t, repo, "branch", "basex")

	cases := []struct {
		args []string
		bad  string
	}{
		{[]string{"HEAD~99"}, "HEAD~99"},
		{[]string{"deadbeef1234567"}, "deadbeef1234567"},
		{[]string{"basxe"}, "basxe"},               // a misspelt branch
		{[]string{"nosuchref123"}, "nosuchref123"}, // ... whatever it is made of
		{[]string{"basxe", "asset.unit"}, "basxe"}, // ... with a path after it
		{[]string{"HEAD~1", "basxe"}, "basxe"},     // ... on the head side
		{[]string{"basxe..HEAD"}, "basxe..HEAD"},   // ... or inside a range
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			out, err := runForge(t, diffCmd(), c.args...)
			if err == nil || !strings.Contains(err.Error(), "not a valid revision: "+c.bad) {
				t.Fatalf("expected a clean error naming %s, got: %v\n%s", c.bad, err, out)
			}
			// It must refuse instead of comparing some other pair of sources.
			mustNotContain(t, out, "payload", "--- a/")
		})
	}
}

// A path the working tree does not have — deleted, or only ever in history — is
// indistinguishable from a misspelt revision, so forge refuses it as git does
// and the separator is how the caller says a path was meant.
func TestDiffAbsentPathNeedsSeparator(t *testing.T) {
	newHistoryRepo(t)

	_, err := runForge(t, diffCmd(), "removed.unit")
	if err == nil {
		t.Fatal("a path the working tree does not have must be refused without \"--\"")
	}
	mustContain(t, err.Error(), "not a valid revision: removed.unit", "--")

	out, err := runForge(t, diffCmd(), "HEAD~1", "--", "removed.unit")
	if err != nil {
		t.Fatalf("forge diff HEAD~1 -- removed.unit: %v\n%s", err, out)
	}
	mustContain(t, out, "- [payload] "+b64("gone"))
}

// A repository with no commits has no HEAD, so a revision argument cannot
// resolve there either and is refused rather than quietly reread as a path. What
// forge does compare against is named for what it is, since that name is printed.
func TestDiffEmptyRepository(t *testing.T) {
	newEmptyRepo(t)

	_, err := runForge(t, diffCmd(), "HEAD")
	if err == nil || !strings.Contains(err.Error(), "not a valid revision: HEAD") {
		t.Fatalf("expected HEAD to be refused where there are no commits, got: %v", err)
	}

	// With nothing to compare against, the working tree reads as wholly added.
	out, err := runForge(t, diffCmd())
	if err != nil {
		t.Fatalf("forge diff: %v\n%s", err, out)
	}
	mustContain(t, out, "+ [payload] "+b64("v1"))

	out, err = runForge(t, diffCmd(), "--", "ghost.unit")
	if err != nil {
		t.Fatalf("forge diff -- ghost.unit: %v\n%s", err, out)
	}
	mustContain(t, out, "ghost.unit: not in an empty repository or the working tree")
	mustNotContain(t, out, "nothing")
}

// An argument that is both a branch and a file can only be disambiguated by the
// caller, so forge refuses it exactly as git does.
func TestDiffAmbiguousArgumentDemandsSeparator(t *testing.T) {
	repo := newHistoryRepo(t)
	gitT(t, repo, "branch", "asset.unit")

	_, err := runForge(t, diffCmd(), "asset.unit")
	if err == nil {
		t.Fatal("an argument that is both a revision and a file must be refused")
	}
	mustContain(t, err.Error(), "ambiguous argument", "--")

	// With the separator it is unambiguously a path.
	out, err := runForge(t, diffCmd(), "--", "asset.unit")
	if err != nil {
		t.Fatalf("forge diff -- asset.unit: %v\n%s", err, out)
	}
	mustContain(t, out, "payload", b64("v3"))
}

// A path no handler claims still diffs — through git's text diff, so mixed
// revisions stay useful rather than silently partial.
func TestDiffFallsBackToTextForUnhandledPath(t *testing.T) {
	newHistoryRepo(t)

	out, err := runForge(t, diffCmd(), "HEAD~1", "HEAD", "notes.txt")
	if err != nil {
		t.Fatalf("forge diff: %v\n%s", err, out)
	}
	mustContain(t, out, "--- a/notes.txt", "+line2")
	mustNotContain(t, out, "payload")
}

func TestDiffWebAcceptsRevisions(t *testing.T) {
	newHistoryRepo(t)

	// No renderer is installed under the temp HOME, so the run stops at renderer
	// resolution — after the revisions resolved and the diff was computed, which
	// is what this asserts.
	_, err := runForge(t, diffCmd(), "--web", "--no-open", "HEAD~1", "HEAD", "asset.unit")
	if err == nil || !strings.Contains(err.Error(), "renderer") {
		t.Fatalf("expected --web to get as far as renderer resolution, got: %v", err)
	}

	_, err = runForge(t, diffCmd(), "--web", "--no-open", "HEAD~1", "HEAD")
	if err == nil || !strings.Contains(err.Error(), "exactly one file") {
		t.Fatalf("--web with no path should say it needs one file, got: %v", err)
	}
}

func TestShowMultiFileCommit(t *testing.T) {
	repo := newHistoryRepo(t)
	head := gitOutputT(t, repo, "rev-parse", "HEAD")

	out, err := runForge(t, showCmd(), "HEAD")
	if err != nil {
		t.Fatalf("forge show HEAD: %v\n%s", err, out)
	}
	mustContain(t, out,
		head,              // the commit's own header, as git prints it
		"4 files changed", // asset/added/removed .unit plus notes.txt
		b64("v1"),         // the handled file, compared against the first parent
		b64("v2"),
		"+ [payload] "+b64("new"),
		"- [payload] "+b64("gone"),
		"notes.txt",
		"+1 -0", // the plain file is summarised, not omitted
	)
}

func TestShowRootCommit(t *testing.T) {
	repo := newHistoryRepo(t)
	root := gitOutputT(t, repo, "rev-list", "--max-parents=0", "HEAD")

	out, err := runForge(t, showCmd(), root)
	if err != nil {
		t.Fatalf("forge show <root>: %v\n%s", err, out)
	}
	// A root commit has no parent to compare against, so everything it
	// introduced reads as added.
	mustContain(t, out, "+ [payload] "+b64("v1"), "+ [payload] "+b64("gone"), "notes.txt")
}

func TestShowFiltersByPath(t *testing.T) {
	newHistoryRepo(t)

	out, err := runForge(t, showCmd(), "HEAD", "--", "notes.txt")
	if err != nil {
		t.Fatalf("forge show: %v\n%s", err, out)
	}
	mustContain(t, out, "1 file changed", "notes.txt")
	mustNotContain(t, out, "payload", "added.unit")
}

func TestShowBadRevisionNamesIt(t *testing.T) {
	newHistoryRepo(t)

	_, err := runForge(t, showCmd(), "no-such-rev")
	if err == nil || !strings.Contains(err.Error(), "not a valid revision: no-such-rev") {
		t.Fatalf("expected a clean error naming the revision, got: %v", err)
	}
}

// The per-repo opt-in decides which formats have a handler, and history is no
// exception: an extension the repo has not listed is summarised as text.
func TestShowRespectsRepoFormatOptIn(t *testing.T) {
	repo := newHistoryRepo(t)
	writeFileT(t, filepath.Join(repo, ".forge", "formats"), ".other\n")

	out, err := runForge(t, showCmd(), "HEAD")
	if err != nil {
		t.Fatalf("forge show: %v\n%s", err, out)
	}
	mustContain(t, out, "asset.unit")
	mustNotContain(t, out, "payload")
}

func TestSplitRevsAndPaths(t *testing.T) {
	repo := newHistoryRepo(t)
	gitT(t, repo, "branch", "asset.unit")

	cases := []struct {
		name        string
		args        []string
		dashAt      int
		revs, paths []string
	}{
		{"no arguments", nil, -1, nil, nil},
		{"one revision", []string{"HEAD"}, -1, []string{"HEAD"}, nil},
		{"two revisions", []string{"HEAD~1", "HEAD"}, -1, []string{"HEAD~1", "HEAD"}, nil},
		{"revision then path", []string{"HEAD", "notes.txt"}, -1, []string{"HEAD"}, []string{"notes.txt"}},
		{"path only", []string{"notes.txt"}, -1, nil, []string{"notes.txt"}},
		{"separator overrides the guess", []string{"HEAD", "asset.unit"}, 1, []string{"HEAD"}, []string{"asset.unit"}},
		{"everything after the separator is a path", []string{"HEAD"}, 0, nil, []string{"HEAD"}},
		// Once no further revision can be meant, a path no side holds is kept
		// rather than refused, so the caller reports it absent.
		{"absent path after the separator", []string{"ghost.unit"}, 0, nil, []string{"ghost.unit"}},
		{"absent path after two revisions", []string{"HEAD~1", "HEAD", "ghost.unit"}, -1,
			[]string{"HEAD~1", "HEAD"}, []string{"ghost.unit"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			revs, paths, err := splitRevsAndPaths(repo, c.args, c.dashAt)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if strings.Join(revs, ",") != strings.Join(c.revs, ",") {
				t.Errorf("revs = %v, want %v", revs, c.revs)
			}
			if strings.Join(paths, ",") != strings.Join(c.paths, ",") {
				t.Errorf("paths = %v, want %v", paths, c.paths)
			}
		})
	}

	// While a revision is still possible, an argument that is not one and is not
	// a file either can only be a mistake, and saying which it was is the
	// caller's to do.
	for _, c := range []struct {
		name string
		args []string
	}{
		{"both a revision and a file", []string{"asset.unit"}},
		{"neither a revision nor a file", []string{"no-such-name"}},
		{"a bad head revision", []string{"HEAD~1", "no-such-name"}},
	} {
		if revs, paths, err := splitRevsAndPaths(repo, c.args, -1); err == nil {
			t.Errorf("%s: %v must be refused, got revs %v paths %v", c.name, c.args, revs, paths)
		}
	}
}

func TestRepoRelPath(t *testing.T) {
	repo := newHistoryRepo(t)

	for _, c := range []struct{ in, want string }{
		{"asset.unit", "asset.unit"},
		{"./asset.unit", "asset.unit"},
		{filepath.Join(repo, "sub", "asset.unit"), "sub/asset.unit"},
	} {
		got, err := repoRelPath(repo, c.in)
		if err != nil {
			t.Fatalf("repoRelPath(%q): %v", c.in, err)
		}
		if got != c.want {
			t.Errorf("repoRelPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}

	for _, in := range []string{"../escape.unit", "sub/../../escape.unit", "/etc/passwd"} {
		if got, err := repoRelPath(repo, in); err == nil {
			t.Errorf("repoRelPath(%q) = %q, want a refusal", in, got)
		}
	}
}

// gitOutputT returns the trimmed stdout of a git command, failing the test if
// git does.
func gitOutputT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := gitOutput(dir, args...)
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return strings.TrimSpace(string(out))
}
