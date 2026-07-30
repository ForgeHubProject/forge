package forgerepo

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/forgehubproject/forge/internal/handler"
)

// echoHandler is a format handler that reports the blobs it was handed, so a test
// can assert which bytes a comparison really read.
type echoHandler struct{}

func (echoHandler) Match(path string) bool { return strings.HasSuffix(path, ".echo") }
func (echoHandler) Format() string         { return "echo-stub" }

func (echoHandler) Diff(base, head handler.Blob) (handler.StructuredDiff, error) {
	return handler.StructuredDiff{
		Version: "1.0",
		Format:  "echo-stub",
		Changes: []handler.DiffChange{{Path: "content", Kind: handler.Modified, Before: string(base), After: string(head)}},
	}, nil
}

func (echoHandler) Merge(_, _, _ handler.Blob) (handler.Blob, *handler.ConflictInfo, error) {
	return nil, nil, handler.ErrNotSupported
}

// A link in the working tree is content in its own right: git records one as a
// blob holding the path it names, which is what the other side of any comparison
// against history holds. Reading through it compares a file the repository does
// not contain against the link that names it, and lets a link the repository
// itself carries decide what gets read.
func TestAWorktreeLinkIsComparedAsTheNameItHolds(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("links are not the same thing here")
	}
	repo := t.TempDir()
	outside := filepath.Join(t.TempDir(), "outside.echo")
	writeFileT(t, outside, "SECRET")
	if err := os.Symlink(outside, filepath.Join(repo, "link.echo")); err != nil {
		t.Fatal(err)
	}

	reg := handler.NewRegistry()
	reg.Register(echoHandler{})
	fc, err := CompareFile(context.Background(), repo, reg, "link.echo", EmptySource(), WorktreeSource())
	if err != nil {
		t.Fatal(err)
	}
	if !fc.HeadFound || !fc.Semantic {
		t.Fatalf("a link is an entry the working tree holds: %+v", fc)
	}
	if string(fc.Head) != outside {
		t.Fatalf("expected the path the link names, got %q", fc.Head)
	}
	if strings.Contains(string(fc.Head), "SECRET") {
		t.Fatal("the comparison read a file outside the repository")
	}
	if got := SourceEntry(context.Background(), repo, WorktreeSource(), "link.echo"); got != "blob" {
		t.Fatalf("git records a link as a blob, this reports %q", got)
	}
}
