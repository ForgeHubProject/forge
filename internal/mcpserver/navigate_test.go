package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// cleanMergeHandlerScript is a handler for ".clean" whose merge reconciles both
// sides and reports nothing left to decide — the answer a preview has to be able
// to give, and the one no fixture here gave before.
const cleanMergeHandlerScript = `#!/bin/sh
case "$1" in
merge)
  ` + mergeOursSide + `
  printf '{"blob":"%s"}\n' "$(printf 'merged:%s' "$ours" | base64)"
  ;;
*)
  echo "unimplemented" >&2
  exit 1
  ;;
esac
`

// newLogRepo builds a history worth navigating on top of the standard fixture:
// an opt-in list that opts one extension in and deliberately ignores another
// whose handler is installed, so the three states a path can be in — claimed,
// ignored, claimed by nobody — are all reachable in one commit. The commits after
// it are a straight line, which is what a cursor is walked down.
func newLogRepo(t *testing.T) *server {
	t.Helper()
	root := newRepo(t)

	writeFileT(t, filepath.Join(root, ".forge", "formats"), ".unit\n.silent\n!.echo\n", 0644)
	writeFileT(t, filepath.Join(root, "asset.unit"), "v4", 0644)
	writeFileT(t, filepath.Join(root, "thing.echo"), "e1", 0644)
	writeFileT(t, filepath.Join(root, "notes.txt"), "line1\nline2\nline3\n", 0644)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-m", "three states")

	for i := range 4 {
		writeFileT(t, filepath.Join(root, "notes.txt"), fmt.Sprintf("line1\nline2\nline3\nmore %d\n", i), 0644)
		gitT(t, root, "add", "-A")
		gitT(t, root, "commit", "-m", fmt.Sprintf("note %d", i))
	}
	return &server{root: root}
}

func TestLogMarksOnlyThePathsThisRepositoryHasAHandlerFor(t *testing.T) {
	s := newLogRepo(t)

	_, out, err := s.log(context.Background(), nil, logIn{Ref: "HEAD~4"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Commits) == 0 || out.Commits[0].Subject != "three states" {
		t.Fatalf("log = %+v", out.Commits)
	}
	c := out.Commits[0]
	if got := strings.Join(c.Paths, ","); got != ".forge/formats,asset.unit,notes.txt,thing.echo,untracked.txt" {
		t.Fatalf("paths = %q, want every file the commit changed", got)
	}
	if len(c.HandledPaths) != 1 || c.HandledPaths[0].Path != "asset.unit" {
		t.Fatalf("handledPaths = %+v: only the opted-in extension with an installed handler is claimed", c.HandledPaths)
	}
	// Every semantic payload carries the handler and the build that produced it.
	if c.HandledPaths[0].HandlerID != "unit-stub" || c.HandledPaths[0].HandlerBuild != stubBuild {
		t.Fatalf("handled path = %+v", c.HandledPaths[0])
	}
	// thing.echo has an installed handler and is listed as ignored, which is a
	// decision and not an absence: marking it handled would send an agent to ask a
	// semantic question this repository has said it does not want answered.
	// notes.txt is claimed by nobody at all.
	for _, path := range []string{"thing.echo", "notes.txt", ".forge/formats"} {
		for _, h := range c.HandledPaths {
			if h.Path == path {
				t.Errorf("%s must not be marked handled", path)
			}
		}
	}

	if out.Ref != "HEAD~4" || out.Commit == "" {
		t.Errorf("a response must name the revision walked and what it resolved to: %+v", out)
	}
}

func TestLogNarrowsToOnePathsHistory(t *testing.T) {
	s := newLogRepo(t)

	_, all, err := s.log(context.Background(), nil, logIn{})
	if err != nil {
		t.Fatal(err)
	}
	_, one, err := s.log(context.Background(), nil, logIn{Path: "asset.unit"})
	if err != nil {
		t.Fatal(err)
	}
	if one.Truncated.Total >= all.Truncated.Total {
		t.Fatalf("a path filter must narrow: %d of %d", one.Truncated.Total, all.Truncated.Total)
	}
	for _, c := range one.Commits {
		if !contains(c.Paths, "asset.unit") {
			t.Errorf("commit %s does not touch asset.unit but was listed for it: %+v", c.SHA, c.Paths)
		}
	}

	// A path this history never held is not an error, and the response says which
	// question it answered rather than coming back empty.
	_, none, err := s.log(context.Background(), nil, logIn{Path: "nothing.here"})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Commits) != 0 || none.Truncated.Truncated || !strings.Contains(none.Note, "nothing.here") {
		t.Fatalf("log of an untouched path = %+v", none)
	}
}

// The cap has to be a page and not a wall: following the cursor a truncated
// response names must actually reach the commits it withheld.
func TestLogCursorWalksBackToTheOldestCommit(t *testing.T) {
	s := newLogRepo(t)
	ctx := context.Background()

	_, whole, err := s.log(ctx, nil, logIn{MaxCommits: 100})
	if err != nil {
		t.Fatal(err)
	}
	if whole.Truncated.Truncated || len(whole.Commits) != whole.Truncated.Total {
		t.Fatalf("an uncapped listing must not report itself truncated: %+v", whole.Truncated)
	}

	var walked []string
	ref := ""
	for range whole.Truncated.Total {
		_, page, err := s.log(ctx, nil, logIn{Ref: ref, MaxCommits: 2})
		if err != nil {
			t.Fatalf("continuing from %q: %v", ref, err)
		}
		for _, c := range page.Commits {
			walked = append(walked, c.SHA)
		}
		if !page.Truncated.Truncated {
			break
		}
		if page.Truncated.Total <= page.Truncated.Returned {
			t.Fatalf("a truncated page must report a total above what it returned: %+v", page.Truncated)
		}
		ref = cursorRefIn(t, page.Truncated.Hint)
	}

	var want []string
	for _, c := range whole.Commits {
		want = append(want, c.SHA)
	}
	if strings.Join(walked, ",") != strings.Join(want, ",") {
		t.Fatalf("walking the cursor visited\n%s\nbut the history is\n%s", strings.Join(walked, ","), strings.Join(want, ","))
	}
	if len(want) < 6 {
		t.Fatalf("the fixture must hold enough commits for the cursor to be walked more than once, got %d", len(want))
	}
}

// cursorRefIn pulls the revision a truncation hint offers, so the test follows
// what a caller would follow rather than what it knows the code built.
func cursorRefIn(t *testing.T, hint string) string {
	t.Helper()
	_, rest, ok := strings.Cut(hint, `ref="`)
	if !ok {
		t.Fatalf("a truncated log must name the cursor that continues it: %s", hint)
	}
	ref, _, _ := strings.Cut(rest, `"`)
	if !strings.HasSuffix(ref, "^") {
		t.Fatalf("the cursor should continue from the last commit's first parent, got %q", ref)
	}
	return ref
}

func TestLogRefusesARevisionGitCannotResolve(t *testing.T) {
	s := newLogRepo(t)

	_, _, err := s.log(context.Background(), nil, logIn{Ref: "no-such-branch"})
	if err == nil || !strings.Contains(err.Error(), "not a valid revision: no-such-branch") {
		t.Fatalf("a revision that does not resolve must be refused by name, got %v", err)
	}
}

// A merge is listed, and what git says about its files — nothing — is said
// plainly rather than left to read as a commit that changed nothing.
func TestLogListsAMergeAndSaysWhyItHasNoPaths(t *testing.T) {
	s := newLogRepo(t)
	root := s.root

	gitT(t, root, "checkout", "-q", "-b", "sidelong", "HEAD~2")
	writeFileT(t, filepath.Join(root, "aside.txt"), "aside\n", 0644)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-m", "aside")
	gitT(t, root, "checkout", "-q", "main")
	gitT(t, root, "merge", "--no-ff", "-m", "merge sidelong", "sidelong")

	_, out, err := s.log(context.Background(), nil, logIn{MaxCommits: 3})
	if err != nil {
		t.Fatal(err)
	}
	merge := out.Commits[0]
	if len(merge.Parents) != 2 || merge.Subject != "merge sidelong" {
		t.Fatalf("the merge should head this listing with both parents: %+v", merge)
	}
	if len(merge.Paths) != 0 {
		t.Fatalf("git lists no files for a merge here; reporting some would be inventing them: %+v", merge.Paths)
	}
	if !strings.Contains(out.Note, "first parent") {
		t.Errorf("a listing holding a pathless merge should say why it is pathless: %q", out.Note)
	}
	// The commit after it is an ordinary one and still carries its files.
	if len(out.Commits[1].Paths) == 0 {
		t.Errorf("a non-merge commit must still list what it changed: %+v", out.Commits[1])
	}
}

func TestBranchesMarksTheOneCheckedOutAndTakesTagsAndRemotesOnAsk(t *testing.T) {
	s := newLogRepo(t)
	root := s.root
	ctx := context.Background()

	gitT(t, root, "branch", "feature")
	gitT(t, root, "tag", "-a", "v1", "-m", "first release")
	gitT(t, root, "update-ref", "refs/remotes/origin/main", "refs/heads/main")

	_, out, err := s.branches(ctx, nil, branchesIn{})
	if err != nil {
		t.Fatal(err)
	}
	if out.Current != "main" || out.Detached || out.Head == "" {
		t.Fatalf("branches = %+v", out)
	}
	if len(out.Branches) != 2 || out.Branches[0].Name != "feature" || out.Branches[1].Name != "main" {
		t.Fatalf("local branches, ordered by ref name: %+v", out.Branches)
	}
	if out.Branches[0].Current || !out.Branches[1].Current {
		t.Fatalf("only the branch HEAD is on is current: %+v", out.Branches)
	}
	if out.Branches[1].SHA != out.Head || out.Branches[1].Subject == "" {
		t.Fatalf("a branch must carry its tip and that commit's subject: %+v", out.Branches[1])
	}
	// Neither is offered unasked: an agent that wanted branches gets branches.
	if len(out.Tags) != 0 || len(out.Remotes) != 0 {
		t.Fatalf("tags and remotes are opt-in: %+v", out)
	}

	_, both, err := s.branches(ctx, nil, branchesIn{Tags: true, Remotes: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(both.Tags) != 1 || both.Tags[0].Name != "v1" {
		t.Fatalf("tags = %+v", both.Tags)
	}
	// An annotated tag reports the commit it points at, not the tag object, so the
	// sha it hands back is one the revision parameters here accept.
	if both.Tags[0].SHA != out.Head || both.Tags[0].Subject == "first release" {
		t.Fatalf("an annotated tag must report its commit and that commit's subject: %+v", both.Tags[0])
	}
	if len(both.Remotes) != 1 || both.Remotes[0].Name != "origin/main" || both.Remotes[0].SHA != out.Head {
		t.Fatalf("remotes = %+v", both.Remotes)
	}
	if both.Truncated.Truncated || both.Truncated.Total != 4 {
		t.Fatalf("four refs, none withheld: %+v", both.Truncated)
	}
}

func TestBranchesAnswersARepositoryWithNoCommits(t *testing.T) {
	root := t.TempDir()
	gitT(t, root, "init", "-b", "main", root)
	s := &server{root: root}

	_, out, err := s.branches(context.Background(), nil, branchesIn{Tags: true, Remotes: true})
	if err != nil {
		t.Fatalf("an empty repository is a repository: %v", err)
	}
	if len(out.Branches) != 0 || len(out.Tags) != 0 || len(out.Remotes) != 0 {
		t.Fatalf("nothing is committed, so there are no refs: %+v", out)
	}
	if out.Head != "" || out.Current != "" || out.Detached {
		t.Fatalf("there is no commit to be on, and no branch yet either: %+v", out)
	}
	if !strings.Contains(out.Note, "no commits yet") || !strings.Contains(out.Note, "main") {
		t.Fatalf("the note should name the branch HEAD is waiting on: %q", out.Note)
	}
}

// newPreviewRepo builds two diverged branches touching, on both sides, one path
// per answer a preview can give: a handler that conflicts, a handler that
// reconciles, a handler with no merge at all, a text path git conflicts on and a
// text path git merges.
func newPreviewRepo(t *testing.T) *server {
	t.Helper()
	root := newRepo(t)

	plugins := filepath.Join(os.Getenv("HOME"), ".forge", "plugins")
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-merge"), mergeHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-merge.json"),
		`{"id":"unit-merge","build":"`+mergeBuild+`","source":"https://example.invalid/manifest.toml","formats":[".merge"]}`, 0644)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-clean"), cleanMergeHandlerScript, 0755)
	writeFileT(t, filepath.Join(plugins, "forge-handler-unit-clean.json"),
		`{"id":"unit-clean","build":"nobuild","source":"https://example.invalid/manifest.toml","formats":[".clean"]}`, 0644)
	writeFileT(t, filepath.Join(root, ".forge", "formats"), ".unit\n.merge\n.clean\n", 0644)

	for _, f := range []string{"both.merge", "both.clean", "both.unit"} {
		writeFileT(t, filepath.Join(root, f), "base", 0644)
	}
	writeFileT(t, filepath.Join(root, "clash.txt"), "shared line\n", 0644)
	writeFileT(t, filepath.Join(root, "apart.txt"), "one\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n", 0644)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-m", "preview base")

	gitT(t, root, "checkout", "-q", "-b", "incoming")
	for _, f := range []string{"both.merge", "both.clean", "both.unit"} {
		writeFileT(t, filepath.Join(root, f), "THEIRS", 0644)
	}
	writeFileT(t, filepath.Join(root, "clash.txt"), "their line\n", 0644)
	writeFileT(t, filepath.Join(root, "apart.txt"), "one\ntwo\nthree\nfour\nfive\nsix\nseven\ntheirs\n", 0644)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-m", "incoming side")

	gitT(t, root, "checkout", "-q", "main")
	for _, f := range []string{"both.merge", "both.clean", "both.unit"} {
		writeFileT(t, filepath.Join(root, f), "OURS", 0644)
	}
	writeFileT(t, filepath.Join(root, "clash.txt"), "our line\n", 0644)
	writeFileT(t, filepath.Join(root, "apart.txt"), "ours\ntwo\nthree\nfour\nfive\nsix\nseven\neight\n", 0644)
	gitT(t, root, "add", "-A")
	gitT(t, root, "commit", "-m", "our side")
	return &server{root: root}
}

func previewFileFor(t *testing.T, out mergePreviewOut, path string) mergePreviewFile {
	t.Helper()
	for _, f := range out.Files {
		if f.Path == path {
			return f
		}
	}
	t.Fatalf("%s is not among the previewed paths: %+v", path, out.Files)
	return mergePreviewFile{}
}

func gitDecisionFor(t *testing.T, out mergePreviewOut, path string) gitDecision {
	t.Helper()
	for _, d := range out.GitDecides {
		if d.Path == path {
			return d
		}
	}
	t.Fatalf("%s is not among the paths left to git: %+v", path, out.GitDecides)
	return gitDecision{}
}

func TestMergePreviewReportsEachAnswerAsItsOwn(t *testing.T) {
	s := newPreviewRepo(t)

	_, out, err := s.mergePreview(context.Background(), nil, mergePreviewIn{Base: "main", Head: "incoming"})
	if err != nil {
		t.Fatal(err)
	}
	if out.Relation != "diverged" || out.MergeBase == "" || out.Mergeable != "conflicts" {
		t.Fatalf("preview = relation %q, mergeBase %q, mergeable %q", out.Relation, out.MergeBase, out.Mergeable)
	}
	if out.Comparison.Base != "main" || out.Comparison.Head != "incoming" {
		t.Fatalf("the two sides must be echoed as they were named: %+v", out.Comparison)
	}

	// The handler's conflicts, verbatim, at the addresses forge_resolve_conflict
	// takes — this is the answer no other server can give.
	conflicted := previewFileFor(t, out, "both.merge")
	if conflicted.Clean || conflicted.Total != 2 || len(conflicted.Conflicts) != 2 {
		t.Fatalf("both.merge = %+v", conflicted)
	}
	if conflicted.HandlerID != "unit-merge" || conflicted.HandlerBuild != mergeBuild {
		t.Fatalf("every semantic payload carries handler and build: %+v", conflicted)
	}
	for i, want := range []semanticConflict{
		{Path: "alpha", Ours: "a-ours", Theirs: "a-theirs"},
		{Path: "beta", Ours: "b-ours", Theirs: "b-theirs"},
	} {
		got := conflicted.Conflicts[i]
		if got.Path != want.Path || got.Ours != want.Ours || got.Theirs != want.Theirs {
			t.Errorf("conflict %d = %+v, want %+v", i, got, want)
		}
	}

	if clean := previewFileFor(t, out, "both.clean"); !clean.Clean || clean.Total != 0 || len(clean.Conflicts) != 0 {
		t.Fatalf("a handler that reconciles both sides must be reported clean: %+v", clean)
	}

	// A handler with no merge call is reported saying so, in its own words, and
	// its path is not quietly counted as clean.
	unsupported := previewFileFor(t, out, "both.unit")
	if unsupported.Clean || unsupported.Total != 0 {
		t.Fatalf("a handler that did not merge is not a clean merge: %+v", unsupported)
	}
	if !strings.Contains(unsupported.Note, "unit-stub") || !strings.Contains(unsupported.Note, "unknown subcommand") {
		t.Fatalf("the handler's own refusal should be surfaced: %q", unsupported.Note)
	}

	// Paths no handler claims are git's, and the response says which of them git
	// would stop on rather than lumping them together.
	if got := gitDecisionFor(t, out, "clash.txt").Outcome; got != "conflict" && got != "notPreviewed" {
		t.Fatalf("clash.txt outcome = %q", got)
	}
	if got := gitDecisionFor(t, out, "apart.txt").Outcome; got != "clean" && got != "notPreviewed" {
		t.Fatalf("apart.txt outcome = %q", got)
	}
	if !strings.Contains(out.Note, "gitDecides") {
		t.Errorf("a response leaving paths to git should say so: %q", out.Note)
	}
	// A path only one side changed needs no merge, so it is not listed at all.
	for _, f := range out.Files {
		if f.Path == "asset.unit" {
			t.Errorf("asset.unit changed on neither side since the merge base and must not be previewed")
		}
	}
	if out.Truncated.Truncated || out.Truncated.Total != 5 {
		t.Fatalf("five paths changed on both sides: %+v", out.Truncated)
	}
}

// The git installed here does merge-tree, so the text paths get a real answer
// rather than the honest shrug. That is worth asserting separately: the shrug is
// a fallback, and a fallback that quietly became the only path would look exactly
// like this test passing on the outcomes above.
func TestMergePreviewLetsGitDecideThePathsForgeCannot(t *testing.T) {
	s := newPreviewRepo(t)

	_, out, err := s.mergePreview(context.Background(), nil, mergePreviewIn{Base: "main", Head: "incoming"})
	if err != nil {
		t.Fatal(err)
	}
	clash, apart := gitDecisionFor(t, out, "clash.txt"), gitDecisionFor(t, out, "apart.txt")
	if clash.Outcome == "notPreviewed" || apart.Outcome == "notPreviewed" {
		t.Skipf("the git installed here cannot be asked: %q", clash.Note)
	}
	if clash.Outcome != "conflict" {
		t.Errorf("both sides rewrote the same line of clash.txt: %+v", clash)
	}
	if apart.Outcome != "clean" {
		t.Errorf("the two sides changed opposite ends of apart.txt: %+v", apart)
	}
}

func TestMergePreviewAnswersTheEdgesHonestly(t *testing.T) {
	s := newPreviewRepo(t)
	ctx := context.Background()

	_, same, err := s.mergePreview(ctx, nil, mergePreviewIn{Base: "main", Head: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if same.Relation != "identical" || same.Mergeable != "clean" || len(same.Files) != 0 {
		t.Fatalf("one revision against itself = %+v", same)
	}

	// Fast-forward, named in both directions, because which way it goes is the
	// whole content of the answer.
	_, forward, err := s.mergePreview(ctx, nil, mergePreviewIn{Base: "main~1", Head: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if forward.Relation != "fast-forward" || forward.Mergeable != "clean" {
		t.Fatalf("an ancestor merging in its descendant is a fast-forward: %+v", forward)
	}
	_, back, err := s.mergePreview(ctx, nil, mergePreviewIn{Base: "main", Head: "main~1"})
	if err != nil {
		t.Fatal(err)
	}
	if back.Relation != "contained" || back.Mergeable != "clean" {
		t.Fatalf("merging in a commit already contained changes nothing: %+v", back)
	}
	if !strings.Contains(back.Note, "already an ancestor") {
		t.Errorf("the note should say which side contains which: %q", back.Note)
	}

	// No common ancestor is not a conflict and not a clean merge; it is neither,
	// and saying so is the answer.
	gitT(t, s.root, "checkout", "-q", "--orphan", "lone")
	gitT(t, s.root, "rm", "-q", "-rf", ".")
	writeFileT(t, filepath.Join(s.root, "alone.txt"), "alone\n", 0644)
	gitT(t, s.root, "add", "-A")
	gitT(t, s.root, "commit", "-m", "unrelated root")
	gitT(t, s.root, "checkout", "-q", "main")

	_, unrelated, err := s.mergePreview(ctx, nil, mergePreviewIn{Base: "main", Head: "lone"})
	if err != nil {
		t.Fatal(err)
	}
	if unrelated.Relation != "unrelated" || unrelated.Mergeable != "unknown" || unrelated.MergeBase != "" {
		t.Fatalf("two histories sharing no commit = %+v", unrelated)
	}
	if !strings.Contains(unrelated.Note, "no common ancestor") {
		t.Errorf("the note should name what is missing: %q", unrelated.Note)
	}

	_, _, err = s.mergePreview(ctx, nil, mergePreviewIn{Base: "main", Head: "no-such-thing"})
	if err == nil || !strings.Contains(err.Error(), "not a valid revision: no-such-thing") {
		t.Fatalf("a revision that does not resolve must be refused by name, got %v", err)
	}
}

// The promise the read-only annotation makes is that this call may be run
// without asking. That promise is only worth what it can be shown to be worth,
// so it is measured rather than asserted: every byte of the repository, the index
// among them, before and after.
func TestMergePreviewLeavesTheRepositoryByteForByte(t *testing.T) {
	s := newPreviewRepo(t)

	before := snapshotTree(t, s.root)
	indexBefore := indexStat(t, s.root)

	_, out, err := s.mergePreview(context.Background(), nil, mergePreviewIn{Base: "main", Head: "incoming"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Files) == 0 || len(out.GitDecides) == 0 {
		t.Fatalf("the call under test has to have done the work: %+v", out)
	}

	after := snapshotTree(t, s.root)
	for path, sum := range after {
		switch was, existed := before[path]; {
		case !existed:
			t.Errorf("%s did not exist before this call and does now", path)
		case was != sum:
			t.Errorf("%s changed", path)
		}
	}
	for path := range before {
		if _, still := after[path]; !still {
			t.Errorf("%s existed before this call and does not now", path)
		}
	}
	if got := indexStat(t, s.root); got != indexBefore {
		t.Errorf("the index was touched: %s, was %s", got, indexBefore)
	}
}

// snapshotTree hashes every file under a root, the repository's own .git
// included: an object written into it is a write, whatever else is left alone.
func snapshotTree(t *testing.T, root string) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			// A link to nowhere is content in its own right and hashes as its target.
			target, linkErr := os.Readlink(path)
			if linkErr != nil {
				return err
			}
			data = []byte(target)
		}
		sum := sha256.Sum256(data)
		tree[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return tree
}

// indexStat is the index's own modification time and size, which git rewrites
// whenever it refreshes one — a change the content hash alone would miss when the
// refresh happens to land on the same bytes.
func indexStat(t *testing.T, root string) string {
	t.Helper()
	info, err := os.Stat(filepath.Join(root, ".git", "index"))
	if err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("%d bytes at %s", info.Size(), info.ModTime().Format("2006-01-02T15:04:05.000000000"))
}

// The three tools have to work over the protocol as well as in Go: schema
// validation of the arguments is part of what a client actually exercises.
func TestSessionAnswersTheNavigationToolsOverTheWire(t *testing.T) {
	s := newPreviewRepo(t)
	session := connect(t, s.root)

	var log logOut
	call(t, session, "forge_log", map[string]any{"max_commits": 2}, &log)
	if len(log.Commits) != 2 || log.Commits[0].Subject != "our side" || !log.Truncated.Truncated {
		t.Fatalf("log = %+v", log)
	}
	if len(log.Commits[0].HandledPaths) == 0 {
		t.Fatalf("the head commit changed handled paths and should say so: %+v", log.Commits[0])
	}

	var branches branchesOut
	raw := call(t, session, "forge_branches", map[string]any{"tags": true}, &branches)
	if len(branches.Branches) != 2 || branches.Current != "main" {
		t.Fatalf("branches = %+v", branches)
	}
	// The schema declares these arrays, so an empty one crosses the wire as an
	// array and not as null, for a client that validates what it is handed.
	if !strings.Contains(raw, `"tags":[]`) {
		t.Fatalf("expected an empty tags array on the wire: %s", raw)
	}

	var preview mergePreviewOut
	call(t, session, "forge_merge_preview", map[string]any{"base": "main", "head": "incoming"}, &preview)
	if preview.Mergeable != "conflicts" || len(preview.Files) != 3 {
		t.Fatalf("preview = %+v", preview)
	}
}

// A capped response is never a dead end, in these tools as in the rest.
func TestNavigationCapsCarryACursorThatMoves(t *testing.T) {
	s := newPreviewRepo(t)
	ctx := context.Background()

	_, first, err := s.branches(ctx, nil, branchesIn{MaxRefs: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Truncated.Truncated || len(first.Branches) != 1 {
		t.Fatalf("branches capped at one = %+v", first)
	}
	_, next, err := s.branches(ctx, nil, branchesIn{MaxRefs: 1, After: first.Branches[0].Ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Branches) != 1 || next.Branches[0].Ref == first.Branches[0].Ref {
		t.Fatalf("after must continue past the ref it names: %+v", next.Branches)
	}

	_, page, err := s.mergePreview(ctx, nil, mergePreviewIn{Base: "main", Head: "incoming", MaxPaths: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !page.Truncated.Truncated || page.Truncated.Returned != 2 || page.Truncated.Total != 5 {
		t.Fatalf("preview capped at two = %+v", page.Truncated)
	}
	listed := previewPaths(page)
	if len(listed) != 2 {
		t.Fatalf("a capped preview must list what it counted: %v", listed)
	}
	_, rest, err := s.mergePreview(ctx, nil, mergePreviewIn{Base: "main", Head: "incoming", After: listed[1]})
	if err != nil {
		t.Fatal(err)
	}
	if got := previewPaths(rest); len(got) != 3 || contains(got, listed[1]) {
		t.Fatalf("after must continue past the path it names: %v after %v", got, listed)
	}
}

// previewPaths is every path a preview reported, in the order the response
// counted them.
func previewPaths(out mergePreviewOut) []string {
	var paths []string
	for _, f := range out.Files {
		paths = append(paths, f.Path)
	}
	for _, d := range out.GitDecides {
		paths = append(paths, d.Path)
	}
	sort.Strings(paths)
	return paths
}
