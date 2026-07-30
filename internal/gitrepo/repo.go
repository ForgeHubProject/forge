// Package gitrepo provides read access to the working tree's state.
// Currently backed by go-git (pure Go). Intended to migrate to git2go once
// git2go publishes a release compatible with libgit2 1.7.x.
package gitrepo

import (
	"fmt"

	gogit "github.com/go-git/go-git/v5"
)

// Repo wraps a git repository.
type Repo struct {
	r *gogit.Repository
}

// Open finds and opens the git repository containing path.
func Open(path string) (*Repo, error) {
	r, err := gogit.PlainOpenWithOptions(path, &gogit.PlainOpenOptions{DetectDotGit: true})
	if err != nil {
		return nil, fmt.Errorf("not a git repository: %w", err)
	}
	return &Repo{r: r}, nil
}

// ChangedFiles returns the list of files that differ between HEAD and the working tree.
func (r *Repo) ChangedFiles() ([]string, error) {
	wt, err := r.r.Worktree()
	if err != nil {
		return nil, err
	}

	status, err := wt.Status()
	if err != nil {
		return nil, err
	}

	var paths []string
	for path, s := range status {
		if s.Worktree != gogit.Unmodified || s.Staging != gogit.Unmodified {
			paths = append(paths, path)
		}
	}
	return paths, nil
}
