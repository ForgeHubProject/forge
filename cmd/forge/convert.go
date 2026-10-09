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
	_ = cmd.MarkFlagRequired("to")
	return cmd
}

func runConvert(cmd *cobra.Command, args []string) error {
	cmd.SilenceUsage = true // the arguments were fine; a failure now is not a usage error
	to, _ := cmd.Flags().GetString("to")
	out, _ := cmd.Flags().GetString("output")
	force, _ := cmd.Flags().GetBool("force")
	return convertFile(forgerepo.AllFormatsRegistry(context.Background()), args[0], to, out, force, cmd.OutOrStdout())
}

// convertFile reads src, converts it to the format named by to, and writes the
// result to out ("" means beside the input, "-" means stdout).
func convertFile(reg *handler.Registry, src, to, out string, force bool, stdout io.Writer) error {
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

	blob, err := os.ReadFile(src)
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
