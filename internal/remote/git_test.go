package remote

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	vaultJSONv1 = `{"version":1,"id":"11111111-1111-1111-1111-111111111111","kdf":{"algo":"argon2id"},"wrapped_key":"x"}`
	vaultJSONv2 = `{"version":1,"id":"22222222-2222-2222-2222-222222222222","kdf":{"algo":"argon2id"},"wrapped_key":"y"}`
)

func TestGitPrepareFreshEmptyRemote(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	bare := newBare(t)
	r, dir := newGitVault(t, bare, "m1")

	if err := r.Prepare(ctx, testLog(t)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if !pathExists(dir, ".git") {
		t.Fatal(".git missing after Prepare")
	}
	if got := mustRead(t, dir, ".gitattributes"); got != gitAttributesContent {
		t.Errorf(".gitattributes = %q, want %q", got, gitAttributesContent)
	}
	if got := mustRead(t, dir, ".gitignore"); got != gitIgnoreContent {
		t.Errorf(".gitignore = %q, want %q", got, gitIgnoreContent)
	}
	for _, want := range []string{".psv-tmp-*", ".DS_Store", "desktop.ini", "Thumbs.db", ".tmp.driveupload/", ".tmp.drivedownload/"} {
		if !slices.Contains(strings.Split(strings.TrimSpace(gitIgnoreContent), "\n"), want) {
			t.Errorf(".gitignore lacks %q", want)
		}
	}
	if st, err := os.Stat(filepath.Join(dir, ".gitignore")); err == nil && st.Mode().Perm() != 0o600 {
		t.Errorf(".gitignore mode = %o, want 0600", st.Mode().Perm())
	}
	if got := rawGit(t, dir, "remote", "get-url", "origin"); got != bare {
		t.Errorf("origin url = %q, want %q", got, bare)
	}
	if got := rawGit(t, dir, "config", "branch.main.remote"); got != "origin" {
		t.Errorf("branch.main.remote = %q", got)
	}
	if got := rawGit(t, dir, "config", "branch.main.merge"); got != "refs/heads/main" {
		t.Errorf("branch.main.merge = %q", got)
	}
	if got := rawGit(t, dir, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Errorf("HEAD = %q", got)
	}

	// Idempotent: a second Prepare keeps user edits to the bootstrap files.
	mustWrite(t, dir, ".gitignore", gitIgnoreContent+"custom\n")
	if err := r.Prepare(ctx, testLog(t)); err != nil {
		t.Fatalf("second Prepare: %v", err)
	}
	if got := mustRead(t, dir, ".gitignore"); !strings.HasSuffix(got, "custom\n") {
		t.Errorf("second Prepare overwrote .gitignore: %q", got)
	}
}

func TestGitPrepareRepointsOrigin(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	bare := newBare(t)
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rawGit(t, dir, "init", "-q")
	rawGit(t, dir, "remote", "add", "origin", "/nonexistent/old.git")

	r, err := NewGit(gitCfg(bare), dir, Options{MachineID: "m1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Prepare(ctx, testLog(t)); err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if got := rawGit(t, dir, "remote", "get-url", "origin"); got != bare {
		t.Errorf("origin url = %q, want %q", got, bare)
	}
	if got := rawGit(t, dir, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Errorf("HEAD = %q, want refs/heads/main (unborn HEAD repointed)", got)
	}
}

func TestGitPushThenSecondMachineAdopts(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	bare := newBare(t)
	a, adir := newGitVault(t, bare, "m1")
	if err := a.Prepare(ctx, testLog(t)); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, adir, "vault.json", vaultJSONv1)
	mustWrite(t, adir, "machines/m1.json.enc", "m1")
	mustWrite(t, adir, "blobs/ab/abcd.enc", "blob")
	mustWrite(t, adir, ".psv-tmp-123", "junk") // ignored
	if err := a.Push(ctx, []string{"vault.json", "machines/m1.json.enc", "blobs/ab/abcd.enc"}, testLog(t)); err != nil {
		t.Fatalf("Push: %v", err)
	}
	head := rawGit(t, adir, "rev-parse", "HEAD")
	if got := rawGit(t, bare, "rev-parse", "refs/heads/main"); got != head {
		t.Fatalf("remote main = %s, want %s", got, head)
	}
	if files := lsFiles(t, adir); slices.Contains(files, ".psv-tmp-123") {
		t.Errorf("temp file was committed: %v", files)
	}
	// Pushing again with nothing new is fine.
	if err := a.Push(ctx, nil, testLog(t)); err != nil {
		t.Fatalf("second Push: %v", err)
	}

	b, bdir := newGitVault(t, bare, "m2")
	if err := b.Prepare(ctx, testLog(t)); err != nil {
		t.Fatalf("B Prepare: %v", err)
	}
	if got := rawGit(t, bdir, "rev-parse", "HEAD"); got != head {
		t.Errorf("B HEAD = %s, want %s", got, head)
	}
	if got := rawGit(t, bdir, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Errorf("B HEAD ref = %q", got)
	}
	for rel, want := range map[string]string{
		"vault.json":           vaultJSONv1,
		"machines/m1.json.enc": "m1",
		"blobs/ab/abcd.enc":    "blob",
		".gitattributes":       gitAttributesContent,
		".gitignore":           gitIgnoreContent,
	} {
		if got := mustRead(t, bdir, rel); got != want {
			t.Errorf("B %s = %q, want %q", rel, got, want)
		}
	}
	if got := rawGit(t, bdir, "status", "--porcelain"); got != "" {
		t.Errorf("B working tree dirty after adoption:\n%s", got)
	}
	// B pushes only its own file; A fetches it.
	mustWrite(t, bdir, "machines/m2.json.enc", "m2")
	if err := b.Push(ctx, []string{"machines/m2.json.enc"}, testLog(t)); err != nil {
		t.Fatalf("B Push: %v", err)
	}
	if err := a.Fetch(ctx, testLog(t)); err != nil {
		t.Fatalf("A Fetch: %v", err)
	}
	if got := mustRead(t, adir, "machines/m2.json.enc"); got != "m2" {
		t.Errorf("A did not receive m2 file: %q", got)
	}
}

// twoSyncedVaults returns two prepared vaults that share the remote history
// (A created the vault, B adopted it).
func twoSyncedVaults(t *testing.T) (a Remote, adir string, b Remote, bdir string) {
	t.Helper()
	ctx := context.Background()
	bare := newBare(t)
	a, adir = newGitVault(t, bare, "m1")
	if err := a.Prepare(ctx, testLog(t)); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, adir, "vault.json", vaultJSONv1)
	if err := a.Push(ctx, []string{"vault.json"}, testLog(t)); err != nil {
		t.Fatal(err)
	}
	b, bdir = newGitVault(t, bare, "m2")
	if err := b.Prepare(ctx, testLog(t)); err != nil {
		t.Fatal(err)
	}
	return a, adir, b, bdir
}

func TestGitFetchPushRoundTrip(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	a, adir, b, bdir := twoSyncedVaults(t)

	mustWrite(t, adir, "projects/p1/state/m1.json.enc", "s1")
	mustWrite(t, adir, "blobs/aa/one.enc", "one")
	if err := a.Push(ctx, []string{"projects/p1/state/m1.json.enc", "blobs/aa/one.enc"}, testLog(t)); err != nil {
		t.Fatalf("A Push: %v", err)
	}
	if err := b.Fetch(ctx, testLog(t)); err != nil {
		t.Fatalf("B Fetch: %v", err)
	}
	if got := mustRead(t, bdir, "projects/p1/state/m1.json.enc"); got != "s1" {
		t.Errorf("B state = %q", got)
	}
	if got := mustRead(t, bdir, "blobs/aa/one.enc"); got != "one" {
		t.Errorf("B blob = %q", got)
	}

	mustWrite(t, bdir, "projects/p1/state/m2.json.enc", "s2")
	if err := b.Push(ctx, []string{"projects/p1/state/m2.json.enc"}, testLog(t)); err != nil {
		t.Fatalf("B Push: %v", err)
	}
	if err := a.Fetch(ctx, testLog(t)); err != nil {
		t.Fatalf("A Fetch: %v", err)
	}
	if got := mustRead(t, adir, "projects/p1/state/m2.json.enc"); got != "s2" {
		t.Errorf("A state from B = %q", got)
	}
	if got := mustRead(t, adir, "projects/p1/state/m1.json.enc"); got != "s1" {
		t.Errorf("A own state changed: %q", got)
	}
	if ah, bh := rawGit(t, adir, "rev-parse", "HEAD"), rawGit(t, bdir, "rev-parse", "HEAD"); ah != bh {
		t.Errorf("heads diverged: A %s B %s", ah, bh)
	}
}

func TestGitPushNonFastForwardRetries(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	a, adir, b, bdir := twoSyncedVaults(t)

	mustWrite(t, adir, "machines/m1.json.enc", "a1")
	if err := a.Push(ctx, []string{"machines/m1.json.enc"}, testLog(t)); err != nil {
		t.Fatal(err)
	}
	// B pushes without fetching first: rejected, rebased, retried.
	mustWrite(t, bdir, "machines/m2.json.enc", "b1")
	var lines []string
	logf := func(s string) { lines = append(lines, s); t.Log(s) }
	if err := b.Push(ctx, []string{"machines/m2.json.enc"}, logf); err != nil {
		t.Fatalf("B Push after divergence: %v", err)
	}
	if !slices.ContainsFunc(lines, func(s string) bool { return strings.Contains(s, "push rejected") }) {
		t.Errorf("expected a rejection/retry log line, got:\n%s", strings.Join(lines, "\n"))
	}
	if got := mustRead(t, bdir, "machines/m1.json.enc"); got != "a1" {
		t.Errorf("B lacks A's file after rebase: %q", got)
	}
	if err := a.Fetch(ctx, testLog(t)); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, adir, "machines/m2.json.enc"); got != "b1" {
		t.Errorf("A lacks B's file: %q", got)
	}
	if ah, bh := rawGit(t, adir, "rev-parse", "HEAD"), rawGit(t, bdir, "rev-parse", "HEAD"); ah != bh {
		t.Errorf("heads diverged: A %s B %s", ah, bh)
	}
	if rh := rawGit(t, filepath.Join(adir), "rev-parse", "refs/remotes/origin/main"); rh != rawGit(t, adir, "rev-parse", "HEAD") {
		t.Errorf("A not at origin/main")
	}
}

func TestGitRebaseConflictIsAborted(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	a, adir, b, bdir := twoSyncedVaults(t)

	mustWrite(t, adir, "shared.txt", "from A\n")
	if err := a.Push(ctx, []string{"shared.txt"}, testLog(t)); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, bdir, "shared.txt", "from B\n")
	err := b.Push(ctx, []string{"shared.txt"}, testLog(t))
	if err == nil {
		t.Fatal("B Push succeeded despite conflicting content")
	}
	var rc *RebaseConflictError
	if !errors.As(err, &rc) {
		t.Fatalf("error is %T (%v), want *RebaseConflictError", err, err)
	}
	if !slices.Equal(rc.Files, []string{"shared.txt"}) {
		t.Errorf("conflict files = %v, want [shared.txt]", rc.Files)
	}
	if !strings.Contains(err.Error(), "shared.txt") {
		t.Errorf("error does not name the file: %v", err)
	}
	for _, d := range []string{".git/rebase-merge", ".git/rebase-apply"} {
		if pathExists(bdir, d) {
			t.Errorf("%s still present: repository left mid-rebase", d)
		}
	}
	if got := rawGit(t, bdir, "status", "--porcelain"); got != "" {
		t.Errorf("B working tree not clean after abort:\n%s", got)
	}
	if got := mustRead(t, bdir, "shared.txt"); got != "from B\n" {
		t.Errorf("B lost its local content: %q", got)
	}
	// Fetch alone reports the same conflict and also leaves the repo clean.
	err = b.Fetch(ctx, testLog(t))
	if !errors.As(err, &rc) {
		t.Fatalf("Fetch error is %T (%v), want *RebaseConflictError", err, err)
	}
	if pathExists(bdir, ".git/rebase-merge") {
		t.Error("rebase-merge present after Fetch conflict")
	}
	if got := rawGit(t, bdir, "symbolic-ref", "HEAD"); got != "refs/heads/main" {
		t.Errorf("HEAD detached after abort: %q", got)
	}
}

func TestGitPrepareVaultConflict(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	bare := newBare(t)
	a, adir := newGitVault(t, bare, "m1")
	if err := a.Prepare(ctx, testLog(t)); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, adir, "vault.json", vaultJSONv1)
	mustWrite(t, adir, "blobs/aa/x.enc", "x")
	if err := a.Push(ctx, []string{"vault.json", "blobs/aa/x.enc"}, testLog(t)); err != nil {
		t.Fatal(err)
	}

	t.Run("different id refused", func(t *testing.T) {
		b, bdir := newGitVault(t, bare, "m2")
		mustWrite(t, bdir, "vault.json", vaultJSONv2)
		err := b.Prepare(ctx, testLog(t))
		if !errors.Is(err, ErrVaultConflict) {
			t.Fatalf("err = %v, want ErrVaultConflict", err)
		}
		var vc *VaultConflict
		if !errors.As(err, &vc) {
			t.Fatalf("err is %T, want *VaultConflict", err)
		}
		if vc.LocalID != "22222222-2222-2222-2222-222222222222" || vc.RemoteID != "11111111-1111-1111-1111-111111111111" {
			t.Errorf("ids = local %q remote %q", vc.LocalID, vc.RemoteID)
		}
		if !strings.Contains(err.Error(), vc.LocalID) || !strings.Contains(err.Error(), vc.RemoteID) {
			t.Errorf("message lacks ids: %v", err)
		}
		if got := mustRead(t, bdir, "vault.json"); got != vaultJSONv2 {
			t.Errorf("local vault.json was modified: %q", got)
		}
		// Nothing was adopted: the local branch is still unborn.
		cmd := rawGitErr(bdir, "rev-parse", "--verify", "-q", "HEAD")
		if cmd == nil {
			t.Error("HEAD exists: remote history was adopted despite the conflict")
		}
	})

	t.Run("same id adopts remote copies", func(t *testing.T) {
		b, bdir := newGitVault(t, bare, "m2")
		// Same id, different bytes: the committed remote copy wins.
		mustWrite(t, bdir, "vault.json", `{"id":"11111111-1111-1111-1111-111111111111","extra":true}`)
		mustWrite(t, bdir, "blobs/aa/x.enc", "x")          // identical content-addressed blob
		mustWrite(t, bdir, "machines/m2.json.enc", "mine") // untracked own file must survive
		if err := b.Prepare(ctx, testLog(t)); err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if got := mustRead(t, bdir, "vault.json"); got != vaultJSONv1 {
			t.Errorf("vault.json = %q, want remote copy", got)
		}
		if got := mustRead(t, bdir, "machines/m2.json.enc"); got != "mine" {
			t.Errorf("untracked own file lost: %q", got)
		}
		if got := rawGit(t, bdir, "rev-parse", "HEAD"); got != rawGit(t, adir, "rev-parse", "HEAD") {
			t.Errorf("B HEAD = %s, want A's", got)
		}
	})
}

func TestGitPrepareRebasesExistingLocalCommits(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	bare := newBare(t)
	a, adir := newGitVault(t, bare, "m1")
	c, cdir := newGitVault(t, bare, "m3")
	if err := a.Prepare(ctx, testLog(t)); err != nil {
		t.Fatal(err)
	}
	if err := c.Prepare(ctx, testLog(t)); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, adir, "vault.json", vaultJSONv1)
	mustWrite(t, adir, "machines/m1.json.enc", "a")
	if err := a.Push(ctx, nil, testLog(t)); err != nil {
		t.Fatal(err)
	}
	// C commits locally (offline) with the same vault id, then re-runs Prepare.
	mustWrite(t, cdir, "vault.json", vaultJSONv1)
	mustWrite(t, cdir, "machines/m3.json.enc", "c")
	rawGit(t, cdir, "add", "-A")
	rawGit(t, cdir, "commit", "-q", "-m", "offline")
	// Plus an uncommitted change that must be committed before the rebase.
	mustWrite(t, cdir, "projects/p/state/m3.json.enc", "dirty")

	if err := c.Prepare(ctx, testLog(t)); err != nil {
		t.Fatalf("C Prepare: %v", err)
	}
	if got := mustRead(t, cdir, "machines/m1.json.enc"); got != "a" {
		t.Errorf("C lacks A's file: %q", got)
	}
	if got := mustRead(t, cdir, "machines/m3.json.enc"); got != "c" {
		t.Errorf("C lost its own file: %q", got)
	}
	if got := rawGit(t, cdir, "status", "--porcelain"); got != "" {
		t.Errorf("C dirty after Prepare:\n%s", got)
	}
	if got := rawGit(t, cdir, "merge-base", "--is-ancestor", "refs/remotes/origin/main", "HEAD"); got != "" {
		t.Errorf("unexpected output: %q", got)
	}
	if err := c.Push(ctx, nil, testLog(t)); err != nil {
		t.Fatalf("C Push: %v", err)
	}
	if err := a.Fetch(ctx, testLog(t)); err != nil {
		t.Fatal(err)
	}
	if got := mustRead(t, adir, "projects/p/state/m3.json.enc"); got != "dirty" {
		t.Errorf("A lacks C's file: %q", got)
	}
}

func TestGitFetchCommitsLeftoverChanges(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	a, adir, _, _ := twoSyncedVaults(t)
	mustWrite(t, adir, "machines/m1.json.enc", "left over")
	if err := a.Fetch(ctx, testLog(t)); err != nil {
		t.Fatal(err)
	}
	if got := rawGit(t, adir, "status", "--porcelain"); got != "" {
		t.Errorf("dirty after Fetch:\n%s", got)
	}
	if !slices.Contains(lsFiles(t, adir), "machines/m1.json.enc") {
		t.Error("leftover file not committed by Fetch")
	}
}

func TestGitFetchAndPushBeforePrepare(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	r, _ := newGitVault(t, newBare(t), "m1")
	for name, fn := range map[string]func() error{
		"Fetch": func() error { return r.Fetch(ctx, nil) },
		"Push":  func() error { return r.Push(ctx, nil, nil) },
	} {
		err := fn()
		if err == nil || !strings.Contains(err.Error(), "not a git repository") {
			t.Errorf("%s before Prepare: err = %v", name, err)
		}
	}
}

func TestGitPushEmptyRepoIsNoop(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "vault")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	rawGit(t, dir, "init", "-q", "-b", "main")
	r, err := NewGit(gitCfg(newBare(t)), dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	if err := r.Push(ctx, nil, func(s string) { lines = append(lines, s) }); err != nil {
		t.Fatalf("Push on empty unborn repo: %v", err)
	}
	if !slices.Contains(lines, "nothing to push") {
		t.Errorf("expected 'nothing to push', got %v", lines)
	}
}

func TestGitBadRemoteURLIsAnError(t *testing.T) {
	requireGit(t)
	ctx := context.Background()
	r, _ := newGitVault(t, filepath.Join(t.TempDir(), "does-not-exist.git"), "m1")
	err := r.Prepare(ctx, testLog(t))
	if err == nil {
		t.Fatal("Prepare against a missing repository succeeded")
	}
	if errors.Is(err, ErrAuth) || errors.Is(err, ErrNetwork) {
		t.Errorf("local path failure misclassified: %v", err)
	}
}

func TestGitContextCancelled(t *testing.T) {
	requireGit(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r, _ := newGitVault(t, newBare(t), "m1")
	if err := r.Prepare(ctx, testLog(t)); err == nil {
		t.Fatal("Prepare with cancelled context succeeded")
	}
}
