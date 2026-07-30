package forgerepo

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/forgehubproject/forge/internal/fhr"
	"github.com/forgehubproject/forge/internal/handler"
	"github.com/forgehubproject/forge/internal/handler/text"
)

// Registry builds the handler registry for one repository: every installed
// handler whose formats the repo has opted into, with the text catch-all last.
// The root is passed in rather than resolved here, so a caller that must not
// change directory can serve a repository other than the process's own.
func Registry(repoDir string) *handler.Registry {
	reg := handler.NewRegistry()

	forgeFormats := LoadForgeFormats(repoDir)

	for _, meta := range fhr.LoadInstalledHandlers() {
		binary := fhr.InstalledHandlerBinary(meta.ID)
		if binary == "" {
			fmt.Fprintf(os.Stderr, "forge: warning: handler %q metadata found but binary missing\n", meta.ID)
			continue
		}
		if len(forgeFormats) > 0 {
			wanted := false
			for _, ext := range meta.Formats {
				if forgeFormats[strings.ToLower(ext)] {
					wanted = true
					break
				}
			}
			if !wanted {
				continue
			}
		}
		reg.Register(fhr.NewSubprocessHandler(binary, meta))
	}

	reg.Register(text.New())
	return reg
}

// FindRoot returns the top of the repository the process is in, or "." where
// git says there is none — the reading every command has always taken, so that
// one that cares reports the absence itself.
func FindRoot() string {
	out, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "."
	}
	return strings.TrimSpace(string(out))
}

// Per-repo forge files live in .forge/ ("formats" and the "handlers" lockfile).
// The legacy root-level names (.forge-formats, .forge-handlers) are still read
// when .forge/ has no copy; any write migrates the legacy file first.
const forgeRepoDir = ".forge"

// legacyFileWarned holds the legacy names already noted on stderr, so the note
// is printed once per process. It is a sync.Map and not a plain one because
// these files are read on the path of every command and every request: a server
// serving one repository to a client answers each call on its own goroutine, and
// a plain map written from two of them at once aborts the process.
var legacyFileWarned sync.Map

// perRepoFilePath resolves a per-repo forge file for reading: the .forge/
// location wins, otherwise a legacy root-level file is used if present.
func perRepoFilePath(repoDir, name, legacyName string) string {
	current := filepath.Join(repoDir, forgeRepoDir, name)
	if _, err := os.Stat(current); err == nil {
		return current
	}
	legacy := filepath.Join(repoDir, legacyName)
	if _, err := os.Stat(legacy); err == nil {
		if _, warned := legacyFileWarned.LoadOrStore(legacyName, true); !warned {
			fmt.Fprintf(os.Stderr, "forge: note: %s now lives at %s/%s; it will be moved automatically on the next forge write\n", legacyName, forgeRepoDir, name)
		}
		return legacy
	}
	return current
}

// migratePerRepoFile prepares a per-repo forge file for writing: ensures
// .forge/ exists and moves a legacy root-level file into it if one is present.
func migratePerRepoFile(repoDir, name, legacyName string) (string, error) {
	if err := os.MkdirAll(filepath.Join(repoDir, forgeRepoDir), 0755); err != nil {
		return "", err
	}
	current := filepath.Join(repoDir, forgeRepoDir, name)
	if _, err := os.Stat(current); err == nil {
		return current, nil
	}
	legacy := filepath.Join(repoDir, legacyName)
	if _, err := os.Stat(legacy); err == nil {
		if err := os.Rename(legacy, current); err != nil {
			return "", fmt.Errorf("migrating %s to %s/%s: %w", legacyName, forgeRepoDir, name, err)
		}
		fmt.Fprintf(os.Stderr, "forge: migrated %s → %s/%s — remember to commit this move\n", legacyName, forgeRepoDir, name)
	}
	return current, nil
}

func LoadForgeFormats(repoDir string) map[string]bool {
	data, err := os.ReadFile(perRepoFilePath(repoDir, "formats", ".forge-formats"))
	if err != nil {
		return nil
	}
	exts := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		// A leading '!' marks an ignored format — tracked by git but deliberately
		// given no handler; it is not an active/included extension.
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		if !strings.HasPrefix(line, ".") {
			line = "." + line
		}
		exts[strings.ToLower(line)] = true
	}
	return exts
}

// LoadIgnoredFormats returns the extensions the repo has explicitly ignored
// (lines prefixed with '!' in .forge/formats).
func LoadIgnoredFormats(repoDir string) map[string]bool {
	data, err := os.ReadFile(perRepoFilePath(repoDir, "formats", ".forge-formats"))
	if err != nil {
		return nil
	}
	exts := map[string]bool{}
	for _, line := range strings.Split(string(data), "\n") {
		ext, marker := parseFormatLine(strings.TrimSpace(line))
		if marker == "!" && ext != "" {
			exts[ext] = true
		}
	}
	return exts
}

// parseFormatLine normalizes one .forge/formats line into (ext, marker), where
// marker is "!" for an ignored entry or "" for an included one. Comment and
// blank lines yield ("", ""). The returned ext is lower-cased and dot-prefixed.
func parseFormatLine(trimmed string) (ext, marker string) {
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return "", ""
	}
	s := trimmed
	if strings.HasPrefix(s, "!") {
		marker = "!"
		s = strings.TrimSpace(s[1:])
	}
	if s == "" {
		return "", marker
	}
	if !strings.HasPrefix(s, ".") {
		s = "." + s
	}
	return strings.ToLower(s), marker
}

// setForgeFormat rewrites .forge/formats so ext carries exactly the given marker
// ("" = included, "!" = ignored), replacing any existing entry for ext (so
// add<->ignore flips cleanly). Comments and blank lines are preserved. Returns
// whether the file content changed.
func setForgeFormat(repoDir, ext, marker string) (bool, error) {
	path, err := migratePerRepoFile(repoDir, "formats", ".forge-formats")
	if err != nil {
		return false, err
	}
	existing, _ := os.ReadFile(path)

	lines := strings.Split(string(existing), "\n")
	// Drop the trailing empty element a final newline produces, so re-adding
	// doesn't insert a phantom blank line.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}

	var out []string
	changed := false
	kept := false
	for _, line := range lines {
		e, m := parseFormatLine(strings.TrimSpace(line))
		if e == ext && e != "" {
			if m == marker && !kept {
				kept = true
				out = append(out, marker+ext) // normalize spacing/case
				if strings.TrimSpace(line) != marker+ext {
					changed = true
				}
			} else {
				changed = true // drop a wrong-marker or duplicate entry
			}
			continue
		}
		out = append(out, line)
	}
	if !kept {
		out = append(out, marker+ext)
		changed = true
	}
	if !changed {
		return false, nil
	}
	content := strings.Join(out, "\n")
	if content != "" {
		content += "\n"
	}
	return true, os.WriteFile(path, []byte(content), 0644)
}

// LoadForgeHandlers reads the .forge/handlers lockfile and returns
// handlerID → pinned build (nil = unpinned).
func LoadForgeHandlers(repoDir string) map[string]*string {
	data, err := os.ReadFile(perRepoFilePath(repoDir, "handlers", ".forge-handlers"))
	if err != nil {
		return map[string]*string{}
	}
	var m map[string]*string
	if err := json.Unmarshal(data, &m); err != nil {
		return map[string]*string{}
	}
	return m
}

// SaveForgeHandlers writes the .forge/handlers lockfile.
func SaveForgeHandlers(repoDir string, m map[string]*string) error {
	path, err := migratePerRepoFile(repoDir, "handlers", ".forge-handlers")
	if err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

// AddToForgeFormats marks ext as included, flipping it out of the ignore list
// if it was previously ignored.
func AddToForgeFormats(repoDir, ext string) error {
	_, err := setForgeFormat(repoDir, ext, "")
	return err
}

// IgnoreInForgeFormats marks ext as ignored (tracked by git, no handler),
// flipping it out of the included list if it was previously added.
func IgnoreInForgeFormats(repoDir, ext string) error {
	_, err := setForgeFormat(repoDir, ext, "!")
	return err
}
func RemoveFromForgeFormats(repoDir, ext string) error {
	path, err := migratePerRepoFile(repoDir, "formats", ".forge-formats")
	if err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf(".forge/formats not found")
	}
	var out []string
	found := false
	for _, line := range strings.Split(string(data), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			out = append(out, line)
			continue
		}
		// Match either an included (".ext") or ignored ("!.ext") entry.
		if e, _ := parseFormatLine(trimmed); e == ext {
			found = true
			continue
		}
		out = append(out, line)
	}
	if !found {
		return fmt.Errorf("%s is not in .forge/formats", ext)
	}
	return os.WriteFile(path, []byte(strings.Join(out, "\n")), 0644)
}

func IsBinaryHandler(h handler.ForgeHandler) bool {
	n, ok := h.(handler.Namer)
	return ok && n.Format() != "text"
}
