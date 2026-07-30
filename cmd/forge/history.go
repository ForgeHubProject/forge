package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/forgehubproject/forge/internal/handler"
	"github.com/spf13/cobra"
)

// ── comparison sources ──────────────────────────────────────────────────────────────────────────────

// sourceKind distinguishes what a side of a comparison reads from.
type sourceKind int

const (
	sourceWorktree sourceKind = iota // the files on disk
	sourceRevision                   // a commit in history
	sourceEmpty                      // nothing at all — what a root commit's parent is
)

// blobSource is one side of a comparison. rev holds the resolved commit, so a
// ref that moves while the command runs cannot change which bytes are read;
// name is what the caller called it and exists only for output.
type blobSource struct {
	kind sourceKind
	rev  string
	name string
}

func worktreeSource() blobSource { return blobSource{kind: sourceWorktree, name: "the working tree"} }

func revisionSource(name, commit string) blobSource {
	return blobSource{kind: sourceRevision, rev: commit, name: name}
}

func emptySource() blobSource { return blobSource{kind: sourceEmpty, name: "nothing"} }

// emptyRepoSource is the empty side of a comparison in a repository with no
// commits. It differs from emptySource only in its name, which is printed: a
// root commit's parent is fairly called nothing, a whole repository is not.
func emptyRepoSource() blobSource {
	return blobSource{kind: sourceEmpty, name: "an empty repository"}
}

func (s blobSource) String() string { return s.name }

// gitOutput runs git inside the repository and returns its stdout. A failure
// carries git's own message, so callers can report why git refused.
func gitOutput(repoDir string, args ...string) ([]byte, error) {
	c := exec.Command("git", args...)
	c.Dir = repoDir
	out, err := c.Output()
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		if msg := strings.TrimSpace(string(exitErr.Stderr)); msg != "" {
			return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
		}
	}
	return out, err
}

// ── revisions and paths ─────────────────────────────────────────────────────────────────────────────

// resolveRev resolves a revision to the commit it names, the way git resolves it
// (object id, branch, tag, HEAD~2, ...). A revision git will not resolve is
// reported by name.
func resolveRev(repoDir, rev string) (string, error) {
	out, err := gitOutput(repoDir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	commit := strings.TrimSpace(string(out))
	if err != nil || commit == "" {
		return "", fmt.Errorf("not a valid revision: %s", rev)
	}
	return commit, nil
}

func isRevision(repoDir, rev string) bool {
	_, err := resolveRev(repoDir, rev)
	return err == nil
}

// pathExists reports whether an argument names something on disk, which is what
// makes it a path rather than a revision.
func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// splitRevsAndPaths tells revisions from paths the way git does. Everything past
// an explicit "--" (dashAt, from cobra's ArgsLenAtDash) is a path; otherwise
// leading arguments that resolve as revisions are revisions and the rest are
// paths. An argument that is both a revision and an existing file is refused
// exactly as git refuses it, because only "--" can say which was meant.
//
// While a revision is still possible, an argument that is neither a revision git
// resolves nor a file on disk is refused by name, as git refuses it: a mistyped
// revision is indistinguishable from a file that is not there, and reading it as
// a path would compare something other than what was asked for while reporting
// success. Once the paths have begun — after "--", after a path, or after two
// revisions, when no further revision can be meant — a path no side holds is
// kept and reported absent instead.
func splitRevsAndPaths(repoDir string, args []string, dashAt int) (revs, paths []string, err error) {
	if dashAt >= 0 && dashAt <= len(args) {
		return args[:dashAt], args[dashAt:], nil
	}
	i := 0
	for ; i < len(args) && len(revs) < 2; i++ {
		arg := args[i]
		switch {
		case isRevision(repoDir, arg):
			if pathExists(arg) {
				return nil, nil, fmt.Errorf("ambiguous argument %q: both a revision and a file\n"+
					"use \"--\" to separate paths from revisions, like: forge diff <revision> -- %s", arg, arg)
			}
			revs = append(revs, arg)
		case pathExists(arg):
			return revs, args[i:], nil
		default:
			return nil, nil, fmt.Errorf("not a valid revision: %s\n"+
				"if it is a path, the working tree does not have it — put it after \"--\"", arg)
		}
	}
	return revs, args[i:], nil
}

// expandRevRange rewrites git's two-dot range form so `forge diff a..b` means
// what `forge diff a b` means, as it does in git; dashAt moves with the extra
// argument. Anything else — a three-dot range, a file whose name contains ".."
// — is left for the ordinary rules.
func expandRevRange(repoDir string, args []string, dashAt int) ([]string, int) {
	if len(args) == 0 || dashAt == 0 {
		return args, dashAt
	}
	arg := args[0]
	if pathExists(arg) || strings.Contains(arg, "...") {
		return args, dashAt
	}
	base, head, found := strings.Cut(arg, "..")
	if !found || base == "" || head == "" || !isRevision(repoDir, base) || !isRevision(repoDir, head) {
		return args, dashAt
	}
	if dashAt > 0 {
		dashAt++
	}
	return append([]string{base, head}, args[1:]...), dashAt
}

// diffSources maps forge diff's revision arguments onto the two sides being
// compared: none is the working tree against HEAD, one is the working tree
// against that revision, two is revision to revision.
func diffSources(repoDir string, revs []string) (base, head blobSource, err error) {
	switch len(revs) {
	case 0:
		commit, headErr := resolveRev(repoDir, "HEAD")
		if headErr != nil {
			// A repository with no commits has nothing to compare against, so
			// the whole working tree reads as newly added.
			return emptyRepoSource(), worktreeSource(), nil
		}
		return revisionSource("HEAD", commit), worktreeSource(), nil
	case 1:
		commit, err := resolveRev(repoDir, revs[0])
		if err != nil {
			return base, head, err
		}
		return revisionSource(revs[0], commit), worktreeSource(), nil
	default:
		baseCommit, err := resolveRev(repoDir, revs[0])
		if err != nil {
			return base, head, err
		}
		headCommit, err := resolveRev(repoDir, revs[1])
		if err != nil {
			return base, head, err
		}
		return revisionSource(revs[0], baseCommit), revisionSource(revs[1], headCommit), nil
	}
}

// repoRelPath resolves a path argument to a repo-relative, slash-separated path.
// Arguments are relative to where the command was run, as git's are, and one
// that lands outside the repository is refused rather than read.
func repoRelPath(repoDir, p string) (string, error) {
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", p, err)
	}
	roots := []string{repoDir}
	if resolved, err := filepath.EvalSymlinks(repoDir); err == nil && resolved != repoDir {
		roots = append(roots, resolved)
	}
	for _, root := range roots {
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			continue
		}
		return filepath.ToSlash(rel), nil
	}
	return "", fmt.Errorf("%s is outside the repository", p)
}

// repoRelPaths resolves every path argument, refusing the whole command if any
// one of them points outside the repository.
func repoRelPaths(repoDir string, args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		rel, err := repoRelPath(repoDir, a)
		if err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, nil
}

// ── reading and comparing one file ──────────────────────────────────────────────────────────────────

// sourceEntry reports what a source holds at path — "blob", "tree", or "" when
// it holds nothing there.
func sourceEntry(repoDir string, src blobSource, path string) string {
	if path == "." {
		return "tree" // the repository root itself
	}
	switch src.kind {
	case sourceWorktree:
		st, err := os.Stat(filepath.Join(repoDir, filepath.FromSlash(path)))
		switch {
		case err != nil:
			return ""
		case st.IsDir():
			return "tree"
		default:
			return "blob"
		}
	case sourceRevision:
		out, err := gitOutput(repoDir, "cat-file", "-t", src.rev+":"+path)
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(out))
	}
	return ""
}

// blobAt returns path's bytes at one source and whether that source holds the
// path at all. Revision blobs come from git itself — `git show <rev>:<path>`,
// the mechanism the merge path already uses to read index stages.
func blobAt(repoDir string, src blobSource, path string) ([]byte, bool, error) {
	if sourceEntry(repoDir, src, path) != "blob" {
		return nil, false, nil
	}
	if src.kind == sourceWorktree {
		data, err := os.ReadFile(filepath.Join(repoDir, filepath.FromSlash(path)))
		if err != nil {
			return nil, false, fmt.Errorf("reading %s: %w", path, err)
		}
		return data, true, nil
	}
	data, err := gitOutput(repoDir, "show", src.rev+":"+path)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s at %s: %w", path, src.name, err)
	}
	return data, true, nil
}

// fileComparison is one path compared across two sources.
type fileComparison struct {
	Path      string
	HandlerID string // the handler that resolved; "text" for the catch-all
	Semantic  bool   // a format handler claimed the path
	Diff      handler.StructuredDiff
	Base      []byte // nil when the base side holds no such file
	Head      []byte
	BaseFound bool
	HeadFound bool
}

// compareFile produces the semantic diff of one path between two sources, and is
// the single route every mode takes — the working tree against HEAD or any
// revision, revision to revision, forge show, and the --web page — so they all
// compute a change the same way, and so callers outside the CLI can compute it
// identically too. path is repo-relative and slash-separated (repoRelPath).
//
// A path no format handler claims is reported with Semantic false and its bytes
// left unread: every caller renders those through git's own text diff, so
// reading a large file only to discard the read would be waste. A path neither
// source holds is not an error — both Found flags come back false and the caller
// says so for that one file instead of abandoning the rest.
func compareFile(repoDir string, reg *handler.Registry, path string, base, head blobSource) (fileComparison, error) {
	h, err := reg.Resolve(path)
	if err != nil {
		return fileComparison{}, err
	}
	fc := fileComparison{Path: path, HandlerID: handlerFormat(h), Semantic: isBinaryHandler(h)}

	if !fc.Semantic {
		fc.BaseFound = sourceEntry(repoDir, base, path) == "blob"
		fc.HeadFound = sourceEntry(repoDir, head, path) == "blob"
		return fc, nil
	}

	fc.Base, fc.BaseFound, err = blobAt(repoDir, base, path)
	if err != nil {
		return fc, err
	}
	fc.Head, fc.HeadFound, err = blobAt(repoDir, head, path)
	if err != nil {
		return fc, err
	}
	if !fc.BaseFound && !fc.HeadFound {
		return fc, nil
	}
	// An absent side is an empty blob: the handler then reports the file as
	// wholly added or wholly removed, which is what it means.
	fc.Diff, err = h.Diff(fc.Base, fc.Head)
	if err != nil {
		return fc, fmt.Errorf("diff %s: %w", path, err)
	}
	return fc, nil
}

// handlerFormat names the handler that resolved, "unknown" if it does not say.
func handlerFormat(h handler.ForgeHandler) string {
	if n, ok := h.(handler.Namer); ok {
		return n.Format()
	}
	return "unknown"
}

// ── listing what changed ────────────────────────────────────────────────────────────────────────────

// gitDiffArgs builds the git invocation comparing two sources: a revision
// against the working tree, revision against revision, or — where the base is
// nothing, as a root commit's is — the commit against the empty tree.
func gitDiffArgs(base, head blobSource, flags, pathspecs []string) []string {
	var args []string
	switch {
	case base.kind == sourceRevision && head.kind == sourceRevision:
		args = append(append([]string{"diff"}, flags...), base.rev, head.rev)
	case base.kind == sourceRevision:
		args = append(append([]string{"diff"}, flags...), base.rev)
	case head.kind == sourceRevision:
		args = append(append([]string{"diff-tree", "-r", "--root", "--no-commit-id"}, flags...), head.rev)
	default:
		args = append([]string{"diff"}, flags...)
	}
	if len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	return args
}

// changedPaths lists the files that differ between two sources, restricted to
// the given pathspecs. --name-only reports one path per changed file (a rename
// arrives as its destination), so every record is a file to compare.
func changedPaths(repoDir string, base, head blobSource, pathspecs []string) ([]string, error) {
	out, err := gitOutput(repoDir, gitDiffArgs(base, head, []string{"--name-only", "-z"}, pathspecs)...)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, p := range strings.Split(string(out), "\x00") {
		if p != "" {
			paths = append(paths, p)
		}
	}
	return paths, nil
}

// comparePaths resolves path arguments to the files to compare: a file is
// itself, and a directory expands to the files that changed under it. An
// argument neither source holds is kept, so it is reported as absent rather
// than silently dropped.
func comparePaths(repoDir string, base, head blobSource, paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		if sourceEntry(repoDir, base, p) != "tree" && sourceEntry(repoDir, head, p) != "tree" {
			out = append(out, p)
			continue
		}
		under, err := changedPaths(repoDir, base, head, []string{p})
		if err != nil {
			return nil, err
		}
		out = append(out, under...)
	}
	return out, nil
}

// textChangeSummary reports what git counts for a path no handler claims.
func textChangeSummary(repoDir string, base, head blobSource, path string) string {
	out, err := gitOutput(repoDir, gitDiffArgs(base, head, []string{"--numstat"}, []string{path})...)
	if err != nil {
		return "changed"
	}
	fields := strings.Fields(strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0])
	if len(fields) < 2 {
		return "unchanged"
	}
	if fields[0] == "-" || fields[1] == "-" {
		return "contents changed (not line-counted)"
	}
	return fmt.Sprintf("+%s -%s", fields[0], fields[1])
}

// ── forge show ──────────────────────────────────────────────────────────────────────────────────────

func showCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "show <revision> [--] [<path>...]",
		Short: "Show what a commit changed, semantically",
		Long: `Resolves a revision and lists what its commit changed against its first
parent — against nothing, for a root commit.

Paths a format handler claims are shown as a semantic diff; the rest are
summarised with git's own line counts. Paths given filter the listing.`,
		Args: cobra.MinimumNArgs(1),
		RunE: runShow,
	}
}

func runShow(cmd *cobra.Command, args []string) error {
	repoDir := findRepoRoot()
	if _, err := gitOutput(repoDir, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("not a git repository")
	}

	dashAt := cmd.ArgsLenAtDash()
	revs, rawPaths, err := splitRevsAndPaths(repoDir, args, dashAt)
	if err != nil {
		return err
	}
	// The first argument is a revision by definition here, so one git cannot
	// resolve is reported as the bad revision it was meant to be even where a
	// file of that name exists, which is the one reading splitRevsAndPaths
	// prefers and forge show has no use for.
	if len(revs) == 0 && dashAt != 0 {
		return fmt.Errorf("not a valid revision: %s", args[0])
	}
	if len(revs) != 1 {
		return fmt.Errorf("forge show takes exactly one revision")
	}
	commit, err := resolveRev(repoDir, revs[0])
	if err != nil {
		return err
	}
	paths, err := repoRelPaths(repoDir, rawPaths)
	if err != nil {
		return err
	}

	head := revisionSource(revs[0], commit)
	base := emptySource()
	if parent, err := resolveRev(repoDir, commit+"^1"); err == nil {
		base = revisionSource(revs[0]+"^", parent)
	}

	files, err := changedPaths(repoDir, base, head, paths)
	if err != nil {
		return err
	}

	printCommitHeader(repoDir, commit)

	if len(files) == 0 {
		if len(paths) > 0 {
			fmt.Println("this commit changed none of the given paths")
		} else {
			fmt.Println("this commit changed no files")
		}
		return nil
	}
	noun := "files"
	if len(files) == 1 {
		noun = "file"
	}
	fmt.Printf("%d %s changed\n", len(files), noun)

	reg := defaultRegistry()
	for _, p := range files {
		if err := showFile(repoDir, reg, p, base, head); err != nil {
			fmt.Fprintf(os.Stderr, "forge: %s: %v\n", p, err)
		}
	}
	return nil
}

// showFile renders one file's entry in a commit: the handler's change tree where
// there is one, git's line counts otherwise.
func showFile(repoDir string, reg *handler.Registry, path string, base, head blobSource) error {
	fc, err := compareFile(repoDir, reg, path, base, head)
	if err != nil {
		return err
	}
	switch {
	case !fc.BaseFound && !fc.HeadFound:
		fmt.Printf("  %-46s not in %s or %s\n", path, base, head)
	case !fc.Semantic:
		fmt.Printf("  %-46s %s  %s\n", path, textChangeSummary(repoDir, base, head, path), handlerLabel(path, reg))
	case len(fc.Diff.Changes) == 0:
		fmt.Printf("  %-46s no semantic changes  %s\n", path, handlerLabel(path, reg))
	default:
		renderDiff(path, fc.Diff)
	}
	return nil
}

// printCommitHeader prints the commit's own header the way git formats it, so
// forge show opens as git show does before the per-file semantics begin.
func printCommitHeader(repoDir, commit string) {
	c := exec.Command("git", "--no-pager", "show", "--no-patch", "--format=medium", commit)
	c.Dir = repoDir
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	_ = c.Run()
	fmt.Println()
}
