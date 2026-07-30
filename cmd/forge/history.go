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
// Revisions come back however many there are, with or without a "--": how many
// are allowed is the command's own to say, as forge show and diffSources each do.
//
// While a revision is still possible, an argument that is neither a revision git
// resolves nor a file on disk is refused by name, as git refuses it: a mistyped
// revision is indistinguishable from a file that is not there, and reading it as
// a path would compare something other than what was asked for while reporting
// success. Once the paths have begun — after "--", or after a path — a path no
// side holds is kept and reported absent instead, except for one that resolves
// as a revision: only "--" can say a revision's name was meant as a path.
func splitRevsAndPaths(repoDir string, args []string, dashAt int) (revs, paths []string, err error) {
	if dashAt >= 0 && dashAt <= len(args) {
		return args[:dashAt], args[dashAt:], nil
	}
	for _, arg := range args {
		if len(paths) > 0 {
			// Past the first path an argument that resolves as a revision and is
			// not on disk is refused rather than kept as an absent path: git
			// refuses it too, and reading it as a path would compare only the
			// arguments before it — a pair the caller did not ask for, reported at
			// exit zero alongside a line calling a revision forge resolved missing.
			if !pathExists(arg) && isRevision(repoDir, arg) {
				return nil, nil, fmt.Errorf("revisions must come before paths: %s came after %s\n"+
					"if %s is a path, the working tree does not have it — put it after \"--\"", arg, paths[0], arg)
			}
			paths = append(paths, arg)
			continue
		}
		if len(revs) >= 2 {
			// Past the second revision the only argument worth telling apart is a
			// further revision: it is collected, so the command's own arity refuses
			// it rather than the extras being dropped into a comparison of the
			// first two that nobody asked for. Anything else begins the paths — a
			// name on disk, with no ambiguity left for "--" to settle, and a name
			// no side holds, kept for the caller to report absent.
			if pathExists(arg) || !isRevision(repoDir, arg) {
				paths = append(paths, arg)
				continue
			}
			revs = append(revs, arg)
			continue
		}
		switch {
		case isRevision(repoDir, arg):
			if pathExists(arg) {
				return nil, nil, fmt.Errorf("ambiguous argument %q: both a revision and a file\n"+
					"use \"--\" to separate paths from revisions, like: forge diff <revision> -- %s", arg, arg)
			}
			revs = append(revs, arg)
		case pathExists(arg):
			paths = append(paths, arg)
		default:
			return nil, nil, fmt.Errorf("not a valid revision: %s\n"+
				"if it is a path, the working tree does not have it — put it after \"--\"", arg)
		}
	}
	return revs, paths, nil
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
//
// More than two is refused. git gives three or more revisions a meaning of its
// own, and one whose result differs from comparing the first two, so keeping
// only the first two would report a comparison the caller did not ask for while
// exiting zero — indistinguishable, to anything reading the status, from the
// comparison that was asked for.
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
	case 2:
		baseCommit, err := resolveRev(repoDir, revs[0])
		if err != nil {
			return base, head, err
		}
		headCommit, err := resolveRev(repoDir, revs[1])
		if err != nil {
			return base, head, err
		}
		return revisionSource(revs[0], baseCommit), revisionSource(revs[1], headCommit), nil
	default:
		return base, head, fmt.Errorf("forge diff takes at most two revisions, got %d: %s\n"+
			"if some of those are paths, put them after \"--\"", len(revs), strings.Join(revs, " "))
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

// sourceEntry reports what a source holds at path — "blob", "tree", "commit"
// (the gitlink a submodule is recorded as), or "" when it holds nothing there.
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
		// The type is read out of the tree entry, which is the only place a
		// gitlink's is: the commit it names lives in the submodule's object store
		// and not this repository's, so `git cat-file -t` cannot report it and the
		// entry would read as absent. ":(literal)" keeps a name containing
		// pathspec wildcards from matching some other entry.
		out, err := gitOutput(repoDir, "ls-tree", "-z", src.rev, "--", ":(literal)"+path)
		if err != nil {
			return ""
		}
		// <mode> SP <type> SP <object> TAB <name>, one NUL-terminated record.
		record, _, _ := strings.Cut(string(out), "\x00")
		meta, _, ok := strings.Cut(record, "\t")
		if !ok {
			return ""
		}
		if fields := strings.Fields(meta); len(fields) >= 2 {
			return fields[1]
		}
	}
	return ""
}

// blobAt returns path's bytes at one source and whether that source holds a blob
// there at all. entry is what sourceEntry reported, since only a blob has bytes.
// Revision blobs come from git itself — `git show <rev>:<path>`, the mechanism
// the merge path already uses to read index stages.
func blobAt(repoDir string, src blobSource, path, entry string) ([]byte, bool, error) {
	if entry != "blob" {
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
// reading a large file only to discard the read would be waste. A path a side
// holds as something other than a blob — a submodule's gitlink — has no bytes for
// a handler to read either, so it takes that same route rather than being called
// absent. A path neither source holds at all is not an error: both Found flags
// come back false and the caller says so for that one file instead of abandoning
// the rest.
func compareFile(repoDir string, reg *handler.Registry, path string, base, head blobSource) (fileComparison, error) {
	h, err := reg.Resolve(path)
	if err != nil {
		return fileComparison{}, err
	}
	fc := fileComparison{Path: path, HandlerID: handlerFormat(h), Semantic: isBinaryHandler(h)}

	baseEntry := sourceEntry(repoDir, base, path)
	headEntry := sourceEntry(repoDir, head, path)
	fc.BaseFound, fc.HeadFound = baseEntry != "", headEntry != ""
	if (fc.BaseFound && baseEntry != "blob") || (fc.HeadFound && headEntry != "blob") {
		fc.Semantic = false
	}
	if !fc.Semantic || (!fc.BaseFound && !fc.HeadFound) {
		return fc, nil
	}

	fc.Base, fc.BaseFound, err = blobAt(repoDir, base, path, baseEntry)
	if err != nil {
		return fc, err
	}
	fc.Head, fc.HeadFound, err = blobAt(repoDir, head, path, headEntry)
	if err != nil {
		return fc, err
	}
	// An absent side is an empty blob: the handler then reports the file as
	// wholly added or wholly removed, which is what it means.
	fc.Diff, err = h.Diff(fc.Base, fc.Head)
	if err != nil {
		return fc, fmt.Errorf("diff %s: %w", path, err)
	}
	return fc, nil
}

// renderPaths renders every path, reporting one that fails and carrying on, so a
// single file forge cannot read does not cost the rest of the report. The
// failures also reach the returned error and so the exit status: a caller that
// shells out to forge and reads the status has no other way to learn that a
// comparison it asked for was never produced.
func renderPaths(paths []string, render func(path string) error) error {
	var failed []string
	for _, p := range paths {
		if err := render(p); err != nil {
			fmt.Fprintf(os.Stderr, "forge: %s: %v\n", p, err)
			failed = append(failed, p)
		}
	}
	if len(failed) == 0 {
		return nil
	}
	return fmt.Errorf("could not compare: %s", strings.Join(failed, ", "))
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
//
// Rename detection is turned off, because forge compares one path at a time and
// a rename git reports as one record names only its destination: the source
// would never be listed and so never be reported gone, while the destination —
// a path the base side does not have — would be compared against nothing and
// read as wholly added, however unchanged its bytes. Both halves are listed
// instead, each honest about the side that holds it.
func gitDiffArgs(base, head blobSource, flags, pathspecs []string) []string {
	var args []string
	flags = append([]string{"--no-renames"}, flags...)
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
// the given pathspecs. --name-only reports one path per changed file, and
// gitDiffArgs leaves renames undetected, so every record is a file to compare.
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
	return renderPaths(files, func(p string) error {
		return showFile(repoDir, reg, p, base, head)
	})
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
