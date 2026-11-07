// Copyright 2026 The flux4swarm Authors
// SPDX-License-Identifier: Apache-2.0

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/storage/memory"

	"github.com/olljanat-ai/flux4swarm/internal/source"
)

// Ref selects what to check out. At most one of Branch, Tag, SemVer, Name
// and Commit should be set; Commit may be combined with Branch. Like Flux,
// the precedence is Commit, Name, SemVer, Tag, Branch.
type Ref struct {
	Branch string
	Tag    string
	// SemVer selects the highest tag that satisfies the constraint.
	SemVer string
	Name   string
	Commit string
}

// Artifact is a checked-out revision. Revision uses the Flux format:
// "<branch>@sha1:<hash>", "<tag>@sha1:<hash>" or "sha1:<hash>".
type Artifact = source.Artifact

// Fetcher keeps one checkout per source in a cache directory.
type Fetcher struct {
	URL  string
	Ref  Ref
	Auth *AuthProvider
	// Dir is the cache directory for this source.
	Dir string
}

func (r Ref) refName() (plumbing.ReferenceName, string) {
	switch {
	case r.Name != "":
		return plumbing.ReferenceName(r.Name), r.Name
	case r.Tag != "":
		return plumbing.NewTagReferenceName(r.Tag), r.Tag
	case r.Branch != "":
		return plumbing.NewBranchReferenceName(r.Branch), r.Branch
	}
	return plumbing.HEAD, ""
}

// RemoteHead returns the commit the ref points at without cloning.
func (f *Fetcher) RemoteHead(ctx context.Context) (string, string, error) {
	if f.Ref.Commit != "" && f.Ref.Branch == "" {
		return f.Ref.Commit, "", nil
	}
	auth, err := f.Auth.Method(ctx)
	if err != nil {
		return "", "", err
	}
	rem := gogit.NewRemote(memory.NewStorage(), &config.RemoteConfig{Name: "origin", URLs: []string{f.URL}})
	refs, err := rem.ListContext(ctx, &gogit.ListOptions{Auth: auth})
	if err != nil {
		return "", "", fmt.Errorf("list %s: %w", redact(f.URL), err)
	}
	byName := map[plumbing.ReferenceName]*plumbing.Reference{}
	for _, r := range refs {
		byName[r.Name()] = r
	}
	ref := f.Ref
	if ref.SemVer != "" && ref.Name == "" {
		var tags []string
		for _, r := range refs {
			if r.Name().IsTag() && !strings.HasSuffix(r.Name().String(), "^{}") {
				tags = append(tags, r.Name().Short())
			}
		}
		tag, err := source.LatestVersion(tags, ref.SemVer, "")
		if err != nil {
			return "", "", fmt.Errorf("%s: %w", redact(f.URL), err)
		}
		ref.Tag = tag
	}
	want, short := ref.refName()
	if want == plumbing.HEAD {
		h := byName[plumbing.HEAD]
		if h == nil {
			return "", "", fmt.Errorf("remote %s has no HEAD", redact(f.URL))
		}
		if h.Type() == plumbing.SymbolicReference {
			target := byName[h.Target()]
			if target == nil {
				return "", "", fmt.Errorf("remote HEAD points at missing %s", h.Target())
			}
			return target.Hash().String(), h.Target().Short(), nil
		}
		return h.Hash().String(), "HEAD", nil
	}
	// Annotated tags: prefer the peeled commit.
	if r := byName[plumbing.ReferenceName(string(want)+"^{}")]; r != nil {
		return r.Hash().String(), short, nil
	}
	r := byName[want]
	if r == nil {
		return "", "", fmt.Errorf("ref %s not found in %s", want, redact(f.URL))
	}
	return r.Hash().String(), short, nil
}

// Fetch makes sure the cache holds the latest revision and returns it. If
// last is not nil and the remote has not moved, last is returned as is.
func (f *Fetcher) Fetch(ctx context.Context, last *Artifact) (*Artifact, error) {
	commit, short, err := f.RemoteHead(ctx)
	if err != nil {
		return nil, err
	}
	if f.Ref.Commit != "" {
		commit = f.Ref.Commit
	}
	if last != nil && last.Commit == commit {
		if _, err := os.Stat(last.Dir); err == nil {
			return last, nil
		}
	}
	auth, err := f.Auth.Method(ctx)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(f.Dir, 0o755); err != nil {
		return nil, err
	}
	tmp, err := os.MkdirTemp(f.Dir, "checkout-")
	if err != nil {
		return nil, err
	}
	opts := &gogit.CloneOptions{URL: f.URL, Auth: auth, Tags: gogit.NoTags}
	// A semver ref was resolved to the tag short by RemoteHead.
	resolved := f.Ref
	if resolved.SemVer != "" && resolved.Name == "" {
		resolved.Tag = short
	}
	ref, _ := resolved.refName()
	if f.Ref.Commit == "" {
		opts.Depth = 1
		opts.SingleBranch = true
		if ref != plumbing.HEAD {
			opts.ReferenceName = ref
		}
	} else if f.Ref.Branch != "" {
		opts.ReferenceName = ref
		opts.SingleBranch = true
	}
	repo, err := gogit.PlainCloneContext(ctx, tmp, false, opts)
	if err != nil {
		os.RemoveAll(tmp)
		return nil, fmt.Errorf("clone %s: %w", redact(f.URL), err)
	}
	head, err := repo.Head()
	if err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	if f.Ref.Commit != "" {
		wt, err := repo.Worktree()
		if err != nil {
			os.RemoveAll(tmp)
			return nil, err
		}
		if err := wt.Checkout(&gogit.CheckoutOptions{Hash: plumbing.NewHash(f.Ref.Commit), Force: true}); err != nil {
			os.RemoveAll(tmp)
			return nil, fmt.Errorf("checkout %s: %w", f.Ref.Commit, err)
		}
		commit = f.Ref.Commit
	} else {
		commit = head.Hash().String()
	}
	// Swap into place: keep exactly one checkout per source.
	final := filepath.Join(f.Dir, "rev-"+commit[:12])
	os.RemoveAll(final)
	if err := os.Rename(tmp, final); err != nil {
		os.RemoveAll(tmp)
		return nil, err
	}
	entries, _ := os.ReadDir(f.Dir)
	for _, e := range entries {
		if p := filepath.Join(f.Dir, e.Name()); p != final {
			os.RemoveAll(p)
		}
	}
	rev := "sha1:" + commit
	if short != "" && f.Ref.Commit == "" {
		rev = short + "@" + rev
	} else if f.Ref.Branch != "" {
		rev = f.Ref.Branch + "@" + rev
	}
	return &Artifact{Dir: final, Revision: rev, Commit: commit, FetchedAt: time.Now()}, nil
}

// PushOptions configures Commit and push.
type PushOptions struct {
	URL     string
	Branch  string
	Auth    *AuthProvider
	Author  string
	Email   string
	Message string
	// Write is called with the worktree root and must write the files.
	// Paths it returns (relative to the root) are deleted first.
	Write func(root string) error
	// Clean lists worktree-relative directories that are removed before
	// Write runs, so that objects deleted from Swarm vanish from Git too.
	Clean []string
	// Describe, when set, writes the commit message from the files the
	// commit changes. An empty result falls back to Message.
	Describe func([]FileChange) string
}

// FileChange is a file a commit adds, changes or deletes. Old is nil for an
// added file and New is nil for a deleted one.
type FileChange struct {
	Path     string
	Old, New []byte
}

// PushResult describes what CommitAndPush did.
type PushResult struct {
	Commit  string
	Changed bool
}

// pushAttempts bounds the retries of CommitAndPush when the remote branch
// moved between the clone and the push.
var pushAttempts = 5

// CommitAndPush clones the branch (or starts it if the repository or branch
// is empty), lets Write change files, commits everything and pushes. When
// the push is rejected because someone else pushed first, it starts over
// from a fresh clone, so Write runs again on top of the new tip.
//
// The clone is shallow (depth 1), so the cost of a sync does not grow with
// the history of the export repository. If the shallow attempt fails for
// any other reason (a server or transport that cannot serve or accept it),
// the sync is retried once with a full clone.
func CommitAndPush(ctx context.Context, workDir string, o PushOptions) (*PushResult, error) {
	for attempt := 1; ; attempt++ {
		res, err := shallowOrFull(ctx, workDir, o)
		if err == nil || !isRejected(err) || attempt >= pushAttempts {
			return res, err
		}
		if err := os.RemoveAll(workDir); err != nil {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(time.Duration(attempt) * 200 * time.Millisecond):
		}
	}
}

// shallowOrFull runs one sync with a shallow clone and falls back to a full
// clone, except when the push lost a race (CommitAndPush retries that).
func shallowOrFull(ctx context.Context, workDir string, o PushOptions) (*PushResult, error) {
	res, err := commitAndPush(ctx, workDir, o, 1)
	if err == nil || ctx.Err() != nil || isRejected(err) {
		return res, err
	}
	if rerr := os.RemoveAll(workDir); rerr != nil {
		return nil, err
	}
	res, ferr := commitAndPush(ctx, workDir, o, 0)
	if ferr != nil && ferr.Error() != err.Error() {
		ferr = fmt.Errorf("%w (shallow attempt: %v)", ferr, err)
	}
	return res, ferr
}

// rejectedError marks a push the remote refused because the branch moved.
type rejectedError struct{ error }

func (e rejectedError) Unwrap() error { return e.error }

func isRejected(err error) bool {
	var r rejectedError
	return errors.As(err, &r)
}

// nonFastForward reports whether a push error means the remote branch has
// commits the local one lacks.
func nonFastForward(err error) bool {
	if errors.Is(err, gogit.ErrNonFastForwardUpdate) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, s := range []string{"non-fast-forward", "fetch first", "rejected"} {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

func commitAndPush(ctx context.Context, workDir string, o PushOptions, depth int) (*PushResult, error) {
	auth, err := o.Auth.Method(ctx)
	if err != nil {
		return nil, err
	}
	if o.Branch == "" {
		o.Branch = "main"
	}
	branch := plumbing.NewBranchReferenceName(o.Branch)
	repo, err := gogit.PlainCloneContext(ctx, workDir, false, &gogit.CloneOptions{
		URL: o.URL, Auth: auth, ReferenceName: branch, SingleBranch: true,
		Depth: depth, Tags: gogit.NoTags,
	})
	switch {
	case err == nil:
	case errors.Is(err, transport.ErrEmptyRemoteRepository) || isMissingRef(err):
		os.RemoveAll(workDir)
		repo, err = gogit.PlainInitWithOptions(workDir, &gogit.PlainInitOptions{
			InitOptions: gogit.InitOptions{DefaultBranch: branch},
		})
		if err != nil {
			return nil, err
		}
		if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{o.URL}}); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("clone %s: %w", redact(o.URL), err)
	}
	for _, c := range o.Clean {
		if err := os.RemoveAll(filepath.Join(workDir, filepath.FromSlash(c))); err != nil {
			return nil, err
		}
	}
	if err := o.Write(workDir); err != nil {
		return nil, err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, err
	}
	if err := wt.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		return nil, err
	}
	st, err := wt.Status()
	if err != nil {
		return nil, err
	}
	if st.IsClean() {
		head, err := repo.Head()
		if err == nil {
			return &PushResult{Commit: head.Hash().String()}, nil
		}
		return &PushResult{}, nil
	}
	if o.Author == "" {
		o.Author = "flux4swarm"
	}
	if o.Email == "" {
		o.Email = "flux4swarm@users.noreply.github.com"
	}
	msg := o.Message
	if o.Describe != nil {
		changes, err := fileChanges(repo, workDir, st)
		if err != nil {
			return nil, err
		}
		if m := o.Describe(changes); m != "" {
			msg = m
		}
	}
	h, err := wt.Commit(msg, &gogit.CommitOptions{
		Author: &object.Signature{Name: o.Author, Email: o.Email, When: time.Now()},
	})
	if err != nil {
		return nil, err
	}
	err = repo.PushContext(ctx, &gogit.PushOptions{
		RemoteName: "origin",
		Auth:       auth,
		RefSpecs:   []config.RefSpec{config.RefSpec(branch + ":" + branch)},
	})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		err = fmt.Errorf("push %s: %w", redact(o.URL), err)
		if nonFastForward(err) {
			err = rejectedError{err}
		}
		return nil, err
	}
	return &PushResult{Commit: h.String(), Changed: true}, nil
}

// fileChanges reads the old (HEAD) and new (worktree) content of every
// file in the status, sorted by path.
func fileChanges(repo *gogit.Repository, root string, st gogit.Status) ([]FileChange, error) {
	var tree *object.Tree
	if head, err := repo.Head(); err == nil {
		c, err := repo.CommitObject(head.Hash())
		if err != nil {
			return nil, err
		}
		if tree, err = c.Tree(); err != nil {
			return nil, err
		}
	}
	var out []FileChange
	for p := range st {
		fc := FileChange{Path: p}
		if tree != nil {
			if f, err := tree.File(p); err == nil {
				s, err := f.Contents()
				if err != nil {
					return nil, err
				}
				fc.Old = []byte(s)
			}
		}
		b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(p)))
		switch {
		case err == nil:
			fc.New = b
		case !errors.Is(err, os.ErrNotExist):
			return nil, err
		}
		if fc.Old == nil && fc.New == nil {
			continue
		}
		out = append(out, fc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

func isMissingRef(err error) bool {
	var nf gogit.NoMatchingRefSpecError
	return errors.As(err, &nf) || strings.Contains(err.Error(), "couldn't find remote ref") ||
		errors.Is(err, plumbing.ErrReferenceNotFound)
}

func redact(u string) string {
	if i := strings.Index(u, "@"); i > 0 && strings.Contains(u[:i], "://") {
		return u[:strings.Index(u, "://")+3] + "***@" + u[i+1:]
	}
	return u
}
