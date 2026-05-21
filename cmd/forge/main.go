package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
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
		logCmd(),
		pushCmd(),
		pullCmd(),
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

	forgeDir := filepath.Join(dir, ".forge")
	if err := os.MkdirAll(forgeDir, 0755); err != nil {
		return fmt.Errorf("creating .forge/: %w", err)
	}

	handlersPath := filepath.Join(forgeDir, "handlers")
	if _, err := os.Stat(handlersPath); os.IsNotExist(err) {
		template := "# .forge/handlers\n" +
			"# Declare which domains this repository requires.\n" +
			"# Official domains (3d, image, text, audio, video) ship with Forge.\n" +
			"# Community domains need a registry entry.\n" +
			"#\n" +
			"# [domains]\n" +
			"# require = [\"3d\", \"image\"]\n" +
			"#\n" +
			"# [community.audio]\n" +
			"# registry = \"https://forge-audio.example.com\"\n" +
			"# version  = \"1.0.0\"\n"
		if err := os.WriteFile(handlersPath, []byte(template), 0644); err != nil {
			return fmt.Errorf("creating .forge/handlers: %w", err)
		}
	}

	abs, _ := filepath.Abs(dir)
	fmt.Printf("Initialized Forge repository in %s\n", abs)
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

	for _, p := range paths {
		fs := st[p]
		label := handlerLabel(p, reg)
		fmt.Printf("%c%c  %-45s %s\n", rune(fs.Staging), rune(fs.Worktree), p, label)
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
		return diffFile(repo, reg, args[0])
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

func diffFile(repo *gitrepo.Repo, reg *handler.Registry, path string) error {
	h, err := reg.Resolve(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "forge: %v\n", err)
		return nil // degrade gracefully, don't abort
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

// ── forge merge ───────────────────────────────────────────────────────────────

func mergeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "merge <branch>",
		Short: "Merge a branch with semantic conflict resolution (M3)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("forge merge is not yet implemented (planned for M3)")
		},
	}
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
	basePath, oursPath, theirsPath := args[0], args[1], args[2]

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

func logCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "log",
		Short: "Show commit log with format-aware metadata (M2)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("forge log is not yet implemented (planned for M2)")
		},
	}
}

// ── forge push / pull ─────────────────────────────────────────────────────────

func pushCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "push",
		Short:              "Push to remote (delegates to git)",
		DisableFlagParsing: true,
		RunE:               delegateToGit("push"),
	}
}

func pullCmd() *cobra.Command {
	return &cobra.Command{
		Use:                "pull",
		Short:              "Pull from remote (delegates to git)",
		DisableFlagParsing: true,
		RunE:               delegateToGit("pull"),
	}
}

func delegateToGit(sub string) func(*cobra.Command, []string) error {
	return func(cmd *cobra.Command, args []string) error {
		argv := append([]string{sub}, args...)
		proc, err := os.StartProcess("/usr/bin/git", append([]string{"git"}, argv...), &os.ProcAttr{
			Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
		})
		if err != nil {
			return fmt.Errorf("could not start git: %w", err)
		}
		state, err := proc.Wait()
		if err != nil {
			return err
		}
		os.Exit(state.ExitCode())
		return nil
	}
}
