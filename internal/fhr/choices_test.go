package fhr

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// fakeHandler writes a handler binary as a shell script: `info` answers the
// given JSON, `apply-choices` records its stdin and answers "applied", and every
// call is logged — so a test can see what forge asked and what it did not.
func fakeHandler(t *testing.T, info string) (binary, logPath, stdinPath string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell-script handler binaries")
	}
	dir := t.TempDir()
	binary = filepath.Join(dir, "forge-handler-fake")
	logPath = filepath.Join(dir, "calls.log")
	stdinPath = filepath.Join(dir, "stdin.json")
	applied := base64.StdEncoding.EncodeToString([]byte("applied"))
	script := `#!/bin/sh
echo "$1" >> '` + logPath + `'
case "$1" in
  info) ` + info + ` ;;
  apply-choices) cat > '` + stdinPath + `'; echo '{"blob":"` + applied + `"}' ;;
  *) echo "unknown subcommand: $1" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(binary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return binary, logPath, stdinPath
}

func TestChoiceApplierIsOfferedWhenTheBinaryDeclaresIt(t *testing.T) {
	bin, logPath, stdinPath := fakeHandler(t, `echo '{"id":"fake","protocol":"1.0","formats":[".fake"],"capabilities":{"semanticMerge":true,"applyChoices":true}}'`)
	h := NewSubprocessHandler(context.Background(), bin, InstalledMeta{ID: "fake", Formats: []string{".fake"}})

	applier, ok := h.ChoiceApplier()
	if !ok {
		t.Fatal("a binary that declares applyChoices must be offered as an applier")
	}
	out, err := applier.ApplyChoices([]byte("merged"), []byte("theirs"), []string{"nodes/A", "mtllib"})
	if err != nil || string(out) != "applied" {
		t.Fatalf("ApplyChoices = %q, %v", out, err)
	}

	var sent struct {
		Merged, Theirs string
		Take           []string
	}
	raw, _ := os.ReadFile(stdinPath)
	if err := json.Unmarshal(raw, &sent); err != nil {
		t.Fatalf("stdin was not the protocol's JSON: %q", raw)
	}
	if m, _ := base64.StdEncoding.DecodeString(sent.Merged); string(m) != "merged" {
		t.Errorf("merged = %q", m)
	}
	if th, _ := base64.StdEncoding.DecodeString(sent.Theirs); string(th) != "theirs" {
		t.Errorf("theirs = %q", th)
	}
	if strings.Join(sent.Take, ",") != "nodes/A,mtllib" {
		t.Errorf("take = %v", sent.Take)
	}

	// info is asked once per handler, however often the picker asks.
	h.ChoiceApplier()
	if got := strings.Join(calls(t, logPath), " "); got != "info apply-choices" {
		t.Errorf("calls = %q, want info once then apply-choices", got)
	}
}

// A binary that does not declare it — every handler that predates the call —
// is never offered as an applier, and apply-choices is never called on it.
func TestChoiceApplierIsNotOfferedWithoutTheDeclaration(t *testing.T) {
	for name, info := range map[string]string{
		"no capabilities":       `echo '{"id":"fake","protocol":"1.0","formats":[".fake"]}'`,
		"declared false":        `echo '{"id":"fake","protocol":"1.0","formats":[".fake"],"capabilities":{"applyChoices":false}}'`,
		"no info call at all":   `echo "unknown subcommand: info" >&2; exit 1`,
		"info that is not JSON": `echo 'not json'`,
	} {
		t.Run(name, func(t *testing.T) {
			bin, logPath, _ := fakeHandler(t, info)
			h := NewSubprocessHandler(context.Background(), bin, InstalledMeta{ID: "fake", Formats: []string{".fake"}})
			if _, ok := h.ChoiceApplier(); ok {
				t.Fatal("must not be offered as an applier")
			}
			for _, c := range calls(t, logPath) {
				if c == "apply-choices" {
					t.Fatal("apply-choices was called on a binary that never declared it")
				}
			}
		})
	}
}
