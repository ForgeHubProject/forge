package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/forgehubproject/forge/internal/handler"
	"github.com/spf13/cobra"
)

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
	repoDir := forgerepo.FindRoot()
	if _, err := forgerepo.GitOutput(repoDir, "rev-parse", "--git-dir"); err != nil {
		return fmt.Errorf("not a git repository")
	}

	dashAt := cmd.ArgsLenAtDash()
	revs, rawPaths, err := forgerepo.SplitRevsAndPaths(repoDir, args, dashAt)
	if err != nil {
		return err
	}
	// The first argument is a revision by definition here, so one git cannot
	// resolve is reported as the bad revision it was meant to be even where a
	// file of that name exists, which is the one reading SplitRevsAndPaths prefers
	// and forge show has no use for.
	if len(revs) == 0 && dashAt != 0 {
		return fmt.Errorf("not a valid revision: %s", args[0])
	}
	if len(revs) != 1 {
		return fmt.Errorf("forge show takes exactly one revision")
	}
	commit, err := forgerepo.ResolveRev(repoDir, revs[0])
	if err != nil {
		return err
	}
	paths, err := forgerepo.RelPaths(repoDir, rawPaths)
	if err != nil {
		return err
	}

	head := forgerepo.RevisionSource(revs[0], commit)
	base := forgerepo.EmptySource()
	if parent, err := forgerepo.ResolveRev(repoDir, commit+"^1"); err == nil {
		base = forgerepo.RevisionSource(revs[0]+"^", parent)
	}

	files, err := forgerepo.ChangedPaths(repoDir, base, head, paths)
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
func showFile(repoDir string, reg *handler.Registry, path string, base, head forgerepo.Source) error {
	fc, err := forgerepo.CompareFile(repoDir, reg, path, base, head)
	if err != nil {
		return err
	}
	switch {
	case !fc.BaseFound && !fc.HeadFound:
		fmt.Printf("  %-46s not in %s or %s\n", path, base, head)
	case !fc.Semantic:
		fmt.Printf("  %-46s %s  %s\n", path, forgerepo.TextChangeSummary(repoDir, base, head, path), handlerLabel(path, reg))
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
