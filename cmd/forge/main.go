package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
	gogithttp "github.com/go-git/go-git/v5/plumbing/transport/http"
	gogitssh "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/spf13/cobra"
	"github.com/yakupatahanov/forge/internal/gitrepo"
	"github.com/yakupatahanov/forge/internal/handler"
	"github.com/yakupatahanov/forge/internal/handler/domain"
	forgegltf "github.com/yakupatahanov/forge/internal/handler/gltf"
	"github.com/yakupatahanov/forge/internal/handler/text"
	"github.com/yakupatahanov/forge/internal/manifest"
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

// defaultRegistry builds the handler registry with official domains and handlers.
// Domains are checked before the TextHandler catch-all.
// Within each domain, specific handlers are registered most-specific first.
func defaultRegistry() *handler.Registry {
	reg := handler.NewRegistry()

	threeD := domain.NewThreeD()
	threeD.DomainRegister(forgegltf.New())
	reg.Register(threeD)

	reg.Register(domain.NewImage())
	reg.Register(text.New()) // catch-all — must be last
	return reg
}

// ── forge init ────────────────────────────────────────────────────────────────

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

// setupGitMergeDriver writes .gitattributes entries for supported binary formats
// and registers the [merge "forge"] driver in .git/config so that git merge
// automatically calls forge merge-file for those formats.
func setupGitMergeDriver(repoDir string) error {
	// .gitattributes — append entries that aren't already present
	attrPath := filepath.Join(repoDir, ".gitattributes")
	existing, _ := os.ReadFile(attrPath)
	entries := []string{
		"*.glb  merge=forge",
		"*.gltf merge=forge",
	}
	var toAdd []string
	for _, e := range entries {
		if !bytes.Contains(existing, []byte(e[:6])) { // match on extension prefix
			toAdd = append(toAdd, e)
		}
	}
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

	// .git/config — register [merge "forge"] driver once
	gitConfigPath := filepath.Join(repoDir, ".git", "config")
	gitConfig, _ := os.ReadFile(gitConfigPath)
	if !bytes.Contains(gitConfig, []byte(`[merge "forge"]`)) {
		f, err := os.OpenFile(gitConfigPath, os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		fmt.Fprintf(f, "\n[merge \"forge\"]\n\tname = Forge semantic merge\n\tdriver = forge merge-file %%O %%A %%B\n")
		f.Close()
	}

	// .gitignore — add safety-net entries for git mergetool temp files and
	// vim swap files so they never accidentally get staged.
	ignorePath := filepath.Join(repoDir, ".gitignore")
	ignoreExisting, _ := os.ReadFile(ignorePath)
	ignoreEntries := []string{
		"*.swp",
		"*_BASE_*.glb",
		"*_LOCAL_*.glb",
		"*_REMOTE_*.glb",
		"*_BACKUP_*.glb",
	}
	var ignoreToAdd []string
	for _, e := range ignoreEntries {
		if !bytes.Contains(ignoreExisting, []byte(e)) {
			ignoreToAdd = append(ignoreToAdd, e)
		}
	}
	if len(ignoreToAdd) > 0 {
		f, err := os.OpenFile(ignorePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
		if err != nil {
			return err
		}
		if len(ignoreExisting) > 0 && !bytes.HasSuffix(ignoreExisting, []byte("\n")) {
			fmt.Fprintln(f)
		}
		fmt.Fprintln(f, "# Forge / git mergetool temp files")
		for _, e := range ignoreToAdd {
			fmt.Fprintln(f, e)
		}
		f.Close()
	}

	return nil
}

// ── forge status ──────────────────────────────────────────────────────────────

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

	// Show current branch.
	if head, err := r.Head(); err == nil {
		if head.Name().IsBranch() {
			fmt.Printf("On branch \x1b[1m%s\x1b[0m\n", head.Name().Short())
		} else {
			fmt.Printf("HEAD detached at %s\n", head.Hash().String()[:7])
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

	// Partition into three buckets (a file can appear in staged AND unstaged).
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
// Format:
//
//	[3d › gltf]   — domain + specific handler
//	[3d]          — domain fallback (no specific handler installed)
//	[text]        — standalone catch-all
func handlerLabel(path string, reg *handler.Registry) string {
	dom, h, err := reg.ResolveFull(path)
	if err != nil {
		return "\x1b[31m[no handler]\x1b[0m"
	}

	handlerName := ""
	if n, ok := h.(handler.Namer); ok {
		handlerName = n.Format()
	}

	if dom != nil {
		domName := dom.Format()
		if h == handler.ForgeHandler(dom) {
			ext := strings.ToLower(filepath.Ext(path))
			return fmt.Sprintf("\x1b[33m[%s — no %s handler]\x1b[0m", domName, ext)
		}
		return fmt.Sprintf("\x1b[36m[%s › %s]\x1b[0m", domName, handlerName)
	}

	// Standalone handler (e.g. TextHandler).
	return fmt.Sprintf("\x1b[36m[%s]\x1b[0m", handlerName)
}

// ── forge clone ───────────────────────────────────────────────────────────────

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

// buildCloneOptions constructs CloneOptions with the appropriate auth method.
// For SSH URLs: tries the SSH agent first (same as git), then falls back to
// reading the key file directly. For HTTPS: uses a token if provided.
// Public repos need no auth.
func buildCloneOptions(rawURL, token, sshKeyPath, sshPassword string) (*gogit.CloneOptions, error) {
	opts := &gogit.CloneOptions{URL: rawURL}

	if isSSHURL(rawURL) {
		// Try SSH agent first — this is how git works and means no passphrase
		// is needed as long as the key is loaded in the agent.
		if agent, err := gogitssh.NewSSHAgentAuth("git"); err == nil {
			opts.Auth = agent
			return opts, nil
		}

		// Agent unavailable — fall back to key file.
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

	// HTTPS: prefer explicit flag, then env vars.
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
	// SCP-style: git@github.com:user/repo.git
	if strings.HasPrefix(rawURL, "git@") {
		return true
	}
	u, err := url.Parse(rawURL)
	return err == nil && u.Scheme == "ssh"
}

// defaultSSHKey returns ~/.ssh/id_ed25519 if it exists, else ~/.ssh/id_rsa.
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

// reportMissingHandlers reads .forge/handlers and reports any required domains
// that are not covered by the currently installed registry.
func reportMissingHandlers(repoDir string) {
	m, err := manifest.LoadHandlers(repoDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: warning: could not read .forge/handlers: %v\n", err)
		return
	}

	reg := defaultRegistry()
	installedDomains := map[string]bool{}
	for _, d := range reg.Domains() {
		installedDomains[d.Format()] = true
	}

	var missing []string

	// Official domains listed in [domains] require = [...]
	for _, name := range m.Domains.Require {
		if !installedDomains[name] {
			missing = append(missing, fmt.Sprintf("  %-10s  (official)  forge domain install %s", name, name))
		}
	}

	// Community domains listed in [community]
	for name, src := range m.Community {
		if !installedDomains[name] {
			missing = append(missing, fmt.Sprintf(
				"  %-10s  %s@%s\n              forge domain install %s --registry %s",
				name, name, src.Version, name, src.Registry,
			))
		}
	}

	if len(missing) == 0 {
		return
	}

	sort.Strings(missing)
	fmt.Println()
	fmt.Println("This repository requires domains that are not installed:")
	for _, m := range missing {
		fmt.Println(m)
	}
	fmt.Println()
	fmt.Println("(forge domain install is available in M4)")
}

// repoNameFromURL derives a local directory name from a clone URL.
func repoNameFromURL(url string) string {
	// Strip trailing slash and .git suffix.
	url = strings.TrimRight(url, "/")
	url = strings.TrimSuffix(url, ".git")
	// Take the last path segment (works for https:// and git@host:user/repo).
	if i := strings.LastIndexAny(url, "/:"); i >= 0 {
		url = url[i+1:]
	}
	if url == "" {
		return "repo"
	}
	return url
}

// ── forge diff ────────────────────────────────────────────────────────────────

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

	// No file given: diff all changed files.
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

// cleanPath normalises a user-supplied file path for use with go-git and git
// commands. It removes leading ./ and .\ (common from PowerShell tab-completion)
// and converts backslashes to forward slashes.
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

	// Text files: delegate to git diff for familiar unified-diff output.
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

// renderChanges renders a slice of DiffChanges with optional tree connectors.
// connPrefix is prepended to each item in this slice (includes the connector
// character); contPrefix is the prefix for continuation / child lines.
// At the top level both are empty, giving flat output compatible with the
// text handler's existing format.
func renderChanges(changes []handler.DiffChange, connPrefix, contPrefix string) {
	n := len(changes)
	for i, c := range changes {
		isLast := i == n-1
		label := c.Label
		if label == "" {
			label = c.Path
		}

		// Compute connector for nested items.
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
			// Section / group node: print label as a plain header, then recurse.
			if connPrefix == "" {
				fmt.Printf("\n%s\n", label)
			} else {
				fmt.Printf("%s%s\n", myConn, label)
			}
			renderChanges(c.Children, childConn, childCont)
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

// ── forge mergetool ───────────────────────────────────────────────────────────

func mergeToolCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "mergetool [file...]",
		Short: "Resolve merge conflicts using the format-appropriate tool",
		Long: `Opens each conflicted file in a resolution tool and checks the result.

For text files: opens in $MERGE_TOOL, $VISUAL, $EDITOR, or the first
available tool from git's built-in list (meld, vimdiff, vim, vi…).
Conflict is resolved when all
<<<<<<< markers have been removed.

For binary formats (.glb, .gltf): prints the semantic conflict paths
so you can resolve them in your DCC tool (Blender, etc.), then re-export
the file and run 'git add <file>'.

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

		// Text: open in editor, check markers after.
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

// findConflictedFiles returns paths of files that have unresolved merge conflicts.
// It detects both text files (<<<<<<< markers) and binary files (unmerged index
// entries from git ls-files -u), so forge mergetool works even when the forge
// merge driver was not the one that ran.
func findConflictedFiles(root string) ([]string, error) {
	seen := make(map[string]bool)
	var conflicted []string

	// 1. Binary / any format: ask git which files have unmerged index entries.
	out, err := exec.Command("git", "ls-files", "-u", "--format=%(path)").Output()
	if err != nil {
		// older git may not support --format; fall back to parsing raw output
		out, _ = exec.Command("git", "ls-files", "-u").Output()
		for _, line := range strings.Split(string(out), "\n") {
			// raw format: "<mode> <hash> <stage>\t<path>"
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

	// 2. Text files not caught above: scan for <<<<<<< markers in working tree.
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
	return conflicted, nil
}

// hasConflictMarkers reports whether a file contains git conflict marker lines.
func hasConflictMarkers(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return bytes.Contains(data, []byte("<<<<<<<"))
}

// isBinaryHandler returns true for handlers that produce binary output (not
// text conflict markers), meaning resolution must happen in an external tool.
func isBinaryHandler(h handler.ForgeHandler) bool {
	n, ok := h.(handler.Namer)
	if !ok {
		return false
	}
	switch n.Format() {
	case "gltf", "3d", "image":
		return true
	}
	return false
}

// resolveInteractive drives the conflict-by-conflict prompt for binary formats.
// Returns true when the file is fully resolved and written back.
func resolveInteractive(path string, h handler.ForgeHandler) bool {
	sidecarPath := path + ".forge-conflict"
	raw, err := os.ReadFile(sidecarPath)
	if err != nil {
		// No sidecar — forge merge-file wasn't the merge driver (e.g. plain git
		// merge on a binary). Run the semantic merge now using the index stages.
		if mergeErr := runSemanticMergeFromIndex(path, h); mergeErr != nil {
			fmt.Printf("%s: binary conflict — resolve in your DCC tool and re-export.\n", path)
			fmt.Printf("  (%v)\n", mergeErr)
			fmt.Printf("Run 'git add %s' once ready.\n", path)
			return promptConfirm(fmt.Sprintf("Mark %s as resolved?", path))
		}
		// Sidecar has now been written; read it.
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
		// Handler exists but doesn't support ApplyChoices — print and ask to confirm.
		fmt.Printf("Conflicts in %s:\n", path)
		for _, c := range sc.Conflicts {
			fmt.Printf("  %s\n    current:  %v\n    incoming: %v\n", c.Path, c.Ours, c.Theirs)
		}
		fmt.Printf("Resolve in your DCC tool and re-export, then 'git add %s'.\n", path)
		resolved := promptConfirm(fmt.Sprintf("Mark %s as resolved?", path))
		if resolved {
			cleanupMergeTempFiles(path)
		}
		return resolved
	}

	n := len(sc.Conflicts)
	// choices[i] == true → take theirs (incoming); false → keep ours (current, the default)
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

// applyConflictChoices prints a summary, asks for confirmation, then patches
// the merged file and removes the sidecar.
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

// cleanupMergeTempFiles removes the _BASE_PID, _LOCAL_PID, _REMOTE_PID, and
// _BACKUP_PID temp files that git's merge driver infrastructure leaves in the
// working directory when a merge driver exits with a non-zero status.
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

// runSemanticMergeFromIndex extracts the :1: (base), :2: (ours), :3: (theirs)
// versions of path from the git index, runs the format handler's Merge(), and
// writes the merged result plus a .forge-conflict sidecar — exactly what
// forge merge-file would have done had it been the configured merge driver.
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
		return err
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

// resolveMergeTool returns the merge tool to use.
// Precedence: MERGE_TOOL → GIT_MERGETOOL → VISUAL → EDITOR →
// first available tool from git's built-in auto-detection list → "vi".
func resolveMergeTool() string {
	for _, env := range []string{"MERGE_TOOL", "GIT_MERGETOOL", "VISUAL", "EDITOR"} {
		if v := os.Getenv(env); v != "" {
			return v
		}
	}
	// Mirror git's built-in auto-detection order.
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

// threeWayTools lists GUI diff tools that accept positional (local, merged, remote) args
// and render a proper 3-pane conflict view when given all three.
var threeWayTools = map[string]bool{
	"meld": true, "kdiff3": true, "xxdiff": true, "diffuse": true,
	"tkdiff": true, "diffmerge": true, "bc": true, "bcompare": true,
}

// openInMergeTool opens path in the given tool. For 3-way capable tools it
// extracts LOCAL and REMOTE from git's index and passes all three files so
// the tool shows a proper conflict view instead of raw markers.
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

// extractConflictVersions writes the :2: (LOCAL/ours) and :3: (REMOTE/theirs)
// index versions of path to temp files, returning their paths.
// The caller is responsible for removing the files when done.
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

// ── forge merge ───────────────────────────────────────────────────────────────

func mergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "merge",
		Short:              "Merge a branch, using Forge handlers for supported formats",
		DisableFlagParsing: true,
		RunE:               runMerge,
	}
}

func runMerge(_ *cobra.Command, args []string) error {
	c := exec.Command("git", append([]string{"merge"}, args...)...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	err := c.Run()
	if err != nil {
		// git merge exited non-zero — conflicts or error.
		// Print a forge-aware hint on top of git's own output.
		fmt.Fprintln(os.Stderr, "\nTo resolve conflicts run: forge mergetool")
		fmt.Fprintln(os.Stderr, "Then: git add <files> && git commit")
		os.Exit(c.ProcessState.ExitCode())
	}
	return nil
}

// ── forge merge-file ──────────────────────────────────────────────────────────

func mergeFileCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge-file <base> <ours> <theirs>",
		Short: "3-way merge three files using the format handler (like git merge-file)",
		Long: `Performs a 3-way merge of BASE, OURS, and THEIRS using the appropriate
format handler. The result is written back to OURS, matching git merge-file behaviour.

Exits 0 on a clean merge, 1 if there are conflicts (conflict markers are
written into OURS so you can inspect and resolve them).`,
		Args: cobra.ExactArgs(3),
		RunE: runMergeFile,
	}
}

func runMergeFile(_ *cobra.Command, args []string) error {
	basePath, oursPath, theirsPath := cleanPath(args[0]), cleanPath(args[1]), cleanPath(args[2])

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
		// Write structured sidecar so forge mergetool can drive interactive resolution.
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
			_ = os.WriteFile(oursPath+".forge-conflict", data, 0644)
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

// ── forge log ─────────────────────────────────────────────────────────────────

// ── git pass-throughs ─────────────────────────────────────────────────────────
// gitPassthrough returns a cobra.Command that forwards all arguments verbatim
// to the equivalent git sub-command. DisableFlagParsing lets flags like
// --oneline or -m pass through without cobra trying to interpret them.

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

// ── forge push / pull ─────────────────────────────────────────────────────────

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

