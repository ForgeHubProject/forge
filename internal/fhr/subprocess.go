package fhr

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/forgehubproject/forge/internal/handler"
)

// SubprocessHandler wraps an FHR handler binary as a ForgeHandler.
// Match is fast (extension check from metadata); Diff/Merge spawn the subprocess.
//
// Those are the calls it always makes. A call the protocol makes optional —
// `apply-choices` — is made only when the binary declares it in `info`: a
// method that spawned a subcommand the handler never promised would be forge
// inventing a call on every handler author's behalf, answered everywhere with
// "unknown subcommand". So SubprocessHandler does not implement
// handler.ConflictApplier itself; ChoiceApplier asks the binary first.
//
// ctx is held rather than taken per call because ForgeHandler's methods take
// none: it bounds the subprocesses this handler spawns, so a caller that builds
// one registry per cancellable unit of work — a request rather than a process —
// gets a handler that dies with that work instead of outliving it.
type SubprocessHandler struct {
	binaryPath string
	id         string
	formats    []string
	ctx        context.Context

	// The binary's own info, asked at most once and only when an optional call
	// needs it: nil when it answered none.
	infoOnce sync.Once
	info     *Info
}

// NewSubprocessHandler builds a SubprocessHandler from a binary path and
// pre-loaded metadata (avoids an info subprocess call on every registry build).
func NewSubprocessHandler(ctx context.Context, binaryPath string, meta InstalledMeta) *SubprocessHandler {
	if ctx == nil {
		ctx = context.Background()
	}
	return &SubprocessHandler{
		binaryPath: binaryPath,
		id:         meta.ID,
		formats:    meta.Formats,
		ctx:        ctx,
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
	out, err := runSubprocess(h.ctx, h.binaryPath, "diff", inp)
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
	out, err := runSubprocess(h.ctx, h.binaryPath, "merge", inp)
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

// declared is the binary's info, asked once; nil when it answers none.
func (h *SubprocessHandler) declared() *Info {
	h.infoOnce.Do(func() {
		if info, err := HandlerInfo(h.ctx, h.binaryPath); err == nil {
			h.info = info
		}
	})
	return h.info
}

// ChoiceApplier implements handler.ChoiceApplierProvider: a ConflictApplier
// when the binary declares `capabilities.applyChoices` in its info. A binary
// that answers nothing, or declares nothing, cannot apply choices — which is
// the honest answer for every handler that predates the call.
func (h *SubprocessHandler) ChoiceApplier() (handler.ConflictApplier, bool) {
	info := h.declared()
	if info == nil || info.Capabilities == nil || info.Capabilities.ApplyChoices == nil || !*info.Capabilities.ApplyChoices {
		return nil, false
	}
	return subprocessApplier{h}, true
}

// subprocessApplier makes the protocol's `apply-choices` call.
type subprocessApplier struct{ h *SubprocessHandler }

func (a subprocessApplier) ApplyChoices(merged, theirs handler.Blob, takePaths []string) (handler.Blob, error) {
	if takePaths == nil {
		takePaths = []string{}
	}
	inp, _ := json.Marshal(struct {
		Merged string   `json:"merged"`
		Theirs string   `json:"theirs"`
		Take   []string `json:"take"`
	}{
		Merged: base64.StdEncoding.EncodeToString(merged),
		Theirs: base64.StdEncoding.EncodeToString(theirs),
		Take:   takePaths,
	})
	out, err := runSubprocess(a.h.ctx, a.h.binaryPath, "apply-choices", inp)
	if err != nil {
		return nil, err
	}
	var result struct {
		Blob string `json:"blob"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parsing apply-choices output from %s: %w", a.h.id, err)
	}
	blob, err := base64.StdEncoding.DecodeString(result.Blob)
	if err != nil {
		return nil, fmt.Errorf("decoding apply-choices blob from %s: %w", a.h.id, err)
	}
	return blob, nil
}

// Previewer implements handler.PreviewerProvider: a Previewer when the binary
// declares a `preview` media type in its info, and nothing otherwise — the
// call is never made on a binary that did not promise it.
func (h *SubprocessHandler) Previewer() (handler.Previewer, bool) {
	info := h.declared()
	if info == nil || info.Preview == "" {
		return nil, false
	}
	return subprocessPreviewer{h: h, mediaType: info.Preview}, true
}

// subprocessPreviewer makes the protocol's `preview` call.
type subprocessPreviewer struct {
	h         *SubprocessHandler
	mediaType string
}

func (p subprocessPreviewer) PreviewMediaType() string { return p.mediaType }

func (p subprocessPreviewer) Preview(blob handler.Blob) (handler.Blob, error) {
	inp, _ := json.Marshal(struct {
		Blob string `json:"blob"`
	}{base64.StdEncoding.EncodeToString(blob)})
	out, err := runSubprocess(p.h.ctx, p.h.binaryPath, "preview", inp)
	if err != nil {
		return nil, err
	}
	var result struct {
		Blob string `json:"blob"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, fmt.Errorf("parsing preview output from %s: %w", p.h.id, err)
	}
	b, err := base64.StdEncoding.DecodeString(result.Blob)
	if err != nil {
		return nil, fmt.Errorf("decoding preview from %s: %w", p.h.id, err)
	}
	return b, nil
}

// Info is what a handler binary answers for the protocol's "info" call: the id
// it goes by, its own version, the extensions it claims, and the protocol
// revision it speaks. Capabilities is the handler's own declaration — a handler
// that omits it has said nothing about what it supports, which is not the same
// as saying it supports nothing.
type Info struct {
	ID           string            `json:"id"`
	Version      string            `json:"version"`
	Protocol     string            `json:"protocol"`
	Formats      []string          `json:"formats"`
	Capabilities *InfoCapabilities `json:"capabilities,omitempty"`
	// Preview is the media type of the optional `preview` call (FHR SPEC §7),
	// present only for handlers that have one.
	Preview string `json:"preview,omitempty"`
}

// InfoCapabilities is the optional capability block of an info answer. Both
// fields are pointers so an undeclared capability stays distinguishable from
// one declared false.
type InfoCapabilities struct {
	SemanticCompare *bool `json:"semanticCompare,omitempty"`
	SemanticMerge   *bool `json:"semanticMerge,omitempty"`
	// ApplyChoices says the binary answers `apply-choices` (FHR SPEC §7).
	ApplyChoices *bool `json:"applyChoices,omitempty"`
}

// HandlerInfo asks an installed handler binary to describe itself. The call is
// optional in the protocol, so a handler that does not implement it fails here
// and the caller reports the absence rather than inventing an answer.
func HandlerInfo(ctx context.Context, binaryPath string) (*Info, error) {
	out, err := runSubprocess(ctx, binaryPath, "info", nil)
	if err != nil {
		return nil, err
	}
	var info Info
	if err := json.Unmarshal(out, &info); err != nil {
		return nil, fmt.Errorf("parsing info output from %s: %w", filepath.Base(binaryPath), err)
	}
	return &info, nil
}

// subprocessWaitDelay bounds how long a killed handler's pipes are waited on
// once its context is done.
const subprocessWaitDelay = 2 * time.Second

// runSubprocess runs one handler call to completion. The context is the caller's
// bound on it: a handler that never returns is killed with the work that asked
// for it rather than left behind, which is the difference between a command and
// a server that keeps answering.
func runSubprocess(ctx context.Context, binary, subcommand string, stdin []byte) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	cmd := exec.CommandContext(ctx, binary, subcommand)
	cmd.Stdin = bytes.NewReader(stdin)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A killed handler that left a child of its own holding the pipes would
	// otherwise keep this wait — and a caller shutting down behind it — blocked
	// on output nobody is going to read.
	cmd.WaitDelay = subprocessWaitDelay
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w (stderr: %s)",
			filepath.Base(binary), subcommand, err, bytes.TrimSpace(stderr.Bytes()))
	}
	return bytes.TrimSpace(stdout.Bytes()), nil
}
