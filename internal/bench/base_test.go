package bench

import (
	"math/rand"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
	formatcfg "github.com/go-git/go-git/v6/plumbing/format/config"
	"github.com/go-git/go-git/v6/plumbing/object"
)

// newBaseRepo makes an on-disk repo with a nested tree, standing in for a
// large repo whose tip the target already has.
func newBaseRepo(t *testing.T) (string, plumbing.Hash) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInit(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"README", "src/a.txt", "src/deep/b.txt"} {
		f, err := wt.Filesystem().Create(name)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write([]byte(name))
		_ = f.Close()
		if _, err := wt.Add(name); err != nil {
			t.Fatal(err)
		}
	}
	sig := &object.Signature{Name: "base", Email: authorEmail, When: time.Unix(1, 0)}
	h, err := wt.Commit("base", &git.CommitOptions{Author: sig, Signer: noopSigner{}})
	if err != nil {
		t.Fatal(err)
	}
	return dir, h
}

func newBaseAgent(t *testing.T, base *baseTip) *agent {
	t.Helper()
	cfg := &CommitConfig{FilesMin: 1, FilesMax: 3, FileSize: 16}
	a, err := newAgent(0, "o/r", "http://127.0.0.1:1", "refs/heads/fm-c1-a0", formatcfg.SHA1, cfg, nil, nil, nil, nil, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// Agents hold only the base commit and root tree. Commits on top must keep
// every base entry and push without the base's lower trees being local.
func TestCommitOnBasePushesOnlyNewObjects(t *testing.T) {
	baseDirPath, baseHash := newBaseRepo(t)
	base, err := loadBaseTip(baseDirPath, "", formatcfg.SHA1)
	if err != nil {
		t.Fatal(err)
	}
	if base.hash != baseHash {
		t.Fatalf("base tip = %s, want HEAD %s", base.hash, baseHash)
	}

	// The target: a bare clone that already has the full base.
	remoteDir := filepath.Join(t.TempDir(), "remote.git")
	if _, err := git.PlainClone(remoteDir, &git.CloneOptions{URL: baseDirPath, Bare: true}); err != nil {
		t.Fatal(err)
	}

	a := newBaseAgent(t, base)
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // test content
	for iter := range 3 {
		if err := a.commitOnBase(rng, iter); err != nil {
			t.Fatalf("commit %d: %v", iter, err)
		}
		if err := a.repo.Push(&git.PushOptions{
			RemoteURL: remoteDir,
			RefSpecs:  []config.RefSpec{config.RefSpec("+refs/heads/master:" + a.ref)},
			Force:     true,
		}); err != nil {
			t.Fatalf("push %d: %v", iter, err)
		}
	}

	remote, err := git.PlainOpen(remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := remote.Reference(plumbing.ReferenceName(a.ref), true)
	if err != nil {
		t.Fatal(err)
	}
	tip, err := remote.CommitObject(ref.Hash())
	if err != nil {
		t.Fatal(err)
	}
	// Walking the remote tip's full tree proves the pushed packs were complete.
	files, err := tip.Files()
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	if err := files.ForEach(func(f *object.File) error { got[f.Name] = true; return nil }); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"README", "src/a.txt", "src/deep/b.txt", baseDir + "/file-0.txt"} {
		if !got[want] {
			t.Errorf("remote tip is missing %s; got %v", want, got)
		}
	}
	// Three commits on one line, rooted at the base tip.
	c := tip
	for range 3 {
		if c.NumParents() != 1 {
			t.Fatalf("commit %s has %d parents, want 1", c.Hash, c.NumParents())
		}
		if c, err = c.Parent(0); err != nil {
			t.Fatal(err)
		}
	}
	if c.Hash != baseHash {
		t.Fatalf("history roots at %s, want base %s", c.Hash, baseHash)
	}
}

func TestLoadBaseTipRejectsFormatMismatch(t *testing.T) {
	dir, _ := newBaseRepo(t)
	if _, err := loadBaseTip(dir, "", formatcfg.SHA256); err == nil {
		t.Fatal("a sha1 base must not load for a sha256 target")
	}
}

func TestWorkloadBaseRepoNeedsPushStrategy(t *testing.T) {
	w := Workload{Strategy: "session", SessionCommits: 1, Concurrency: []int{1}, Duration: time.Second,
		Commit: CommitConfig{FilesMin: 1, FilesMax: 1, FileSize: 1}, BaseRepo: "/x"}
	if err := w.Validate(); err == nil {
		t.Fatal("-base-repo with -strategy session must be rejected")
	}
	w.Strategy = "branch"
	if err := w.Validate(); err != nil {
		t.Fatalf("-base-repo with -strategy branch: %v", err)
	}
}
