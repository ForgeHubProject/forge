package fhr

import (
	"testing"

	"github.com/forgehubproject/forge/internal/handler"
)

// A handler binary speaks the subprocess protocol and nothing else: match, diff,
// merge, and the optional info. handler.ConflictApplier has no call there, so
// nothing here may claim it. A method that shelled out to a subcommand the
// protocol does not define would be forge inventing one on every handler
// author's behalf, and every handler that exists would answer it the same way —
// unknown subcommand, exit 1 — after the caller had already been told the
// capability was available.
//
// Callers branch on exactly this assertion: forge mergetool offers the
// interactive picker only to a handler that can apply choices, and takes the
// manual route otherwise. A handler binary that started passing it would put
// every user of one through a picker that cannot finish.
func TestASubprocessHandlerClaimsNoCallTheProtocolDoesNotHave(t *testing.T) {
	var h any = &SubprocessHandler{}

	if _, ok := h.(handler.ConflictApplier); ok {
		t.Fatal("a handler binary cannot apply conflict choices: the subprocess protocol has no call for it")
	}
	if _, ok := h.(handler.ForgeHandler); !ok {
		t.Fatal("a handler binary must still be a ForgeHandler")
	}
	if _, ok := h.(handler.Namer); !ok {
		t.Fatal("a handler binary names its format")
	}
}
