// Package credential talks to git's own credential-helper protocol
// (`git credential fill/approve/reject`), so forge can reuse whatever
// credential helper is already configured on the machine (osxkeychain,
// wincred, libsecret, cache, store, ...) instead of inventing its own
// credential storage.
package credential

import (
	"bufio"
	"bytes"
	"fmt"
	"net/url"
	"os/exec"
	"strings"
)

// entry is one exchange in the git credential protocol: a set of
// key=value lines terminated by a blank line.
type entry struct {
	Protocol string
	Host     string
	Path     string
	Username string
	Password string
}

func entryFromURL(rawURL string) (entry, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return entry{}, fmt.Errorf("parsing URL: %w", err)
	}
	return entry{Protocol: u.Scheme, Host: u.Host, Path: strings.TrimPrefix(u.Path, "/")}, nil
}

func (e entry) encode() []byte {
	var b bytes.Buffer
	writeField := func(k, v string) {
		if v != "" {
			fmt.Fprintf(&b, "%s=%s\n", k, v)
		}
	}
	writeField("protocol", e.Protocol)
	writeField("host", e.Host)
	writeField("path", e.Path)
	writeField("username", e.Username)
	writeField("password", e.Password)
	b.WriteString("\n")
	return b.Bytes()
}

func parseEntry(data []byte, base entry) entry {
	result := base
	scanner := bufio.NewScanner(bytes.NewReader(data))
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		k, v, found := strings.Cut(line, "=")
		if !found {
			continue
		}
		switch k {
		case "username":
			result.Username = v
		case "password":
			result.Password = v
		case "protocol":
			result.Protocol = v
		case "host":
			result.Host = v
		case "path":
			result.Path = v
		}
	}
	return result
}

func run(action string, in entry) (entry, error) {
	cmd := exec.Command("git", "credential", action)
	cmd.Stdin = bytes.NewReader(in.encode())
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return entry{}, fmt.Errorf("git credential %s: %w", action, err)
	}
	return parseEntry(out.Bytes(), in), nil
}

// Fill asks git's configured credential helper(s) for a username/password
// for rawURL. ok is false if git isn't on PATH, no helper is configured, or
// nothing was found for this URL — callers should fall back to their own
// auth flow rather than treating this as a hard error.
func Fill(rawURL string) (username, password string, ok bool) {
	base, err := entryFromURL(rawURL)
	if err != nil {
		return "", "", false
	}
	result, err := run("fill", base)
	if err != nil || result.Password == "" {
		return "", "", false
	}
	return result.Username, result.Password, true
}

// Approve tells git's credential helper(s) to store this credential for
// future use against rawURL.
func Approve(rawURL, username, password string) error {
	base, err := entryFromURL(rawURL)
	if err != nil {
		return err
	}
	base.Username = username
	base.Password = password
	_, err = run("approve", base)
	return err
}

// Reject tells git's credential helper(s) to discard a stored credential
// for rawURL, e.g. after the server reports it as invalid.
func Reject(rawURL, username string) error {
	base, err := entryFromURL(rawURL)
	if err != nil {
		return err
	}
	base.Username = username
	_, err = run("reject", base)
	return err
}
