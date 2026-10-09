package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/forgehubproject/forge/internal/forgerepo"
	"github.com/forgehubproject/forge/internal/handler"
	"github.com/spf13/cobra"
)

// ── forge convert ───────────────────────────────────────────────────────────────────────────────────

func convertCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "convert <file> --to <format> [-o <output>]",
		Short: "Convert a 3D file to another format",
		Long: `Converts a 3D file between formats by pivoting through glTF: the source
format's handler imports it, the target format's handler exports it. Any pair of
installed 3D formats works, because each needs one importer and one exporter,
not a converter per pair.

  forge convert bracket.obj --to glb
  forge convert scan.ply --to .stl -o parts/scan.stl
  forge convert bracket.glb --to 3mf -o -      (write to stdout)
  forge convert bracket.obj --to stl --rev v1.0   (the file as of a revision)

Handlers are the ones "forge formats install" put on this machine; a repository's
.forge-formats list does not limit them. Conversion is lossy exactly where the
target format is: OBJ has no transforms or PBR materials, STL no names or
colours, and so on. Coordinates are not rescaled: STL and 3MF are unitless /
millimetres, glTF metres, and the numbers pass through unchanged.`,
		Args: cobra.ExactArgs(1),
		RunE: runConvert,
	}
	cmd.Flags().String("to", "", "Target format, e.g. glb, .obj (required)")
	cmd.Flags().StringP("output", "o", "", `Output file (default: the input with the new extension; "-" for stdout)`)
	cmd.Flags().BoolP("force", "f", false, "Overwrite the output file if it exists")
	cmd.Flags().String("rev", "", "Convert the file as of this revision (branch, tag or commit) instead of the working tree")
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func runConvert(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true // the arguments were fine; a failure now is not a usage error
	to, _ := cmd.Flags().GetString("to")
	out, _ := cmd.Flags().GetString("output")
	force, _ := cmd.Flags().GetBool("force")
	rev, _ := cmd.Flags().GetString("rev")
	reg := forgerepo.AllFormatsRegistry(context.Background())
	if rev == "" {
		return convertFile(reg, args[0], to, out, force, cmd.OutOrStdout(), func() ([]byte, error) { return os.ReadFile(args[0]) })
	}
	return convertFile(reg, args[0], to, defaultRevOut(args[0], to, out), force, cmd.OutOrStdout(), func() ([]byte, error) {
		return readAtRevision(context.Background(), rev, args[0])
	})
}

// defaultRevOut is where a converted historical file goes when -o is not given:
// its base name with the new extension, in the current directory. The path may
// not exist in the working tree any more, so "beside the input" is not an option.
func defaultRevOut(src, to, out string) string {
	if out != "" {
		return out
	}
	base := filepath.Base(src)
	return strings.TrimSuffix(base, filepath.Ext(base)) + handler.NormalizeExt(to)
}

// readAtRevision is the content of path (relative to the current directory) at
// rev. The revision is resolved to a commit first, so a ref that moves while the
// command runs cannot change which bytes are read, and a name that git would read
// as an option is refused rather than passed on.
func readAtRevision(ctx context.Context, rev, path string) ([]byte, error) {
	if strings.HasPrefix(rev, "-") {
		return nil, fmt.Errorf("%q is not a revision", rev)
	}
	dir := "."
	commit, err := forgerepo.GitOutput(ctx, dir, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	if err != nil {
		return nil, fmt.Errorf("unknown revision %q", rev)
	}
	blob, err := forgerepo.GitOutput(ctx, dir, "show", strings.TrimSpace(string(commit))+":./"+filepath.ToSlash(path))
	if err != nil {
		return nil, fmt.Errorf("%s does not exist at %s: %w", path, rev, err)
	}
	return blob, nil
}

// convertFile reads src, converts it to the format named by to, and writes the
// result to out ("" means beside the input, "-" means stdout).
func convertFile(reg *handler.Registry, src, to, out string, force bool, stdout io.Writer, read func() ([]byte, error)) error {
	to = handler.NormalizeExt(to)
	if to == "" {
		return fmt.Errorf("--to is required")
	}
	if out == "" {
		out = strings.TrimSuffix(src, filepath.Ext(src)) + to
	}
	if out != "-" && sameFile(src, out) {
		return fmt.Errorf("refusing to overwrite the input %s: pass -o to name another file", src)
	}
	if out != "-" && !force {
		if _, err := os.Stat(out); err == nil {
			return fmt.Errorf("%s already exists (use --force to overwrite)", out)
		}
	}

	blob, err := read()
	if err != nil {
		return err
	}
	result, err := handler.Convert(reg, src, to, blob)
	if err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	if out == "-" {
		_, err = stdout.Write(result)
		return err
	}
	if err := os.WriteFile(out, result, 0o644); err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "wrote %s (%d bytes)\n", out, len(result))
	return nil
}

func sameFile(a, b string) bool {
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}
