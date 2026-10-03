package fhr

import (
	"testing"

	"github.com/forgehubproject/forge/internal/handler"
)

// A handler binary speaks the subprocess protocol: match, diff, merge, and the
// optional info — plus optional calls it declares, like apply-choices. So a
// SubprocessHandler must not *be* a handler.ConflictApplier: a method that
// shelled out to a subcommand the binary never promised would be answered
// "unknown subcommand, exit 1" after the caller had been told the capability
// was there. It offers one through ChoiceApplier, only when info declares it.
//
// Callers branch on exactly this: forge mergetool offers the interactive
// picker only to a handler that can apply choices, and takes the manual route
// otherwise. A picker that cannot finish is worse than no picker.
func TestASubprocessHandlerClaimsNoCallTheProtocolDoesNotHave(t *testing.T) {
	var h any = &SubprocessHandler{}

	if _, ok := h.(handler.ConflictApplier); ok {
		t.Fatal("a handler binary must not claim apply-choices before its info declares it")
	}
	if _, ok := h.(handler.ChoiceApplierProvider); !ok {
		t.Fatal("a handler binary must be asked whether it can apply choices")
	}
	if _, ok := h.(handler.ForgeHandler); !ok {
		t.Fatal("a handler binary must still be a ForgeHandler")
	}
	if _, ok := h.(handler.Namer); !ok {
		t.Fatal("a handler binary names its format")
	}
}
