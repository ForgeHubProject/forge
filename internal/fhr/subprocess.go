package fhr

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/forgehubproject/forge/internal/handler"
)

// SubprocessHandler wraps an FHR handler binary as a ForgeHandler.
// Match is fast (extension check from metadata); Diff/Merge spawn the subprocess.
type SubprocessHandler struct {
	binaryPath string
	id         string
	formats    []string
}

// NewSubprocessHandler builds a SubprocessHandler from a binary path and
// pre-loaded metadata (avoids an info subprocess call on every registry build).
func NewSubprocessHandler(binaryPath string, meta InstalledMeta) *SubprocessHandler {
	return &SubprocessHandler{
		binaryPath: binaryPath,
		id:         meta.ID,
		formats:    meta.Formats,
	}
}

// Format implements handler.Namer.
func (h *SubprocessHandler) Format() string { return h.id }

// Match implements handler.ForgeHandler.
// Uses the cached formats list — no subprocess spawned on status/diff discovery.
func (h *SubprocessHandler) Match(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	for _, f := range h.formats {
		if f == ext {
			return true
		}
	}
	return false
}

// Diff implements handler.ForgeHandler.
func (h *SubprocessHandler) Diff(base, head handler.Blob) (handler.StructuredDiff, error) {
	inp, _ := json.Marshal(struct {
		Base string `json:"base"`
		Head string `json:"head"`
	}{
		Base: base64.StdEncoding.EncodeToString(base),
		Head: base64.StdEncoding.EncodeToString(head),
	})
	out, err := runSubprocess(h.binaryPath, "diff", inp)
	if err != nil {
		return handler.StructuredDiff{}, err
	}
	var diff handler.StructuredDiff
	if err := json.Unmarshal(out, &diff); err != nil {
		return handler.StructuredDiff{}, fmt.Errorf("parsing diff output from %s: %w", h.id, err)
	}
	return diff, nil
}

// Merge implements handler.ForgeHandler.
func (h *SubprocessHandler) Merge(base, ours, theirs handler.Blob) (handler.Blob, *handler.ConflictInfo, error) {
	inp, _ := json.Marshal(struct {
		Base   string `json:"base"`
		Ours   string `json:"ours"`
		Theirs string `json:"theirs"`
	}{
		Base:   base64.StdEncoding.EncodeToString(base),
		Ours:   base64.StdEncoding.EncodeToString(ours),
		Theirs: base64.StdEncoding.EncodeToString(theirs),
	})
	out, err := runSubprocess(h.binaryPath, "merge", inp)
	if err != nil {
		return nil, nil, err
	}
	var result struct {
		Blob      string                     `json:"blob"`
		Conflicts []handler.SemanticConflict `json:"conflicts,omitempty"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, nil, fmt.Errorf("parsing merge output from %s: %w", h.id, err)
	}
	merged, err := base64.StdEncoding.DecodeString(result.Blob)
	if err != nil {
		return nil, nil, fmt.Errorf("decoding merged blob from %s: %w", h.id, err)
	}
	var ci *handler.ConflictInfo
	if len(result.Conflicts) > 0 {
		ci = &handler.ConflictInfo{Conflicts: result.Conflicts}
	}
	return merged, ci, nil
}

func runSubprocess(binary, subcommand string, stdin []byte) ([]byte, error) {
	cmd := exec.Command(binary, subcommand)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w (stderr: %s)",
			filepath.Base(binary), subcommand, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return bytes.TrimSpace(stdout.Bytes()), nil
}
