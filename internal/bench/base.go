package bench

import (
	"fmt"
	"io"
	"math/rand"
	"sort"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
	"github.com/go-git/go-git/v6/plumbing/filemode"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/go-git/go-git/v6/plumbing/storer"
	"github.com/go-git/go-git/v6/storage"
)

// baseDir is the top-level directory each agent writes its files into when
// it branches from a base tip. Keeping all load under one directory means
// only the root tree changes above it, so the base's other trees are never
// needed locally.
const baseDir = "forgemark"

// baseTip is an existing commit that agents branch from instead of starting
// an orphan line, so pushes land on a real repo's history and tree. Only the
// commit and its root tree are kept: the target already has everything
// below them, and go-git's push walk stops at a shallow commit.
type baseTip struct {
	hash    plumbing.Hash
	commit  []byte // raw commit object
	tree    []byte // raw root tree object
	entries []object.TreeEntry
}

// loadBaseTip reads ref (default HEAD) from the local repo at path. A clone
// made with `git clone --depth 1 --filter=blob:none --no-checkout` is enough.
// The target must already have this commit (for example, a mirror of the same
// repo), since agents push only the objects they create.
func loadBaseTip(path, ref string, objFmt formatcfg.ObjectFormat) (*baseTip, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return nil, fmt.Errorf("open -base-repo %s: %w", path, err)
	}
	var tip *plumbing.Reference
	if ref == "" {
		tip, err = repo.Head()
	} else {
		tip, err = repo.Reference(normalizeBaseRef(ref), true)
	}
	if err != nil {
		return nil, fmt.Errorf("resolve base ref %q: %w", ref, err)
	}
	if tip.Hash().Size() != objFmt.Size() {
		return nil, fmt.Errorf("-base-repo object format does not match the target (%s)", objFmt)
	}
	commit, err := repo.CommitObject(tip.Hash())
	if err != nil {
		return nil, fmt.Errorf("read base commit %s: %w", tip.Hash(), err)
	}
	tree, err := repo.TreeObject(commit.TreeHash)
	if err != nil {
		return nil, fmt.Errorf("read base tree %s: %w", commit.TreeHash, err)
	}
	b := &baseTip{hash: tip.Hash(), entries: tree.Entries}
	if b.commit, err = rawObject(repo.Storer, plumbing.CommitObject, tip.Hash()); err != nil {
		return nil, err
	}
	if b.tree, err = rawObject(repo.Storer, plumbing.TreeObject, commit.TreeHash); err != nil {
		return nil, err
	}
	return b, nil
}

func rawObject(s storer.EncodedObjectStorer, t plumbing.ObjectType, h plumbing.Hash) ([]byte, error) {
	o, err := s.EncodedObject(t, h)
	if err != nil {
		return nil, fmt.Errorf("read %s %s: %w", t, h, err)
	}
	r, err := o.Reader()
	if err != nil {
		return nil, fmt.Errorf("read %s %s: %w", t, h, err)
	}
	defer r.Close()
	return io.ReadAll(r)
}

// seed copies the base commit and root tree into s, marks the commit shallow
// so history walks stop there, and points master at it.
func (b *baseTip) seed(s storage.Storer) error {
	if _, err := storeObject(s, plumbing.CommitObject, b.commit); err != nil {
		return err
	}
	if _, err := storeObject(s, plumbing.TreeObject, b.tree); err != nil {
		return err
	}
	if err := s.SetShallow([]plumbing.Hash{b.hash}); err != nil {
		return fmt.Errorf("set shallow: %w", err)
	}
	return s.SetReference(plumbing.NewHashReference(plumbing.Master, b.hash))
}

// commitOnBase writes the changed files under baseDir and commits on top of
// master without a worktree, so the base's files are never checked out.
// a.files keeps earlier blobs, so later commits modify the same working set.
func (a *agent) commitOnBase(rng *rand.Rand, iter int) error {
	s := a.repo.Storer
	buf := make([]byte, a.cfg.FileSize)
	for i := range a.fileCount(rng) {
		_, _ = rng.Read(buf) // math/rand never errors; random bytes → distinct blob
		h, err := storeObject(s, plumbing.BlobObject, buf)
		if err != nil {
			return err
		}
		a.files[fmt.Sprintf("file-%d.txt", i)] = h
	}
	dir := make([]object.TreeEntry, 0, len(a.files))
	for name, h := range a.files {
		dir = append(dir, object.TreeEntry{Name: name, Mode: filemode.Regular, Hash: h})
	}
	dirHash, err := storeTree(s, dir)
	if err != nil {
		return err
	}
	root := make([]object.TreeEntry, 0, len(a.base.entries)+1)
	for _, e := range a.base.entries {
		if e.Name != baseDir {
			root = append(root, e)
		}
	}
	root = append(root, object.TreeEntry{Name: baseDir, Mode: filemode.Dir, Hash: dirHash})
	rootHash, err := storeTree(s, root)
	if err != nil {
		return err
	}

	head, err := s.Reference(plumbing.Master)
	if err != nil {
		return fmt.Errorf("read master: %w", err)
	}
	sig := object.Signature{Name: fmt.Sprintf("agent-%d", a.id), Email: authorEmail, When: time.Now()}
	c := &object.Commit{
		Author:       sig,
		Committer:    sig,
		Message:      fmt.Sprintf("agent %d commit %d\n", a.id, iter),
		TreeHash:     rootHash,
		ParentHashes: []plumbing.Hash{head.Hash()},
	}
	o := s.NewEncodedObject()
	if err := c.Encode(o); err != nil {
		return fmt.Errorf("encode commit: %w", err)
	}
	h, err := s.SetEncodedObject(o)
	if err != nil {
		return fmt.Errorf("store commit: %w", err)
	}
	return s.SetReference(plumbing.NewHashReference(plumbing.Master, h))
}

func storeObject(s storer.EncodedObjectStorer, t plumbing.ObjectType, data []byte) (plumbing.Hash, error) {
	o := s.NewEncodedObject()
	o.SetType(t)
	o.SetSize(int64(len(data)))
	w, err := o.Writer()
	if err != nil {
		return plumbing.ZeroHash, err
	}
	if _, err := w.Write(data); err != nil {
		_ = w.Close()
		return plumbing.ZeroHash, err
	}
	if err := w.Close(); err != nil {
		return plumbing.ZeroHash, err
	}
	return s.SetEncodedObject(o)
}

func storeTree(s storer.EncodedObjectStorer, entries []object.TreeEntry) (plumbing.Hash, error) {
	sort.Sort(object.TreeEntrySorter(entries))
	o := s.NewEncodedObject()
	if err := (&object.Tree{Entries: entries}).Encode(o); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("encode tree: %w", err)
	}
	return s.SetEncodedObject(o)
}
