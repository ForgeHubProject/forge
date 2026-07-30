// Package forgerepo reads a forge repository: the git plumbing the commands
// share, the per-repo handler configuration under .forge/, and the semantic
// comparison of one path across two sides. It is where the CLI and any other
// consumer meet the same answer; internal/gitrepo, by contrast, is the go-git
// view of the working tree.
package forgerepo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/forgehubproject/forge/internal/handler"
)

// ── comparison sources ──────────────────────────────────────────────────────────────────────────────

// SourceKind distinguishes what a side of a comparison reads from.
type SourceKind int

const (
	SourceWorktree SourceKind = iota // the files on disk
	SourceRevision                   // a commit in history
	SourceEmpty                      // nothing at all — what a root commit's parent is
)

// Source is one side of a comparison. Rev holds the resolved commit, so a
// ref that moves while the command runs cannot change which bytes are read;
// Name is what the caller called it and exists only for output.
type Source struct {
	Kind SourceKind
	Rev  string
	Name string
}

func WorktreeSource() Source { return Source{Kind: SourceWorktree, Name: "the working tree"} }

func RevisionSource(name, commit string) Source {
	return Source{Kind: SourceRevision, Rev: commit, Name: name}
}

func EmptySource() Source { return Source{Kind: SourceEmpty, Name: "nothing"} }

// EmptyRepoSource is the empty side of a comparison in a repository with no
// commits. It differs from EmptySource only in its name, which is printed: a
// root commit's parent is fairly called nothing, a whole repository is not.
func EmptyRepoSource() Source {
	return Source{Kind: SourceEmpty, Name: "an empty repository"}
}

func (s Source) String() string { return s.Name }

// GitOutput runs git inside the repository and returns its stdout. A failure
// carries git's own message, so callers can report why git refused.
//
// ctx bounds the process: a caller whose own context is cancelled — a request of
// a long-lived server, abandoned by its client — takes git down with it instead
// of leaving it to finish work nobody will read.
func GitOutput(ctx context.Context, repoDir string, args ...string) ([]byte, error) {
	c := exec.CommandContext(ctx, "git", args...)
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

// ResolveRev resolves a revision to the commit it names, the way git resolves it
// (object id, branch, tag, HEAD~2, ...). A revision git will not resolve is
// reported by name.
func ResolveRev(ctx context.Context, repoDir, rev string) (string, error) {
	out, err := GitOutput(ctx, repoDir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	commit := strings.TrimSpace(string(out))
	if err != nil || commit == "" {
		return "", fmt.Errorf("not a valid revision: %s", rev)
	}
	return commit, nil
}

func IsRevision(ctx context.Context, repoDir, rev string) bool {
	_, err := ResolveRev(ctx, repoDir, rev)
	return err == nil
}

// pathExists reports whether an argument names something on disk, which is what
// makes it a path rather than a revision.
func pathExists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

// SplitRevsAndPaths tells revisions from paths the way git does. Everything past
// an explicit "--" (dashAt, from cobra's ArgsLenAtDash) is a path; otherwise
// leading arguments that resolve as revisions are revisions and the rest are
// paths. An argument that is both a revision and an existing file is refused
// exactly as git refuses it, because only "--" can say which was meant.
//
// Revisions come back however many there are, with or without a "--": how many
// are allowed is the command's own to say, as forge show and DiffSources each do.
//
// While a revision is still possible, an argument that is neither a revision git
// resolves nor a file on disk is refused by name, as git refuses it: a mistyped
// revision is indistinguishable from a file that is not there, and reading it as
// a path would compare something other than what was asked for while reporting
// success. Once the paths have begun — after "--", or after a path — a path no
// side holds is kept and reported absent instead, except for one that resolves
// as a revision: only "--" can say a revision's name was meant as a path.
func SplitRevsAndPaths(ctx context.Context, repoDir string, args []string, dashAt int) (revs, paths []string, err error) {
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
			if !pathExists(arg) && IsRevision(ctx, repoDir, arg) {
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
			if pathExists(arg) || !IsRevision(ctx, repoDir, arg) {
				paths = append(paths, arg)
				continue
			}
			revs = append(revs, arg)
			continue
		}
		switch {
		case IsRevision(ctx, repoDir, arg):
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

// ExpandRevRange rewrites git's two-dot range form so `forge diff a..b` means
// what `forge diff a b` means, as it does in git; dashAt moves with the extra
// argument. Anything else — a three-dot range, a file whose name contains ".."
// — is left for the ordinary rules.
func ExpandRevRange(ctx context.Context, repoDir string, args []string, dashAt int) ([]string, int) {
	if len(args) == 0 || dashAt == 0 {
		return args, dashAt
	}
	arg := args[0]
	if pathExists(arg) || strings.Contains(arg, "...") {
		return args, dashAt
	}
	base, head, found := strings.Cut(arg, "..")
	if !found || base == "" || head == "" || !IsRevision(ctx, repoDir, base) || !IsRevision(ctx, repoDir, head) {
		return args, dashAt
	}
	if dashAt > 0 {
		dashAt++
	}
	return append([]string{base, head}, args[1:]...), dashAt
}

// DiffSources maps forge diff's revision arguments onto the two sides being
// compared: none is the working tree against HEAD, one is the working tree
// against that revision, two is revision to revision.
//
// More than two is refused. git gives three or more revisions a meaning of its
// own, and one whose result differs from comparing the first two, so keeping
// only the first two would report a comparison the caller did not ask for while
// exiting zero — indistinguishable, to anything reading the status, from the
// comparison that was asked for.
func DiffSources(ctx context.Context, repoDir string, revs []string) (base, head Source, err error) {
	switch len(revs) {
	case 0:
		commit, headErr := ResolveRev(ctx, repoDir, "HEAD")
		if headErr != nil {
			// A repository with no commits has nothing to compare against, so
			// the whole working tree reads as newly added.
			return EmptyRepoSource(), WorktreeSource(), nil
		}
		return RevisionSource("HEAD", commit), WorktreeSource(), nil
	case 1:
		commit, err := ResolveRev(ctx, repoDir, revs[0])
		if err != nil {
			return base, head, err
		}
		return RevisionSource(revs[0], commit), WorktreeSource(), nil
	case 2:
		baseCommit, err := ResolveRev(ctx, repoDir, revs[0])
		if err != nil {
			return base, head, err
		}
		headCommit, err := ResolveRev(ctx, repoDir, revs[1])
		if err != nil {
			return base, head, err
		}
		return RevisionSource(revs[0], baseCommit), RevisionSource(revs[1], headCommit), nil
	default:
		return base, head, fmt.Errorf("forge diff takes at most two revisions, got %d: %s\n"+
			"if some of those are paths, put them after \"--\"", len(revs), strings.Join(revs, " "))
	}
}

// RelPath resolves a path argument to a repo-relative, slash-separated path.
// Arguments are relative to where the command was run, as git's are, and one
// that lands outside the repository is refused rather than read.
func RelPath(repoDir, p string) (string, error) {
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

// RelPaths resolves every path argument, refusing the whole command if any
// one of them points outside the repository.
func RelPaths(repoDir string, args []string) ([]string, error) {
	var out []string
	for _, a := range args {
		rel, err := RelPath(repoDir, a)
		if err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, nil
}

// ── reading and comparing one file ──────────────────────────────────────────────────────────────────

// SourceEntry reports what a source holds at path — "blob", "tree", "commit"
// (the gitlink a submodule is recorded as), or "" when it holds nothing there.
func SourceEntry(ctx context.Context, repoDir string, src Source, path string) string {
	if path == "." {
		return "tree" // the repository root itself
	}
	switch src.Kind {
	case SourceWorktree:
		// Lstat, not Stat: a symlink is an entry in its own right — git records one
		// as a blob holding the path it names — and following it here would report
		// the type of a file the repository does not contain, one a committed link
		// can point anywhere.
		st, err := os.Lstat(filepath.Join(repoDir, filepath.FromSlash(path)))
		switch {
		case err != nil:
			return ""
		case st.IsDir():
			return "tree"
		default:
			return "blob"
		}
	case SourceRevision:
		// The type is read out of the tree entry, which is the only place a
		// gitlink's is: the commit it names lives in the submodule's object store
		// and not this repository's, so `git cat-file -t` cannot report it and the
		// entry would read as absent. ":(literal)" keeps a name containing
		// pathspec wildcards from matching some other entry.
		out, err := GitOutput(ctx, repoDir, "ls-tree", "-z", src.Rev, "--", ":(literal)"+path)
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
// there at all. entry is what SourceEntry reported, since only a blob has bytes.
// Revision blobs come from git itself — `git show <rev>:<path>`, the mechanism
// the merge path already uses to read index stages.
func blobAt(ctx context.Context, repoDir string, src Source, path, entry string) ([]byte, bool, error) {
	if entry != "blob" {
		return nil, false, nil
	}
	if src.Kind == SourceWorktree {
		full := filepath.Join(repoDir, filepath.FromSlash(path))
		// A symlink's content is the path it names, which is what git committed for
		// it and so what the other side of the comparison holds. Reading through it
		// would compare a file the repository does not contain against the link that
		// names it — two sides disagreeing about what the file even is — and would
		// let repository content, not a path argument, decide what gets read.
		if st, err := os.Lstat(full); err == nil && st.Mode()&os.ModeSymlink != 0 {
			target, err := os.Readlink(full)
			if err != nil {
				return nil, false, fmt.Errorf("reading %s: %w", path, err)
			}
			return []byte(target), true, nil
		}
		data, err := os.ReadFile(full)
		if err != nil {
			return nil, false, fmt.Errorf("reading %s: %w", path, err)
		}
		return data, true, nil
	}
	data, err := GitOutput(ctx, repoDir, "show", src.Rev+":"+path)
	if err != nil {
		return nil, false, fmt.Errorf("reading %s at %s: %w", path, src.Name, err)
	}
	return data, true, nil
}

// FileComparison is one path compared across two sources.
type FileComparison struct {
	Path      string
	HandlerID string // the handler that resolved; "text" for the catch-all
	Semantic  bool   // a format handler claimed the path
	Diff      handler.StructuredDiff
	Base      []byte // nil when the base side holds no such file
	Head      []byte
	BaseFound bool
	HeadFound bool
}

// CompareFile produces the semantic diff of one path between two sources, and is
// the single route every mode takes — the working tree against HEAD or any
// revision, revision to revision, forge show, and the --web page — so they all
// compute a change the same way, and so callers outside the CLI can compute it
// identically too. path is repo-relative and slash-separated (RelPath).
//
// A path no format handler claims is reported with Semantic false and its bytes
// left unread: every caller renders those through git's own text diff, so
// reading a large file only to discard the read would be waste. A path a side
// holds as something other than a blob — a submodule's gitlink — has no bytes for
// a handler to read either, so it takes that same route rather than being called
// absent. A path neither source holds at all is not an error: both Found flags
// come back false and the caller says so for that one file instead of abandoning
// the rest.
func CompareFile(ctx context.Context, repoDir string, reg *handler.Registry, path string, base, head Source) (FileComparison, error) {
	h, err := reg.Resolve(path)
	if err != nil {
		return FileComparison{}, err
	}
	fc := FileComparison{Path: path, HandlerID: HandlerFormat(h), Semantic: IsBinaryHandler(h)}

	baseEntry := SourceEntry(ctx, repoDir, base, path)
	headEntry := SourceEntry(ctx, repoDir, head, path)
	fc.BaseFound, fc.HeadFound = baseEntry != "", headEntry != ""
	if (fc.BaseFound && baseEntry != "blob") || (fc.HeadFound && headEntry != "blob") {
		fc.Semantic = false
	}
	if !fc.Semantic || (!fc.BaseFound && !fc.HeadFound) {
		return fc, nil
	}

	fc.Base, fc.BaseFound, err = blobAt(ctx, repoDir, base, path, baseEntry)
	if err != nil {
		return fc, err
	}
	fc.Head, fc.HeadFound, err = blobAt(ctx, repoDir, head, path, headEntry)
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

// HandlerFormat names the handler that resolved, "unknown" if it does not say.
func HandlerFormat(h handler.ForgeHandler) string {
	if n, ok := h.(handler.Namer); ok {
		return n.Format()
	}
	return "unknown"
}

// ── listing what changed ────────────────────────────────────────────────────────────────────────────

// GitDiffArgs builds the git invocation comparing two sources: a revision
// against the working tree, revision against revision, or — where the base is
// nothing, as a root commit's is — the commit against the empty tree.
//
// Rename detection is turned off, because forge compares one path at a time and
// a rename git reports as one record names only its destination: the source
// would never be listed and so never be reported gone, while the destination —
// a path the base side does not have — would be compared against nothing and
// read as wholly added, however unchanged its bytes. Both halves are listed
// instead, each honest about the side that holds it.
func GitDiffArgs(base, head Source, flags, pathspecs []string) []string {
	var args []string
	flags = append([]string{"--no-renames"}, flags...)
	switch {
	case base.Kind == SourceRevision && head.Kind == SourceRevision:
		args = append(append([]string{"diff"}, flags...), base.Rev, head.Rev)
	case base.Kind == SourceRevision:
		args = append(append([]string{"diff"}, flags...), base.Rev)
	case head.Kind == SourceRevision:
		args = append(append([]string{"diff-tree", "-r", "--root", "--no-commit-id"}, flags...), head.Rev)
	default:
		args = append([]string{"diff"}, flags...)
	}
	if len(pathspecs) > 0 {
		args = append(args, "--")
		args = append(args, pathspecs...)
	}
	return args
}

// ChangedPaths lists the files that differ between two sources, restricted to
// the given pathspecs. --name-only reports one path per changed file, and
// GitDiffArgs leaves renames undetected, so every record is a file to compare.
func ChangedPaths(ctx context.Context, repoDir string, base, head Source, pathspecs []string) ([]string, error) {
	out, err := GitOutput(ctx, repoDir, GitDiffArgs(base, head, []string{"--name-only", "-z"}, pathspecs)...)
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

// ComparePaths resolves path arguments to the files to compare: a file is
// itself, and a directory expands to the files that changed under it. An
// argument neither source holds is kept, so it is reported as absent rather
// than silently dropped.
func ComparePaths(ctx context.Context, repoDir string, base, head Source, paths []string) ([]string, error) {
	var out []string
	for _, p := range paths {
		if SourceEntry(ctx, repoDir, base, p) != "tree" && SourceEntry(ctx, repoDir, head, p) != "tree" {
			out = append(out, p)
			continue
		}
		under, err := ChangedPaths(ctx, repoDir, base, head, []string{p})
		if err != nil {
			return nil, err
		}
		out = append(out, under...)
	}
	return out, nil
}

// TextChangeSummary reports what git counts for a path no handler claims.
func TextChangeSummary(ctx context.Context, repoDir string, base, head Source, path string) string {
	out, err := GitOutput(ctx, repoDir, GitDiffArgs(base, head, []string{"--numstat"}, []string{path})...)
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
