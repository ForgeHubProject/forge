package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/spf13/cobra"
	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/gitrepo"
	"github.com/forgehubproject/forge/internal/handler"
	"github.com/forgehubproject/forge/internal/handler/text"
)

func main() {
	if err := rootCmd().Execute(); err != nil {
		os.Exit(1)
	}
}

func rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "forge",
		Short: "Git, but for everything.",
		Long:  "Forge is a format-aware version control system with semantic diff and merge for any file type.",
	}
	root.AddCommand(
		initCmd(),
		cloneCmd(),
		statusCmd(),
		diffCmd(),
		mergeCmd(),
		mergeFileCmd(),
		mergeToolCmd(),
		logCmd(),
		pushCmd(),
		pullCmd(),
		sourceCmd(),
		formatsCmd(),
		gitPassthrough("add", "Stage file contents (delegates to git)"),
		gitPassthrough("commit", "Record staged changes (delegates to git)"),
		gitPassthrough("branch", "List, create or delete branches (delegates to git)"),
		gitPassthrough("checkout", "Switch branches or restore files (delegates to git)"),
		gitPassthrough("switch", "Switch branches (delegates to git)"),
		gitPassthrough("fetch", "Download objects and refs from remote (delegates to git)"),
		gitPassthrough("stash", "Stash working tree changes (delegates to git)"),
		gitPassthrough("reset", "Reset HEAD or working tree (delegates to git)"),
		gitPassthrough("restore", "Restore working tree files (delegates to git)"),
		gitPassthrough("rebase", "Reapply commits on top of another branch (delegates to git)"),
		gitPassthrough("tag", "Create, list or delete tags (delegates to git)"),
		gitPassthrough("remote", "Manage remote connections (delegates to git)"),
	)
	return root
}

// defaultRegistry builds the handler registry for the current repo.
// Extensions listed in .forge-formats activate their installed FHR handlers.
// Everything else falls through to the text catch-all.
func defaultRegistry() *handler.Registry {
	reg := handler.NewRegistry()

	repoDir := findRepoRoot()
	forgeFormats := loadForgeFormats(repoDir)

	for _, meta := range fhr.LoadInstalledHandlers() {
		binary := fhr.InstalledHandlerBinary(meta.ID)
		if binary == "" {
			fmt.Fprintf(os.Stderr, "forge: warning: handler %q metadata found but binary missing\n", meta.ID)
			continue
		}
		// If .forge-formats exists, only activate handlers for listed extensions.
		if len(forgeFormats) > 0 {
			wanted := false
			for _, ext := range meta.Formats {
				if forgeFormats[strings.ToLower(ext)] {
					wanted = true
					break
				}
			}
			if !wanted {
				continue
			}
		}
		reg.Register(fhr.NewSubprocessHandler(binary, meta))
	}

	reg.Register(text.New()) // catch-all — must be last
	return reg
}

// findRepoRoot returns the root of the current git repository, or "." if not in one.
func findRepoRoot() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "."
	}
	return strings.TrimSpace(string(out))
}

// loadForgeFormats reads .forge-formats from repoDir and returns the set of
// extensions (lower-cased, with leading dot) that this repo wants handled
// semantically. Returns nil if the file does not exist.
func loadForgeFormats(repoDir string) map[string]bool {
	data, err := os.ReadFile(filepath.Join(repoDir, ".forge-formats"))
	if err != nil {
		return nil
	}
	exts := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, ".") {
			line = "." + line
		}
		exts[strings.ToLower(line)] = true
	}
	return exts
}

// ── forge init ────────────────────────────────────────────────────────────────────────────────

func initCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "init [directory]",
		Short: "Create a new Forge repository",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runInit,
	}
}

func runInit(_ *cobra.Command, args []string) error {
	dir := "."
	if len(args) == 1 {
		dir = args[0]
	}

	if _, err := gogit.PlainInit(dir, false); err != nil && !errors.Is(err, gogit.ErrRepositoryAlreadyExists) {
		return fmt.Errorf("init failed: %w", err)
	}

	if err := setupGitMergeDriver(dir); err != nil {
		fmt.Fprintf(os.Stderr, "forge: warning: could not configure git merge driver: %v\n", err)
	}

	abs, _ := filepath.Abs(dir)
	fmt.Printf("Initialized Forge repository in %s\n", abs)
	return nil
}

// setupGitMergeDriver registers the forge merge driver in .git/config and
// adds .gitattributes entries for each extension listed in .forge-formats.
func setupGitMergeDriver(repoDir string) error {
	attrPath := filepath.Join(repoDir, ".gitattributes")
	existing, _ := os.ReadFile(attrPath)
	forgeFormats := loadForgeFormats(repoDir)
	var toAdd []string
	for ext := range forgeFormats {
		entry := "*" + ext + "  merge=forge"
		if !bytes.Contains(existing, []byte("*"+ext)) {
			toAdd = append(toAdd, entry)
		}
	}
	sort.Strings(toAdd)
	if len(toAdd) > 0 {
		f, err := os.OpenFile(attrPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		if len(existing) > 0 && !bytes.HasSuffix(existing, []byte("\n")) {
			fmt.Fprintln(f)
		}
		fmt.Fprintln(f, "# Forge semantic merge drivers")
		for _, e := range toAdd {
			fmt.Fprintln(f, e)
		}
		f.Close()
	}

	gitConfigPath := filepath.Join(repoDir, ".git", "config")
	gitConfig, _ := os.ReadFile(gitConfigPath)
	if !bytes.Contains(gitConfig, []byte(`[merge "forge"]`)) {
		f, err := os.OpenFile(gitConfigPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		fmt.Fprintf(f, "\n[merge \"forge\"]\n\tname = Forge semantic merge\n\tdriver = forge merge-file %%O %%A %%B %%P\n")
		f.Close()
	}

	return nil
}

// ── forge status ────────────────────────────────────────────────────────────────────────────

func statusCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Show working tree status with handler annotations",
		RunE:  runStatus,
	}
}

func runStatus(_ *cobra.Command, _ []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	r, err := gogit.PlainOpenWithOptions(cwd, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return fmt.Errorf("not a git repository")
	}

	if head, err := r.Head(); err == nil {
		if head.Name().IsBranch() {
			fmt.Printf("On branch \x1b[1m%s\x1b[0m\n", head.Name().Short())
			printAheadBehind()
		} else {
			fmt.Printf("HEAD detached at %s\n", head.Hash().String()[:7])
		}
	}

	if gitDir, err := exec.Command("git", "rev-parse", "--git-dir").Output(); err == nil {
		mergeHead := filepath.Join(strings.TrimSpace(string(gitDir)), "MERGE_HEAD")
		if _, mergeErr := os.Stat(mergeHead); mergeErr == nil {
			printMergeStatus()
		}
	}

	wt, err := r.Worktree()
	if err != nil {
		return err
	}

	st, err := wt.Status()
	if err != nil {
		return err
	}

	if st.IsClean() {
		fmt.Println("nothing to commit, working tree clean")
		return nil
	}

	reg := defaultRegistry()

	paths := make([]string, 0, len(st))
	for p := range st {
		paths = append(paths, p)
	}
	sort.Strings(paths)

	var stagedPaths, unstagedPaths, untrackedPaths []string
	for _, p := range paths {
		fs := st[p]
		s, w := rune(fs.Staging), rune(fs.Worktree)
		if s == '?' && w == '?' {
			untrackedPaths = append(untrackedPaths, p)
			continue
		}
		if s != ' ' && s != '?' {
			stagedPaths = append(stagedPaths, p)
		}
		if w != ' ' && w != '?' {
			unstagedPaths = append(unstagedPaths, p)
		}
	}

	statusWord := func(code rune) string {
		switch code {
		case 'A':
			return "new file:  "
		case 'D':
			return "deleted:   "
		case 'R':
			return "renamed:   "
		case 'C':
			return "copied:    "
		default:
			return "modified:  "
		}
	}

	printStagedEntry := func(p string) {
		label := handlerLabel(p, reg)
		word := statusWord(rune(st[p].Staging))
		fmt.Printf("\x1b[32m\t%s%-38s\x1b[0m %s\n", word, p, label)
	}

	printUnstagedEntry := func(p string) {
		label := handlerLabel(p, reg)
		word := statusWord(rune(st[p].Worktree))
		fmt.Printf("\x1b[31m\t%s%-38s\x1b[0m %s\n", word, p, label)
	}

	printUntrackedEntry := func(p string) {
		label := handlerLabel(p, reg)
		fmt.Printf("\x1b[31m\t%-49s\x1b[0m %s\n", p, label)
	}

	if len(stagedPaths) > 0 {
		fmt.Println("Changes to be committed:")
		fmt.Println("  \x1b[2m(use \"forge restore --staged <file>...\" to unstage)\x1b[0m")
		for _, p := range stagedPaths {
			printStagedEntry(p)
		}
		fmt.Println()
	}

	if len(unstagedPaths) > 0 {
		fmt.Println("Changes not staged for commit:")
		fmt.Println("  \x1b[2m(use \"forge add <file>...\" to update what will be committed)\x1b[0m")
		fmt.Println("  \x1b[2m(use \"forge restore <file>...\" to discard changes in working directory)\x1b[0m")
		for _, p := range unstagedPaths {
			printUnstagedEntry(p)
		}
		fmt.Println()
	}

	if len(untrackedPaths) > 0 {
		fmt.Println("Untracked files:")
		fmt.Println("  \x1b[2m(use \"forge add <file>...\" to include in what will be committed)\x1b[0m")
		for _, p := range untrackedPaths {
			printUntrackedEntry(p)
		}
		fmt.Println()
	}

	return nil
}

// handlerLabel returns a coloured handler annotation for a file path.
func handlerLabel(path string, reg *handler.Registry) string {
	h, err := reg.Resolve(path)
	if err != nil {
		return "\x1b[31m[no handler]\x1b[0m"
	}
	name := "text"
	if n, ok := h.(handler.Namer); ok {
		name = n.Format()
	}
	return fmt.Sprintf("\x1b[36m[%s]\x1b[0m", name)
}

// printAheadBehind prints branch divergence info relative to the upstream.
func printAheadBehind() {
	upstreamOut, err := exec.Command("git", "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{u}").Output()
	if err != nil {
		return
	}
	upstream := strings.TrimSpace(string(upstreamOut))

	aheadOut, _ := exec.Command("git", "rev-list", "--count", upstream+"..HEAD").Output()
	behindOut, _ := exec.Command("git", "rev-list", "--count", "HEAD.."+upstream).Output()
	ahead, _ := strconv.Atoi(strings.TrimSpace(string(aheadOut)))
	behind, _ := strconv.Atoi(strings.TrimSpace(string(behindOut)))

	switch {
	case ahead > 0 && behind > 0:
		fmt.Printf("Your branch and '%s' have diverged,\nand have %d and %d different commits each, respectively.\n", upstream, ahead, behind)
		fmt.Println("  (use \"forge pull\" to update your local branch)")
	case ahead > 0:
		noun := "commit"
		if ahead != 1 {
			noun = "commits"
		}
		fmt.Printf("Your branch is ahead of '%s' by %d %s.\n", upstream, ahead, noun)
		fmt.Println("  (use \"forge push\" to publish your local commits)")
	case behind > 0:
		noun := "commit"
		if behind != 1 {
			noun = "commits"
		}
		fmt.Printf("Your branch is behind '%s' by %d %s, and can be fast-forwarded.\n", upstream, behind, noun)
		fmt.Println("  (use \"forge pull\" to update your local branch)")
	}
}

// printMergeStatus prints unmerged paths and hints during an active merge.
func printMergeStatus() {
	out, _ := exec.Command("git", "status", "--porcelain").Output()

	type unmergedEntry struct{ code, path string }
	var entries []unmergedEntry
	for _, line := range strings.Split(string(out), "\n") {
		if len(line) < 4 {
			continue
		}
		xy := line[:2]
		path := strings.TrimSpace(line[3:])
		if strings.ContainsAny(xy, "U") || xy == "AA" || xy == "DD" {
			entries = append(entries, unmergedEntry{xy, path})
		}
	}

	conflictLabel := func(xy string) string {
		switch xy {
		case "AA":
			return "both added:     "
		case "UU":
			return "both modified:  "
		case "DD":
			return "both deleted:   "
		case "AU", "UA":
			return "added/modified: "
		case "DU", "UD":
			return "deleted/modified:"
		default:
			return "unmerged:       "
		}
	}

	fmt.Println("You have unmerged paths.")
	fmt.Println("  (fix conflicts and run \"forge commit\")")
	fmt.Println("  (use \"forge merge --abort\" to abort the merge)")
	if len(entries) > 0 {
		fmt.Println()
		fmt.Println("Unmerged paths:")
		fmt.Println("  (use \"forge mergetool\" to resolve · \"forge add <file>\" to mark resolved)")
		reg := defaultRegistry()
		for _, e := range entries {
			label := handlerLabel(e.path, reg)
			fmt.Printf("\x1b[31m\t%s%-38s\x1b[0m %s\n", conflictLabel(e.code), e.path, label)
		}
	}
	fmt.Println()
}

// ── forge clone ─────────────────────────────────────────────────────────────────────────────

func cloneCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clone <url> [directory]",
		Short: "Clone a Forge repository and report required handlers",
		Args:  cobra.RangeArgs(1, 2),
		RunE:  runClone,
	}
	cmd.Flags().String("token", "", "Personal access token for HTTPS authentication")
	cmd.Flags().String("ssh-key", defaultSSHKey(), "Path to SSH private key")
	cmd.Flags().String("ssh-password", "", "Passphrase for the SSH private key (if encrypted)")
	return cmd
}

func runClone(cmd *cobra.Command, args []string) error {
	rawURL := args[0]
	dir := ""
	if len(args) == 2 {
		dir = args[1]
	} else {
		dir = repoNameFromURL(rawURL)
	}

	token, _ := cmd.Flags().GetString("token")
	sshKey, _ := cmd.Flags().GetString("ssh-key")
	sshPassword, _ := cmd.Flags().GetString("ssh-password")

	cloneOpts, err := buildCloneOptions(rawURL, token, sshKey, sshPassword)
	if err != nil {
		return err
	}
	cloneOpts.Progress = os.Stdout

	fmt.Printf("Cloning into '%s'...\n", dir)

	_, err = gogit.PlainClone(dir, false, cloneOpts)
	if err != nil {
		return fmt.Errorf("clone failed: %w", err)
	}

	if err := setupGitMergeDriver(dir); err != nil {
		fmt.Fprintf(os.Stderr, "forge: warning: could not configure git merge driver: %v\n", err)
	}

	reportMissingHandlers(dir)
	return nil
}

func buildCloneOptions(rawURL, token, sshKeyPath, sshPassword string) (*gogit.CloneOptions, error) {
	opts := &gogit.CloneOptions{URL: rawURL}

	if isSSHURL(rawURL) {
		if agent, err := gogitssh.NewSSHAgentAuth("git"); err == nil {
			opts.Auth = agent
			return opts, nil
		}
		keys, err := gogitssh.NewPublicKeysFromFile("git", sshKeyPath, sshPassword)
		if err != nil {
			return nil, fmt.Errorf(
				"SSH agent unavailable and could not load key %s: %w\n"+
					"  Start an SSH agent and run: ssh-add %s\n"+
					"  Or pass the key passphrase: forge clone <url> --ssh-password <passphrase>\n"+
					"  Or clone over HTTPS with a token: forge clone <https-url> --token <token>",
				sshKeyPath, err, sshKeyPath,
			)
		}
		opts.Auth = keys
		return opts, nil
	}

	if token == "" {
		for _, env := range []string{"FORGE_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"} {
			if t := os.Getenv(env); t != "" {
				token = t
				break
			}
		}
	}
	if token != "" {
		opts.Auth = &gogithttp.BasicAuth{Username: "x-token", Password: token}
	}

	return opts, nil
}

func isSSHURL(rawURL string) bool {
	if strings.HasPrefix(rawURL, "git@") {
		return true
	}
	u, err := url.Parse(rawURL)
	return err == nil && u.Scheme == "ssh"
}

func defaultSSHKey() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
		p := filepath.Join(home, ".ssh", name)
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return filepath.Join(home, ".ssh", "id_ed25519")
}

// reportMissingHandlers reads .forge-formats and warns about extensions that
// have no installed FHR handler.
func reportMissingHandlers(repoDir string) {
	forgeFormats := loadForgeFormats(repoDir)
	if len(forgeFormats) == 0 {
		return
	}

	covered := map[string]bool{}
	for _, meta := range fhr.LoadInstalledHandlers() {
		for _, ext := range meta.Formats {
			covered[strings.ToLower(ext)] = true
		}
	}

	var missing []string
	for ext := range forgeFormats {
		if !covered[ext] {
			missing = append(missing, ext)
		}
	}
	if len(missing) == 0 {
		return
	}

	sort.Strings(missing)
	fmt.Println()
	fmt.Println("This repository requires format handlers that are not installed:")
	for _, ext := range missing {
		fmt.Printf("  forge formats add %s\n", ext)
	}
	fmt.Println()
}

func repoNameFromURL(url string) string {
	url = strings.TrimRight(url, "/")
	url = strings.TrimSuffix(url, ".git")
	if i := strings.LastIndexAny(url, "/:'"); i >= 0 {
		url = url[i+1:]
	}
	if url == "" {
		return "repo"
	}
	return url
}

// ── forge diff ─────────────────────────────────────────────────────────────────────────────

func diffCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "diff [file]",
		Short: "Show semantic diff between working tree and HEAD",
		Args:  cobra.MaximumNArgs(1),
		RunE:  runDiff,
	}
}

func runDiff(cmd *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	repo, err := gitrepo.Open(cwd)
	if err != nil {
		return err
	}

	reg := defaultRegistry()

	if len(args) == 1 {
		return diffFile(repo, reg, cleanPath(args[0]))
	}

	changed, err := repo.ChangedFiles()
	if err != nil {
		return err
	}
	if len(changed) == 0 {
		fmt.Println("no changes")
		return nil
	}
	for _, path := range changed {
		if err := diffFile(repo, reg, path); err != nil {
			fmt.Fprintf(os.Stderr, "forge: %s: %v\n", path, err)
		}
	}
	return nil
}

func cleanPath(p string) string {
	return filepath.ToSlash(filepath.Clean(p))
}

func diffFile(repo *gitrepo.Repo, reg *handler.Registry, path string) error {
	path = cleanPath(path)
	h, err := reg.Resolve(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: %v\n", err)
		return nil
	}

	if n, ok := h.(interface{ Format() string }); ok && n.Format() == "text" {
		c := exec.Command("git", "diff", "HEAD", "--", path)
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		_ = c.Run()
		return nil
	}

	base, err := repo.BlobAtHEAD(filepath.ToSlash(path))
	if err != nil {
		return fmt.Errorf("reading HEAD blob for %s: %w", path, err)
	}

	head, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading working tree file %s: %w", path, err)
	}

	diff, err := h.Diff(base, head)
	if err != nil {
		return fmt.Errorf("diff %s: %w", path, err)
	}

	renderDiff(path, diff)
	return nil
}

func renderDiff(path string, diff handler.StructuredDiff) {
	if len(diff.Changes) == 0 {
		return
	}
	fmt.Printf("\x1b[1m--- a/%s\n+++ b/%s\x1b[0m\n", path, path)
	renderChanges(diff.Changes, "", "")
}

func renderChanges(changes []handler.DiffChange, connPrefix, contPrefix string) {
	n := len(changes)
	for i, c := range changes {
		isLast := i == n-1
		label := c.Label
		if label == "" {
			label = c.Path
		}

		myConn := connPrefix
		childConn := contPrefix + "  "
		childCont := contPrefix + "  "
		if connPrefix != "" {
			if isLast {
				myConn = contPrefix + "└─ "
				childConn = contPrefix + "   "
				childCont = contPrefix + "   "
			} else {
				myConn = contPrefix + "├─ "
				childConn = contPrefix + "│  "
				childCont = contPrefix + "│  "
			}
		}

		if len(c.Children) > 0 {
			switch c.Kind {
			case handler.Added:
				fmt.Printf("\x1b[32m%s+ [%s] %v\x1b[0m\n", myConn, label, c.After)
				renderChanges(c.Children, childConn, childCont)
			case handler.Removed:
				fmt.Printf("\x1b[31m%s- [%s] %v\x1b[0m\n", myConn, label, c.Before)
				renderChanges(c.Children, childConn, childCont)
			default:
				if connPrefix == "" {
					fmt.Printf("\n%s\n", label)
				} else {
					fmt.Printf("%s%s\n", myConn, label)
				}
				renderChanges(c.Children, childConn, childCont)
			}
		} else {
			switch c.Kind {
			case handler.Added:
				fmt.Printf("\x1b[32m%s+ [%s] %v\x1b[0m\n", myConn, label, c.After)
			case handler.Removed:
				fmt.Printf("\x1b[31m%s- [%s] %v\x1b[0m\n", myConn, label, c.Before)
			case handler.Modified:
				fmt.Printf("\x1b[33m%s%s  %v  →  %v\x1b[0m\n", myConn, label, c.Before, c.After)
			}
		}
	}
}

// ── forge mergetool ──────────────────────────────────────────────────────────────────────────

func mergeToolCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mergetool [file...]",
		Short: "Resolve merge conflicts using the format-appropriate tool",
		Long: `Opens each conflicted file in a resolution tool and checks the result.

For text files: opens in $MERGE_TOOL, $VISUAL, $EDITOR, or the first
available tool from git's built-in list (meld, vimdiff, vim, vi…).
Conflict is resolved when all <<<<<<< markers have been removed.

For binary formats (handlers installed via forge formats add):
prints the semantic conflict paths so you can resolve them in your
tool of choice, then re-export the file and run 'git add <file>'.

After all conflicts are resolved: run 'git add <file>' and 'git commit'.`,
		Args: cobra.ArbitraryArgs,
		RunE: runMergeTool,
	}
}

func runMergeTool(_ *cobra.Command, args []string) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}

	var files []string
	if len(args) > 0 {
		files = args
	} else {
		files, err = findConflictedFiles(cwd)
		if err != nil {
			return err
		}
	}

	if len(files) == 0 {
		fmt.Println("No files need merging.")
		return nil
	}

	reg := defaultRegistry()
	tool := resolveMergeTool()
	resolved, total := 0, len(files)

	for _, f := range files {
		fmt.Printf("\nMerging %s\n", f)

		h, _ := reg.Resolve(f)
		isBinary := h != nil && isBinaryHandler(h)

		if isBinary {
			if resolveInteractive(f, h) {
				resolved++
			}
			continue
		}

		if err := openInMergeTool(tool, f); err != nil {
			fmt.Fprintf(os.Stderr, "forge: could not open %s in %q: %v\n", f, tool, err)
			continue
		}
		if hasConflictMarkers(f) {
			fmt.Printf("%s: conflict markers remain — not resolved\n", f)
		} else {
			fmt.Printf("%s: resolved\n", f)
			resolved++
		}
	}

	fmt.Printf("\n%d/%d file(s) resolved.\n", resolved, total)
	if resolved < total {
		fmt.Println("Run 'git add <file>' and 'git commit' once all conflicts are resolved.")
		os.Exit(1)
	}
	fmt.Println("All conflicts resolved. Run 'git add' and 'git commit' to complete the merge.")
	return nil
}

func findConflictedFiles(root string) ([]string, error) {
	seen := make(map[string]bool)
	var conflicted []string

	out, err := exec.Command("git", "ls-files", "-u", "--format=%(path)").Output()
	if err != nil {
		out, _ = exec.Command("git", "ls-files", "-u").Output()
		for _, line := range strings.Split(string(out), "\n") {
			parts := strings.SplitN(line, "\t", 2)
			if len(parts) == 2 {
				p := strings.TrimSpace(parts[1])
				if p != "" && !seen[p] {
					seen[p] = true
					conflicted = append(conflicted, p)
				}
			}
		}
	} else {
		for _, p := range strings.Split(string(out), "\n") {
			p = strings.TrimSpace(p)
			if p != "" && !seen[p] {
				seen[p] = true
				conflicted = append(conflicted, p)
			}
		}
	}

	r, err := gogit.PlainOpenWithOptions(root, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err == nil {
		if wt, err := r.Worktree(); err == nil {
			if st, err := wt.Status(); err == nil {
				for path := range st {
					if seen[path] {
						continue
					}
					abs := filepath.Join(root, filepath.FromSlash(path))
					if hasConflictMarkers(abs) {
						seen[path] = true
						conflicted = append(conflicted, path)
					}
				}
			}
		}
	}

	sort.Strings(conflicted)
	cleanupForgeConflictSidecars(root)

	return conflicted, nil
}

func cleanupForgeConflictSidecars(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if strings.HasSuffix(d.Name(), ".forge-conflict") {
			os.Remove(p)
		}
		return nil
	})
}

func hasConflictMarkers(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte("<<<<<<<"))
}

// isBinaryHandler returns true for any non-text handler (i.e. an FHR plugin).
func isBinaryHandler(h handler.ForgeHandler) bool {
	n, ok := h.(handler.Namer)
	return ok && n.Format() != "text"
}

func resolveInteractive(path string, h handler.ForgeHandler) bool {
	sidecarPath := path + ".forge-conflict"
	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		if mergeErr := runSemanticMergeFromIndex(path, h); mergeErr != nil {
			fmt.Printf("%s: binary conflict — resolve in your tool and re-export.\n", path)
			fmt.Printf("  (%v)\n", mergeErr)
			fmt.Printf("Run 'git add %s' once ready.\n", path)
			return promptConfirm(fmt.Sprintf("Mark %s as resolved?", path))
		}
		raw, err = os.ReadFile(sidecarPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forge: sidecar still missing after on-the-fly merge: %v\n", err)
			return false
		}
	}

	var sc handler.ConflictSidecar
	if err := json.Unmarshal(raw, &sc); err != nil {
		fmt.Fprintf(os.Stderr, "forge: malformed sidecar for %s: %v\n", path, err)
		return false
	}

	applier, canApply := h.(handler.ConflictApplier)
	if !canApply {
		fmt.Printf("Conflicts in %s:\n", path)
		for _, c := range sc.Conflicts {
			fmt.Printf("  %s\n    current:  %v\n    incoming: %v\n", c.Path, c.Ours, c.Theirs)
		}
		fmt.Printf("Resolve in your tool and re-export, then 'git add %s'.\n", path)
		resolved := promptConfirm(fmt.Sprintf("Mark %s as resolved?", path))
		if resolved {
			cleanupMergeTempFiles(path)
		}
		return resolved
	}

	n := len(sc.Conflicts)
	choices := make([]bool, n)
	idx := 0

	for {
		c := sc.Conflicts[idx]
		dot := func(want bool) string {
			if choices[idx] == want {
				return "●"
			}
			return " "
		}
		fmt.Printf("\n%s  ─  %d conflict(s)   [%d/%d]\n", path, n, idx+1, n)
		fmt.Printf("  path:     %s\n", c.Path)
		fmt.Printf("  ──────────────────────────────────────────\n")
		fmt.Printf("  [c] %s current   %v\n", dot(false), c.Ours)
		fmt.Printf("  [i] %s incoming  %v\n", dot(true), c.Theirs)
		fmt.Printf("\n  c/i = pick · n = next · p = prev · a = apply · q = quit\n")
		fmt.Printf("  > ")

		var input string
		fmt.Scanln(&input)
		switch strings.ToLower(strings.TrimSpace(input)) {
		case "c":
			choices[idx] = false
			if idx < n-1 {
				idx++
			}
		case "i":
			choices[idx] = true
			if idx < n-1 {
				idx++
			}
		case "n":
			if idx < n-1 {
				idx++
			}
		case "p":
			if idx > 0 {
				idx--
			}
		case "a":
			return applyConflictChoices(path, sidecarPath, sc, choices, applier)
		case "q":
			fmt.Println("Aborted — no changes made.")
			return false
		}
	}
}

func applyConflictChoices(path, sidecarPath string, sc handler.ConflictSidecar, choices []bool, applier handler.ConflictApplier) bool {
	fmt.Printf("\nSummary for %s:\n", path)
	var takePaths []string
	for i, c := range sc.Conflicts {
		label, val := "current ", c.Ours
		if choices[i] {
			label, val = "incoming", c.Theirs
			takePaths = append(takePaths, c.Path)
		}
		fmt.Printf("  [%d/%d]  %-45s → %s  %v\n", i+1, len(sc.Conflicts), c.Path, label, val)
	}
	if !promptConfirm("Apply these choices?") {
		fmt.Println("Aborted — no changes made.")
		return false
	}

	theirsBlob, err := base64.StdEncoding.DecodeString(sc.TheirsB64)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: could not decode theirs blob: %v\n", err)
		return false
	}
	merged, err := os.ReadFile(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: could not read %s: %v\n", path, err)
		return false
	}
	result, err := applier.ApplyChoices(merged, theirsBlob, takePaths)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: ApplyChoices failed: %v\n", err)
		return false
	}
	if err := os.WriteFile(path, result, 0644); err != nil {
		fmt.Fprintf(os.Stderr, "forge: could not write %s: %v\n", path, err)
		return false
	}
	_ = os.Remove(sidecarPath)
	cleanupMergeTempFiles(path)
	fmt.Printf("%s: resolved.\n", path)
	return true
}

func cleanupMergeTempFiles(path string) {
	dir := filepath.Dir(path)
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	ext := filepath.Ext(path)
	for _, tag := range []string{"_BASE_", "_LOCAL_", "_REMOTE_", "_BACKUP_"} {
		pattern := filepath.Join(dir, base+tag+"*"+ext)
		matches, _ := filepath.Glob(pattern)
		for _, m := range matches {
			os.Remove(m)
		}
	}
}

func promptConfirm(prompt string) bool {
	fmt.Printf("%s [Y/n] ", prompt)
	var answer string
	fmt.Scanln(&answer)
	a := strings.ToLower(strings.TrimSpace(answer))
	return a == "" || a == "y"
}

func runSemanticMergeFromIndex(path string, h handler.ForgeHandler) error {
	readStage := func(stage int) ([]byte, error) {
		data, err := exec.Command("git", "show", fmt.Sprintf(":%d:%s", stage, path)).Output()
		if err != nil {
			return nil, fmt.Errorf("git show :%d:%s: %w", stage, path, err)
		}
		return data, nil
	}

	base, err := readStage(1)
	if err != nil {
		base = nil
	}
	ours, err := readStage(2)
	if err != nil {
		return err
	}
	theirs, err := readStage(3)
	if err != nil {
		return err
	}

	merged, ci, err := h.Merge(base, ours, theirs)
	if err != nil {
		return fmt.Errorf("semantic merge: %w", err)
	}

	if err := os.WriteFile(path, merged, 0644); err != nil {
		return fmt.Errorf("writing merged result: %w", err)
	}

	if ci != nil && len(ci.Conflicts) > 0 {
		format := "unknown"
		if n, ok := h.(interface{ Format() string }); ok {
			format = n.Format()
		}
		sc := handler.ConflictSidecar{
			Handler:   format,
			Conflicts: ci.Conflicts,
			TheirsB64: base64.StdEncoding.EncodeToString(theirs),
		}
		if data, err := json.MarshalIndent(sc, "", "  "); err == nil {
			_ = os.WriteFile(path+".forge-conflict", data, 0644)
		}
	}
	return nil
}

func resolveMergeTool() string {
	for _, env := range []string{"MERGE_TOOL", "GIT_MERGETOOL", "VISUAL", "EDITOR"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	builtins := []string{
		"meld", "opendiff", "kdiff3", "tkdiff", "xxdiff",
		"tortoisemerge", "gvimdiff", "diffuse", "diffmerge",
		"ecmerge", "p4merge", "araxis", "bc", "codecompare",
		"smerge", "emerge", "nvimdiff", "nvim", "vimdiff", "vim", "vi",
	}
	for _, t := range builtins {
		if _, err := exec.LookPath(t); err == nil {
			return t
		}
	}
	return "vi"
}

var threeWayTools = map[string]bool{
	"meld": true, "kdiff3": true, "xxdiff": true, "diffuse": true,
	"tkdiff": true, "diffmerge": true, "bc": true, "bcompare": true,
}

func openInMergeTool(tool, path string) error {
	args := []string{path}
	var cleanup []string

	if threeWayTools[filepath.Base(tool)] {
		local, remote, err := extractConflictVersions(path)
		if err == nil {
			args = []string{local, path, remote}
			cleanup = []string{local, remote}
		}
	}

	c := exec.Command(tool, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	err := c.Run()
	for _, f := range cleanup {
		os.Remove(f)
	}
	return err
}

func extractConflictVersions(path string) (localFile, remoteFile string, err error) {
	localData, err := exec.Command("git", "show", ":2:"+path).Output()
	if err != nil {
		return "", "", fmt.Errorf("git show :2:%s: %w", path, err)
	}
	remoteData, err := exec.Command("git", "show", ":3:"+path).Output()
	if err != nil {
		return "", "", fmt.Errorf("git show :3:%s: %w", path, err)
	}

	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	ext := filepath.Ext(path)

	lf, err := os.CreateTemp("", "forge-LOCAL-"+base+"-*"+ext)
	if err != nil {
		return "", "", err
	}
	defer lf.Close()
	if _, err = lf.Write(localData); err != nil {
		os.Remove(lf.Name())
		return "", "", err
	}

	rf, err := os.CreateTemp("", "forge-REMOTE-"+base+"-*"+ext)
	if err != nil {
		os.Remove(lf.Name())
		return "", "", err
	}
	defer rf.Close()
	if _, err = rf.Write(remoteData); err != nil {
		os.Remove(lf.Name())
		os.Remove(rf.Name())
		return "", "", err
	}

	return lf.Name(), rf.Name(), nil
}

// ── forge merge ────────────────────────────────────────────────────────────────────────────

func mergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "merge",
		Short:              "Merge a branch, using Forge handlers for supported formats",
		DisableFlagParsing: true,
		RunE:               runMerge,
	}
}

func runMerge(_ *cobra.Command, args []string) error {
	isAbort := false
	for _, a := range args {
		if a == "--abort" {
			isAbort = true
			break
		}
	}

	c := exec.Command("git", append([]string{"merge"}, args...)...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	err := c.Run()

	if isAbort {
		cleanupForgeConflictSidecars(".")
		return err
	}

	if err != nil {
		fmt.Fprintln(os.Stderr, "\nTo resolve conflicts run: forge mergetool")
		fmt.Fprintln(os.Stderr, "Then: git add <files> && git commit")
		os.Exit(c.ProcessState.ExitCode())
	}
	return nil
}

// ── forge merge-file ───────────────────────────────────────────────────────────────────────────

func mergeFileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge-file <base> <ours> <theirs>",
		Short: "3-way merge three files using the format handler (like git merge-file)",
		Long: `Performs a 3-way merge of BASE, OURS, and THEIRS using the appropriate
format handler. The result is written back to OURS, matching git merge-file behaviour.

Exits 0 on a clean merge, 1 if there are conflicts.`,
		Args: cobra.RangeArgs(3, 4),
		RunE: runMergeFile,
	}
}

func runMergeFile(_ *cobra.Command, args []string) error {
	basePath, oursPath, theirsPath := cleanPath(args[0]), cleanPath(args[1]), cleanPath(args[2])
	sidecarBase := oursPath
	if len(args) == 4 {
		sidecarBase = cleanPath(args[3])
	}

	base, err := os.ReadFile(basePath)
	if err != nil {
		return fmt.Errorf("reading base %s: %w", basePath, err)
	}
	ours, err := os.ReadFile(oursPath)
	if err != nil {
		return fmt.Errorf("reading ours %s: %w", oursPath, err)
	}
	theirs, err := os.ReadFile(theirsPath)
	if err != nil {
		return fmt.Errorf("reading theirs %s: %w", theirsPath, err)
	}

	reg := defaultRegistry()
	h, err := reg.Resolve(oursPath)
	if err != nil {
		return err
	}

	merged, ci, err := h.Merge(base, ours, theirs)
	if err != nil {
		return fmt.Errorf("merge failed: %w", err)
	}

	if err := os.WriteFile(oursPath, merged, 0644); err != nil {
		return fmt.Errorf("writing result to %s: %w", oursPath, err)
	}

	if ci != nil && len(ci.Conflicts) > 0 {
		format := "unknown"
		if n, ok := h.(interface{ Format() string }); ok {
			format = n.Format()
		}
		sidecar := handler.ConflictSidecar{
			Handler:   format,
			Conflicts: ci.Conflicts,
			TheirsB64: base64.StdEncoding.EncodeToString(theirs),
		}
		if data, err := json.MarshalIndent(sidecar, "", "  "); err == nil {
			_ = os.WriteFile(sidecarBase+".forge-conflict", data, 0644)
		}

		fmt.Fprintf(os.Stderr, "CONFLICT: %d conflict(s) in %s\n", len(ci.Conflicts), oursPath)
		for _, c := range ci.Conflicts {
			fmt.Fprintf(os.Stderr, "  %s\n", c.Path)
		}
		os.Exit(1)
	}

	fmt.Printf("Merged cleanly into %s\n", oursPath)
	return nil
}

// ── forge source ───────────────────────────────────────────────────────────────────────────

func sourceCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "source",
		Short: "Manage FHR handler sources",
	}
	cmd.AddCommand(sourceAddCmd(), sourceListCmd(), sourceUpdateCmd())
	return cmd
}

func sourceAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <url>",
		Short: "Add a handler source and fetch its manifest",
		Args:  cobra.ExactArgs(1),
		RunE:  runSourceAdd,
	}
	cmd.Flags().String("name", "", "Short name for this source (default: derived from URL)")
	return cmd
}

func runSourceAdd(cmd *cobra.Command, args []string) error {
	rawURL := args[0]
	name, _ := cmd.Flags().GetString("name")
	if name == "" {
		name = deriveSourceName(rawURL)
	}

	fmt.Printf("Fetching manifest from %s...\n", rawURL)
	m, err := fhr.FetchManifest(rawURL)
	if err != nil {
		return err
	}

	if err := fhr.AddSource(name, rawURL); err != nil {
		return err
	}

	var exts []string
	for ext := range m.Formats {
		exts = append(exts, ext)
	}
	sort.Strings(exts)

	fmt.Printf("Added source %q (%s)\n", name, m.Name)
	fmt.Printf("Available formats: %s\n", strings.Join(exts, ", "))
	fmt.Printf("Install a handler with: forge formats add <extension>\n")
	return nil
}

func deriveSourceName(rawURL string) string {
	parts := strings.Split(strings.TrimRight(rawURL, "/"), "/")
	for i := len(parts) - 1; i >= 0; i-- {
		p := parts[i]
		if p != "" && p != "manifest.toml" && p != "main" && p != "master" {
			return p
		}
	}
	return "source"
}

func sourceListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List configured handler sources",
		RunE: func(_ *cobra.Command, _ []string) error {
			sources, err := fhr.LoadSources()
			if err != nil {
				return err
			}
			if len(sources) == 0 {
				fmt.Println("No sources configured.")
				fmt.Println("Add one with: forge source add <url>")
				return nil
			}
			for _, s := range sources {
				fmt.Printf("%-20s %s\n", s.Name, s.URL)
			}
			return nil
		},
	}
}

func sourceUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "update [name]",
		Short: "Re-fetch manifests to verify sources are reachable",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(_ *cobra.Command, args []string) error {
			sources, err := fhr.LoadSources()
			if err != nil {
				return err
			}
			if len(sources) == 0 {
				fmt.Println("No sources configured.")
				return nil
			}
			for _, s := range sources {
				if len(args) == 1 && s.Name != args[0] {
					continue
				}
				fmt.Printf("Checking %s (%s)... ", s.Name, s.URL)
				if _, err := fhr.FetchManifest(s.URL); err != nil {
					fmt.Printf("ERROR: %v\n", err)
				} else {
					fmt.Println("OK")
				}
			}
			return nil
		},
	}
}

// ── forge formats ────────────────────────────────────────────────────────────────────────────

func formatsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "formats",
		Short: "Manage installed format handlers",
	}
	cmd.AddCommand(formatsAddCmd(), formatsListCmd())
	return cmd
}

func formatsAddCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "add <extension>",
		Short: "Download and install a handler for a file extension",
		Args:  cobra.ExactArgs(1),
		RunE:  runFormatsAdd,
	}
	cmd.Flags().String("source", "", "Source name to search (default: all configured sources)")
	return cmd
}

func runFormatsAdd(cmd *cobra.Command, args []string) error {
	ext := args[0]
	if !strings.HasPrefix(ext, ".") {
		ext = "." + ext
	}
	sourceName, _ := cmd.Flags().GetString("source")

	sources, err := fhr.LoadSources()
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("no sources configured — run: forge source add <url>")
	}
	if sourceName != "" {
		var found fhr.Source
		for _, s := range sources {
			if s.Name == sourceName {
				found = s
				break
			}
		}
		if found.Name == "" {
			return fmt.Errorf("source %q not found — run: forge source list", sourceName)
		}
		sources = []fhr.Source{found}
	}

	for _, src := range sources {
		m, err := fhr.FetchManifest(src.URL)
		if err != nil {
			fmt.Fprintf(os.Stderr, "forge: warning: could not fetch source %q: %v\n", src.Name, err)
			continue
		}
		handlerID, version, err := m.HandlerForExt(ext)
		if err != nil {
			continue
		}
		fmt.Printf("Found handler %q v%s in source %q\n", handlerID, version, src.Name)
		binary, err := fhr.DownloadHandler(m, handlerID, version, src.URL)
		if err != nil {
			return err
		}
		fmt.Printf("Installed: %s\n", binary)
		return nil
	}
	return fmt.Errorf("no handler found for %s in any configured source", ext)
}

func formatsListCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List installed format handlers",
		RunE: func(_ *cobra.Command, _ []string) error {
			handlers := fhr.LoadInstalledHandlers()
			if len(handlers) == 0 {
				fmt.Println("No external handlers installed.")
				fmt.Println("Install one with: forge formats add <extension>")
				return nil
			}
			fmt.Printf("%-20s %-10s %s\n", "HANDLER", "VERSION", "FORMATS")
			for _, h := range handlers {
				sort.Strings(h.Formats)
				fmt.Printf("%-20s %-10s %s\n", h.ID, h.Version, strings.Join(h.Formats, ", "))
			}
			return nil
		},
	}
}

// ── git pass-throughs ────────────────────────────────────────────────────────────────────────────

func gitPassthrough(name, short string) *cobra.Command {
	return &cobra.Command{
		Use:                name,
		Short:              short,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			c := exec.Command("git", append([]string{name}, args...)...)
			c.Stdin = os.Stdin
			c.Stdout = os.Stdout
			c.Stderr = os.Stderr
			return c.Run()
		},
	}
}

func logCmd() *cobra.Command {
	return gitPassthrough("log", "Show commit log (delegates to git)")
}

func pushCmd() *cobra.Command {
	return gitPassthrough("push", "Push to remote (delegates to git)")
}

func pullCmd() *cobra.Command {
	return gitPassthrough("pull", "Pull from remote (delegates to git)")
}

func delegateToGit(sub string) func(*cobra.Command, []string) error {
	return func(_ *cobra.Command, args []string) error {
		c := exec.Command("git", append([]string{sub}, args...)...)
		c.Stdin = os.Stdin
		c.Stdout = os.Stdout
		c.Stderr = os.Stderr
		return c.Run()
	}
}
