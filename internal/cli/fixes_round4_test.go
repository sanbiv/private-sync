package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/keysource"
	"github.com/sanbiv/private-sync/internal/remote"
	"github.com/sanbiv/private-sync/internal/sync"
)

// reportJSON is the part of the --json report these tests read.
type reportJSON struct {
	Projects []struct {
		ID      string `json:"id"`
		Badge   string `json:"badge"`
		Summary struct {
			Missing        int `json:"missing"`
			DeletedInVault int `json:"deleted_in_vault"`
		} `json:"summary"`
		Items []struct {
			Path   string `json:"path"`
			Action string `json:"action"`
			Error  string `json:"error"`
		} `json:"items"`
	} `json:"projects"`
	Errors     []string `json:"errors"`
	Unresolved []struct {
		Path string `json:"path"`
	} `json:"unresolved"`
}

// TestFilesDeleteKeepsLocalWhenTrashFails: `files delete --local` must not
// remove the last plaintext copy when the pre-image could not reach the
// encrypted trash (spec §13). A corrupt trash index makes TrashPut fail
// before it writes anything.
func TestFilesDeleteKeepsLocalWhenTrashFails(t *testing.T) {
	root, dirs := isolate(t)
	writeConfig(t, dirs, root, "alpha")
	proj := filepath.Join(root, "proj")
	env := filepath.Join(proj, ".env")
	writeFile(t, env, "SECRET=1\n")
	_, vaultID := createVault(t, dirs, proj)
	r := runCLI(t, runOpts{}, "files", "add", "myapp", ".env", "--no-remote")
	mustContain(t, r, 0, "↑ .env")

	writeFile(t, filepath.Join(dirs.State, "vaults", vaultID, "trash", "index.json"), "{ not json")

	r = runCLI(t, runOpts{}, "files", "delete", "myapp", ".env", "--local", "--yes", "--no-remote")
	if r.code != 0 {
		t.Fatalf("files delete: exit %d\n%s%s", r.code, r.out, r.err)
	}
	if got := readFile(t, env); got != "SECRET=1\n" {
		t.Fatalf(".env = %q: the local copy was removed although the trash refused the pre-image", got)
	}
	if !strings.Contains(r.err, "local copy kept") {
		t.Fatalf("stderr = %q, want a warning saying the local copy is kept", r.err)
	}
	if !strings.Contains(r.out, "local copy kept") || strings.Contains(r.out, "copy in the trash") {
		t.Fatalf("stdout = %q, want the file reported as kept", r.out)
	}

	// --json must not list it under removed_local either.
	writeFile(t, filepath.Join(proj, "other.env"), "SECRET=2\n")
	r = runCLI(t, runOpts{}, "files", "add", "myapp", "other.env", "--no-remote")
	mustContain(t, r, 0, "↑ other.env")
	r = runCLI(t, runOpts{}, "--json", "files", "delete", "myapp", "other.env", "--local", "--yes", "--no-remote")
	var del struct {
		Deleted      []string `json:"deleted"`
		RemovedLocal []string `json:"removed_local"`
	}
	decode(t, r.out, &del)
	if len(del.RemovedLocal) != 0 {
		t.Fatalf("removed_local = %v, want none", del.RemovedLocal)
	}
	if readFile(t, filepath.Join(proj, "other.env")) != "SECRET=2\n" {
		t.Fatal("other.env was removed although the trash refused the pre-image")
	}
}

// logRemote is the none remote whose Fetch/Push echo command lines.
type logRemote struct{ remote.None }

func (logRemote) Fetch(_ context.Context, log func(string)) error {
	log("$ git fetch origin")
	log("ERROR : remote chatter")
	return nil
}

func (logRemote) Push(_ context.Context, _ []string, log func(string)) error {
	log("$ git add -A")
	return nil
}

// TestRemoteLogOnlyWithVerbose: the remote command lines are debugging detail.
// They stay out of the report unless --verbose is given, and a remote failure
// prints the captured tail.
func TestRemoteLogOnlyWithVerbose(t *testing.T) {
	root, dirs := isolate(t)
	writeConfig(t, dirs, root, "alpha")
	proj := filepath.Join(root, "proj")
	writeFile(t, filepath.Join(proj, ".env"), "A=1\n")
	createVault(t, dirs, proj)

	logging := func(config.RemoteConfig, string, remote.Options) (remote.Remote, error) {
		return logRemote{}, nil
	}
	r := runCLI(t, runOpts{remoteFactory: logging}, "files", "add", "myapp", ".env")
	mustContain(t, r, 0, "↑ .env", "uploaded 1")
	for _, unwanted := range []string{"$ git fetch origin", "ERROR : remote chatter", "$ git add -A"} {
		if strings.Contains(r.err, unwanted) {
			t.Fatalf("stderr echoes %q without --verbose:\n%s", unwanted, r.err)
		}
	}
	writeFile(t, filepath.Join(proj, "b.env"), "B=1\n")
	r = runCLI(t, runOpts{remoteFactory: logging}, "--verbose", "files", "add", "myapp", "b.env")
	mustContain(t, r, 0, "↑ b.env")
	for _, want := range []string{"fetch: $ git fetch origin", "push: $ git add -A"} {
		if !strings.Contains(r.err, want) {
			t.Fatalf("stderr lacks %q with --verbose:\n%s", want, r.err)
		}
	}

	// A failed push prints the tail that --verbose would have shown live.
	c := newCLI(nil, Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}})
	errOut := c.errOut.(*bytes.Buffer)
	for i := 0; i < maxRemoteLogTail+5; i++ {
		c.progress(sync.Event{Stage: "push", Message: "$ line " + string(rune('a'+i))})
	}
	if errOut.Len() != 0 {
		t.Fatalf("log lines printed without --verbose: %q", errOut.String())
	}
	c.dumpRemoteLog()
	got := errOut.String()
	if strings.Count(got, "push: $ line ") != maxRemoteLogTail {
		t.Fatalf("dump kept %d lines, want %d:\n%s", strings.Count(got, "push: $ line "), maxRemoteLogTail, got)
	}
	if strings.Contains(got, "$ line a") {
		t.Fatalf("dump kept the oldest line instead of the tail:\n%s", got)
	}
	errOut.Reset()
	c.dumpRemoteLog()
	if errOut.Len() != 0 {
		t.Fatalf("second dump repeated the log: %q", errOut.String())
	}
	// Failures are always reported, verbose or not.
	c.progress(sync.Event{Stage: "push", Message: "push failed", Err: context.Canceled})
	if !strings.Contains(errOut.String(), "push failed") {
		t.Fatalf("failure event not printed: %q", errOut.String())
	}
}

// TestDeletedInVaultBadge: decision table row 9 (deleted in the vault, local
// copy deliberately kept) must not be reported as "missing locally" while the
// file is on disk.
func TestDeletedInVaultBadge(t *testing.T) {
	root, dirs := isolate(t)
	writeConfig(t, dirs, root, "alpha")
	proj := filepath.Join(root, "proj")
	cfgPath := filepath.Join(proj, "config.yaml")
	writeFile(t, cfgPath, "name: myapp\n")
	createVault(t, dirs, proj)
	r := runCLI(t, runOpts{}, "files", "add", "myapp", "config.yaml", "--no-remote")
	mustContain(t, r, 0, "↑ config.yaml")
	r = runCLI(t, runOpts{}, "files", "delete", "myapp", "config.yaml", "--yes", "--no-remote")
	mustContain(t, r, 0, "local copy kept")
	if readFile(t, cfgPath) != "name: myapp\n" {
		t.Fatal("the local copy was removed without --local")
	}

	r = runCLI(t, runOpts{}, "status", "--json")
	var st reportJSON
	decode(t, r.out, &st)
	if r.code != 0 || len(st.Projects) != 1 {
		t.Fatalf("status --json: %d %+v", r.code, st)
	}
	p := st.Projects[0]
	if p.Badge != "deleted in the vault, local copy kept" {
		t.Fatalf("badge = %q, want the row 9 badge", p.Badge)
	}
	if p.Summary.Missing != 0 || p.Summary.DeletedInVault != 1 {
		t.Fatalf("summary = %+v, want missing 0 and deleted_in_vault 1", p.Summary)
	}
	r = runCLI(t, runOpts{}, "status")
	mustContain(t, r, 0, "deleted in the vault (local copy kept)")
	if strings.Contains(r.out, "missing locally") {
		t.Fatalf("status still says the file is missing:\n%s", r.out)
	}

	// A genuinely absent tracked file is still "missing locally".
	local := &sync.FileRef{}
	key := sync.ItemKey{Project: "p", Path: "f"}
	absent := &sync.ProjectPlan{ID: "p", Items: []sync.Item{{Key: key, Action: sync.ActionMissingLocal}}}
	if got := badge(absent); got != "missing locally" {
		t.Fatalf("absent file badge = %q", got)
	}
	kept := &sync.ProjectPlan{ID: "p", Items: []sync.Item{
		{Key: key, Action: sync.ActionReportOnly, Original: sync.ActionMissingLocal, Local: local},
	}}
	if got := badge(kept); got != "deleted in the vault, local copy kept" {
		t.Fatalf("row 9 badge = %q", got)
	}
	if s := summarize(kept); s.Missing != 0 || s.DeletedInVault != 1 {
		t.Fatalf("summarize = %+v", s)
	}
}

// TestPassphraseChangeRefusesCapturedEnv: PRIVATE_SYNC_PASSPHRASE overrides
// every configured key source, so a rekey would leave the captured value
// stale and lock the shell out of the vault; the command must refuse instead.
func TestPassphraseChangeRefusesCapturedEnv(t *testing.T) {
	root, dirs := isolate(t)
	writeConfig(t, dirs, root, "alpha")
	proj := filepath.Join(root, "proj")
	writeFile(t, filepath.Join(proj, ".env"), "A=1\n")
	createVault(t, dirs, proj)
	keyBefore := readFile(t, filepath.Join(root, "key"))

	setEnvPassphraseSeen(true)
	t.Cleanup(func() { setEnvPassphraseSeen(false) })

	pp := &scriptedPrompter{passwords: []string{"pw-2", "pw-2"}}
	r := runCLI(t, runOpts{prompter: pp}, "passphrase", "change", "--no-remote")
	if r.code != ExitError {
		t.Fatalf("passphrase change: exit %d, want %d\n%s%s", r.code, ExitError, r.out, r.err)
	}
	if !strings.Contains(r.err, keysource.EnvVar) {
		t.Fatalf("stderr = %q, want it to name %s", r.err, keysource.EnvVar)
	}
	if len(pp.asked) != 0 {
		t.Fatalf("the new passphrase was asked for before the refusal: %v", pp.asked)
	}
	if got := readFile(t, filepath.Join(root, "key")); got != keyBefore {
		t.Fatalf("key file rewritten (%q) although the rekey was refused", got)
	}
	// The vault still opens with the unchanged key file.
	r = runCLI(t, runOpts{}, "status", "--no-remote")
	if r.code != 0 {
		t.Fatalf("status after the refused rekey: exit %d\n%s%s", r.code, r.out, r.err)
	}
}

// TestJSONIsNonInteractive: --json owns stdout, so the full screen conflict
// resolver must not run; the conflict is reported as unresolved instead.
func TestJSONIsNonInteractive(t *testing.T) {
	yes := true
	c := newCLI(nil, Options{Stdout: &bytes.Buffer{}, Stderr: &bytes.Buffer{}, Interactive: &yes})
	c.g.json = true
	if c.isInteractive() {
		t.Fatal("--json is interactive")
	}
	c.g.json = false
	if !c.isInteractive() {
		t.Fatal("the override no longer applies without --json")
	}

	root, _ := isolate(t)
	dirsA := useMachine(t, root, "a")
	writeConfig(t, dirsA, root, "alpha")
	projA := filepath.Join(root, "projA")
	envA := filepath.Join(projA, ".env")
	writeFile(t, envA, "DB_PORT=1\n")
	createVault(t, dirsA, projA)
	r := runCLI(t, runOpts{}, "files", "add", "myapp", ".env", "--no-remote")
	mustContain(t, r, 0, "↑ .env")

	dirsB := useMachine(t, root, "b")
	writeConfig(t, dirsB, root, "beta")
	projB := filepath.Join(root, "projB")
	if err := os.MkdirAll(projB, 0o755); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, runOpts{}, "projects", "link", "myapp", projB)
	mustContain(t, r, 0, "linked myapp")
	r = runCLI(t, runOpts{}, "pull", "--no-remote")
	mustContain(t, r, 0, "↓ .env")

	useMachine(t, root, "a")
	writeFile(t, envA, "DB_PORT=2\n")
	r = runCLI(t, runOpts{}, "sync", "--no-remote")
	mustContain(t, r, 0, "↑ .env")

	useMachine(t, root, "b")
	writeFile(t, filepath.Join(projB, ".env"), "DB_PORT=3\n")
	fe := &fakeFrontend{}
	r = runCLI(t, runOpts{interactive: true, fe: fe}, "--json", "sync", "--no-remote")
	if fe.resolveCalls != 0 {
		t.Fatalf("the resolver ran %d time(s) with --json (its UI renders on stdout)", fe.resolveCalls)
	}
	if r.code != ExitConflicts {
		t.Fatalf("sync --json: exit %d, want %d\n%s%s", r.code, ExitConflicts, r.out, r.err)
	}
	var rep reportJSON
	if err := json.Unmarshal([]byte(r.out), &rep); err != nil {
		t.Fatalf("stdout is not one JSON document (%v):\n%s", err, r.out)
	}
	if len(rep.Unresolved) != 1 || rep.Unresolved[0].Path != ".env" {
		t.Fatalf("unresolved = %+v", rep.Unresolved)
	}
}

// TestApplyItemErrorsFailTheCommand: a per item failure is printed by the
// report and must also fail the command, so a cron or CI wrapper keyed on the
// exit code does not treat a failed sync as a successful one.
func TestApplyItemErrorsFailTheCommand(t *testing.T) {
	root, _ := isolate(t)
	dirsA := useMachine(t, root, "a")
	writeConfig(t, dirsA, root, "alpha")
	projA := filepath.Join(root, "projA")
	writeFile(t, filepath.Join(projA, ".env"), "A=1\n")
	createVault(t, dirsA, projA)
	r := runCLI(t, runOpts{}, "files", "add", "myapp", ".env", "--no-remote")
	mustContain(t, r, 0, "↑ .env")

	dirsB := useMachine(t, root, "b")
	writeConfig(t, dirsB, root, "beta")
	projB := filepath.Join(root, "projB")
	if err := os.MkdirAll(projB, 0o755); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, runOpts{}, "projects", "link", "myapp", projB)
	mustContain(t, r, 0, "linked myapp")

	// A read-only project directory: the download cannot write its temp file,
	// which Apply records as a per item error.
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("a read-only directory needs POSIX permissions and a non-root user")
	}
	if err := os.Chmod(projB, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(projB, 0o700) })

	r = runCLI(t, runOpts{}, "sync", "--no-remote", "--yes")
	if r.code == 0 {
		t.Fatalf("sync exited 0 although an item failed:\n%s%s", r.out, r.err)
	}
	if !strings.Contains(r.out, "errors 1") {
		t.Fatalf("stdout lacks the error counter:\n%s", r.out)
	}
	if !strings.Contains(r.err, "item(s) failed") {
		t.Fatalf("stderr = %q, want it to say an item failed", r.err)
	}

	r = runCLI(t, runOpts{}, "--json", "sync", "--no-remote", "--yes")
	if r.code == 0 {
		t.Fatalf("sync --json exited 0 although an item failed:\n%s%s", r.out, r.err)
	}
	var rep reportJSON
	decode(t, r.out, &rep)
	if len(rep.Errors) != 1 {
		t.Fatalf("report errors = %v", rep.Errors)
	}
}

// TestUnreadableJournalFailsSync: Plan drops every item of a project whose
// journal cannot be decrypted (spec §5), so the run must not report success.
func TestUnreadableJournalFailsSync(t *testing.T) {
	root, dirs := isolate(t)
	writeConfig(t, dirs, root, "alpha")
	proj := filepath.Join(root, "proj")
	writeFile(t, filepath.Join(proj, ".env"), "A=1\n")
	projectID, _ := createVault(t, dirs, proj)
	r := runCLI(t, runOpts{}, "files", "add", "myapp", ".env", "--no-remote")
	mustContain(t, r, 0, "↑ .env")

	stateDir := filepath.Join(root, "vault", "projects", projectID, "state")
	entries, err := os.ReadDir(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json.enc") {
			writeFile(t, filepath.Join(stateDir, e.Name()), "not a sealed journal")
			n++
		}
	}
	if n == 0 {
		t.Fatalf("no journal in %s", stateDir)
	}

	r = runCLI(t, runOpts{}, "sync", "--no-remote", "--yes")
	if r.code == 0 {
		t.Fatalf("sync exited 0 with an unreadable journal:\n%s%s", r.out, r.err)
	}
	if !strings.Contains(r.out, "nothing applied") {
		t.Fatalf("stdout = %q, want it to say nothing was applied", r.out)
	}
	if !strings.Contains(r.err, "could not be read") {
		t.Fatalf("stderr = %q", r.err)
	}
	r = runCLI(t, runOpts{}, "status")
	if r.code == 0 {
		t.Fatalf("status exited 0 with an unreadable journal:\n%s%s", r.out, r.err)
	}
	r = runCLI(t, runOpts{}, "status", "--json")
	if r.code == 0 {
		t.Fatalf("status --json exited 0 with an unreadable journal:\n%s%s", r.out, r.err)
	}
	var st reportJSON
	if err := json.Unmarshal([]byte(r.out), &st); err != nil {
		t.Fatalf("stdout is not one JSON document (%v):\n%s", err, r.out)
	}
}

// TestFilesAddFailsWhenNothingTracked: `files add` must not exit 0 when the
// planner refused the requested path (here: over max_file_size), otherwise a
// script believes the file is protected.
func TestFilesAddFailsWhenNothingTracked(t *testing.T) {
	root, dirs := isolate(t)
	writeConfig(t, dirs, root, "alpha")
	proj := filepath.Join(root, "proj")
	writeFile(t, filepath.Join(proj, ".env"), "A=1\n")
	big := filepath.Join(proj, "big.key")
	if err := os.WriteFile(big, make([]byte, 3<<20), 0o600); err != nil {
		t.Fatal(err)
	}
	createVault(t, dirs, proj)

	r := runCLI(t, runOpts{}, "files", "add", "myapp", "big.key", "--no-remote")
	if r.code == 0 {
		t.Fatalf("files add exited 0 although nothing was tracked:\n%s%s", r.out, r.err)
	}
	if !strings.Contains(r.err, "not tracked") || !strings.Contains(r.err, "big.key") {
		t.Fatalf("stderr = %q, want it to name the untracked path", r.err)
	}
	// A path the planner accepts still succeeds.
	r = runCLI(t, runOpts{}, "files", "add", "myapp", ".env", "--no-remote")
	mustContain(t, r, 0, "↑ .env", "uploaded 1")
}
