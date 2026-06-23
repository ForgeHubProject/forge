package fhr

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Source is an entry in ~/.forge/sources.list.
type Source struct {
	Name string
	URL  string
}

func forgeDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolving home dir: %w", err)
	}
	return filepath.Join(home, ".forge"), nil
}

func sourcesPath() (string, error) {
	d, err := forgeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "sources.list"), nil
}

// LoadSources reads ~/.forge/sources.list.
// Returns nil (not an error) if the file does not exist.
func LoadSources() ([]Source, error) {
	path, err := sourcesPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var sources []Source
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) < 2 {
			continue
		}
		sources = append(sources, Source{Name: parts[0], URL: parts[1]})
	}
	return sources, sc.Err()
}

// AddSource appends a source entry. Returns an error if the name or URL is already registered.
func AddSource(name, rawURL string) error {
	path, err := sourcesPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	sources, err := LoadSources()
	if err != nil {
		return err
	}
	for _, s := range sources {
		if s.Name == name {
			return fmt.Errorf("source %q already registered (url: %s)", name, s.URL)
		}
		if s.URL == rawURL {
			return fmt.Errorf("url %q already registered as source %q", rawURL, s.Name)
		}
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "%s\t%s\n", name, rawURL)
	return err
}
