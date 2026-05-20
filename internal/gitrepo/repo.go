// Package gitrepo provides read access to the git object store.
// Currently backed by go-git (pure Go). Intended to migrate to git2go once
// git2go publishes a release compatible with libgit2 1.7.x.
package gitrepo

import (
	"fmt"
	"io"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// Repo wraps a git repository for blob retrieval.
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

// BlobAtHEAD returns the content of relPath at HEAD, or nil if the file is
// untracked (new file not yet committed).
func (r *Repo) BlobAtHEAD(relPath string) ([]byte, error) {
	ref, err := r.r.Head()
	if err == plumbing.ErrReferenceNotFound {
		return nil, nil // empty repo, no commits yet
	}
	if err != nil {
		return nil, err
	}

	commit, err := r.r.CommitObject(ref.Hash())
	if err != nil {
		return nil, err
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, err
	}

	entry, err := tree.FindEntry(relPath)
	if err == object.ErrEntryNotFound {
		return nil, nil // file not in HEAD (new file)
	}
	if err != nil {
		return nil, err
	}

	blob, err := r.r.BlobObject(entry.Hash)
	if err != nil {
		return nil, err
	}

	rc, err := blob.Reader()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	return io.ReadAll(rc)
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
