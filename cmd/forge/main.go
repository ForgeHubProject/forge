package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yakupatahanov/forge/internal/gitrepo"
	"github.com/yakupatahanov/forge/internal/handler"
	"github.com/yakupatahanov/forge/internal/handler/text"
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
		diffCmd(),
		mergeCmd(),
		logCmd(),
		pushCmd(),
		pullCmd(),
	)
	return root
}

// defaultRegistry builds the handler registry with official handlers.
// Handlers are registered most-specific first; TextHandler is the catch-all.
func defaultRegistry() *handler.Registry {
	reg := handler.NewRegistry()
	reg.Register(text.New())
	return reg
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
	renderChanges(diff.Changes, 0)
}

func renderChanges(changes []handler.DiffChange, depth int) {
	indent := strings.Repeat("  ", depth)
	for _, c := range changes {
		label := c.Label
		if label == "" {
			label = c.Path
		}
		switch c.Kind {
		case handler.Added:
			fmt.Printf("\x1b[32m%s+ [%s] %v\x1b[0m\n", indent, label, c.After)
		case handler.Removed:
			fmt.Printf("\x1b[31m%s- [%s] %v\x1b[0m\n", indent, label, c.Before)
		case handler.Modified:
			fmt.Printf("\x1b[33m%s~ [%s] %v → %v\x1b[0m\n", indent, label, c.Before, c.After)
		}
		if len(c.Children) > 0 {
			renderChanges(c.Children, depth+1)
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
