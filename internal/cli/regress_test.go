package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/remote"
)

// TestDeletePropagationNonInteractive: --delete confirms the deletions it
// produces under every non-interactive strategy (spec §13: it is the only
// way to propagate a local deletion without a terminal), including the
// abort default of --yes; --yes alone never deletes.
func TestDeletePropagationNonInteractive(t *testing.T) {
	root, dirs := isolate(t)
	writeConfig(t, dirs, root, "alpha")
	proj := filepath.Join(root, "proj")
	for _, f := range []string{"a.env", "b.env", "c.env", "d.env"} {
		writeFile(t, filepath.Join(proj, f), "K="+f+"\n")
	}
	createVault(t, dirs, proj)
	r := runCLI(t, runOpts{}, "files", "add", "myapp", "a.env", "b.env", "c.env", "d.env", "--no-remote")
	mustContain(t, r, 0, "uploaded 4")

	actionOf := func(path string) string {
		t.Helper()
		r := runCLI(t, runOpts{}, "status", "--json")
		var st statusJSON
		decode(t, r.out, &st)
		if r.code != 0 || len(st.Projects) != 1 {
			t.Fatalf("status --json: %d %+v", r.code, st)
		}
		for _, it := range st.Projects[0].Items {
			if it.Path == path {
				return it.Action
			}
		}
		return ""
	}
	gone := func(path string) {
		t.Helper()
		switch got := actionOf(path); got {
		case "missing-local", "upload", "download":
			t.Fatalf("%s still %s after the deletion was propagated", path, got)
		}
	}
	remove := func(name string) {
		t.Helper()
		if err := os.Remove(filepath.Join(proj, name)); err != nil {
			t.Fatal(err)
		}
	}

	// --yes alone never deletes anything.
	remove("a.env")
	r = runCLI(t, runOpts{}, "sync", "--no-remote", "--yes")
	mustContain(t, r, 0, "? a.env")
	if got := actionOf("a.env"); got != "missing-local" {
		t.Fatalf("a.env after sync --yes = %q, want missing-local", got)
	}

	// --yes --delete: the abort strategy must not swallow the confirmed deletion.
	r = runCLI(t, runOpts{}, "sync", "--no-remote", "--yes", "--delete")
	mustContain(t, r, 0, "↑ a.env", "deleted in the vault", "deleted 1")
	if strings.Contains(r.out, "unresolved") || strings.Contains(r.err, "unresolved") || strings.Contains(r.out, "not confirmed") {
		t.Fatalf("deletion left unresolved:\n%s%s", r.out, r.err)
	}
	gone("a.env")

	// The same with an explicit --strategy abort.
	remove("b.env")
	r = runCLI(t, runOpts{}, "--strategy", "abort", "sync", "--no-remote", "--delete")
	mustContain(t, r, 0, "↑ b.env", "deleted 1")
	gone("b.env")

	// push honours --delete too; the JSON report counts it and lists nothing unresolved.
	remove("c.env")
	r = runCLI(t, runOpts{}, "--json", "push", "--no-remote", "--yes", "--delete")
	var rep struct {
		Report struct {
			Deleted int `json:"deleted"`
			Skipped int `json:"skipped"`
		} `json:"report"`
		Unresolved []any `json:"unresolved"`
	}
	decode(t, r.out, &rep)
	if r.code != 0 || rep.Report.Deleted != 1 || rep.Report.Skipped != 0 || len(rep.Unresolved) != 0 {
		t.Fatalf("push --json --yes --delete: %d %+v", r.code, rep)
	}
	gone("c.env")

	// Interactive (strategy ask): the deletions are already resolved, so the
	// front end's resolver is never invoked.
	remove("d.env")
	fe := &fakeFrontend{}
	r = runCLI(t, runOpts{interactive: true, fe: fe}, "sync", "--no-remote", "--delete")
	mustContain(t, r, 0, "↑ d.env", "deleted 1")
	if fe.resolveCalls != 0 {
		t.Fatalf("resolver called %d times for confirmed deletions", fe.resolveCalls)
	}
	gone("d.env")
}

// pushFailRemote is the none remote with a failing Push.
type pushFailRemote struct {
	remote.None
	err error
}

func (r pushFailRemote) Push(context.Context, []string, func(string)) error { return r.err }

// TestPassphraseChangeKeyFileAndPush: with key.source=file the key file is
// rewritten with the new passphrase right after the rekey, so the next
// command opens the vault even when the push fails; that failure says the
// local vault.json already carries the new passphrase.
func TestPassphraseChangeKeyFileAndPush(t *testing.T) {
	root, dirs := isolate(t)
	writeConfig(t, dirs, root, "alpha")
	keyPath := filepath.Join(root, "key")
	proj := filepath.Join(root, "proj")
	writeFile(t, filepath.Join(proj, ".env"), "A=1\n")
	_, vaultID := createVault(t, dirs, proj)

	failing := func(config.RemoteConfig, string, remote.Options) (remote.Remote, error) {
		return pushFailRemote{err: errors.New("network down")}, nil
	}
	pp := &scriptedPrompter{passwords: []string{"pw-2", "pw-2"}}
	r := runCLI(t, runOpts{prompter: pp, remoteFactory: failing}, "passphrase", "change")
	if r.code != ExitError {
		t.Fatalf("failing push: exit %d, want %d\n%s%s", r.code, ExitError, r.out, r.err)
	}
	for _, want := range []string{"error: passphrase changed locally", "vault.json", "network down", "passphrase change"} {
		if !strings.Contains(r.err, want) {
			t.Fatalf("stderr %q lacks %q", r.err, want)
		}
	}
	if !strings.Contains(r.out, "key file ") || !strings.Contains(r.out, " updated") {
		t.Fatalf("stdout = %q", r.out)
	}
	if got := readFile(t, keyPath); got != "pw-2\n" {
		t.Fatalf("key file = %q, want the new passphrase", got)
	}
	if runtime.GOOS != "windows" {
		st, err := os.Stat(keyPath)
		if err != nil || st.Mode().Perm() != 0o600 {
			t.Fatalf("key file mode = %v, %v", st.Mode(), err)
		}
	}
	r = runCLI(t, runOpts{}, "unlock")
	mustContain(t, r, 0, "vault "+vaultID+" opened")

	// A reachable remote: pushed, and --json keeps stdout to one document.
	pp = &scriptedPrompter{passwords: []string{"pw-3", "pw-3"}}
	r = runCLI(t, runOpts{prompter: pp}, "--json", "passphrase", "change")
	var out struct {
		Vault   string `json:"vault"`
		KeyFile string `json:"key_file"`
		Pushed  bool   `json:"pushed"`
	}
	decode(t, r.out, &out)
	if r.code != 0 || out.Vault != vaultID || !out.Pushed || out.KeyFile == "" {
		t.Fatalf("passphrase change --json: %d %+v", r.code, out)
	}
	if got := readFile(t, keyPath); got != "pw-3\n" {
		t.Fatalf("key file = %q", got)
	}
	r = runCLI(t, runOpts{}, "unlock")
	mustContain(t, r, 0, "vault "+vaultID+" opened")

	// When the key file cannot be rewritten the change still succeeds, with
	// the manual instruction as a warning.
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		return
	}
	if err := os.Chmod(root, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o700) })
	pp = &scriptedPrompter{passwords: []string{"pw-4", "pw-4"}}
	r = runCLI(t, runOpts{prompter: pp}, "passphrase", "change", "--no-remote")
	mustContain(t, r, 0, "passphrase changed for vault "+vaultID)
	if !strings.Contains(r.err, "could not be updated") || !strings.Contains(r.err, "0600") {
		t.Fatalf("stderr = %q", r.err)
	}
	if got := readFile(t, keyPath); got != "pw-3\n" {
		t.Fatalf("key file changed although the write failed: %q", got)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, runOpts{}, "unlock")
	if r.code != ExitError || !strings.Contains(r.err, "wrong passphrase") {
		t.Fatalf("unlock with the stale key file: %d %q", r.code, r.err)
	}
}

func TestUpdateKeyFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := updateKeyFile("", []byte("x")); err == nil {
		t.Fatal("empty path accepted")
	}
	if _, err := updateKeyFile(filepath.Join(dir, "missing"), []byte("x")); err == nil {
		t.Fatal("missing key file rewritten")
	}
	path := filepath.Join(dir, "key")
	writeFile(t, path, "old\n")
	link := filepath.Join(dir, "link")
	if err := os.Symlink(path, link); err != nil {
		t.Skipf("symlink: %v", err)
	}
	written, err := updateKeyFile(link, []byte("new"))
	if err != nil {
		t.Fatal(err)
	}
	if resolved, _ := filepath.EvalSymlinks(path); written != resolved {
		t.Fatalf("written %q, want the resolved target %q", written, resolved)
	}
	if got := readFile(t, path); got != "new\n" {
		t.Fatalf("target = %q", got)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced: %v %v", st, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), "tmp") {
			t.Fatalf("temp file left behind: %s", e.Name())
		}
	}
}
