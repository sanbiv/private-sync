package cli

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/app"
	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/paths"
	"github.com/sanbiv/private-sync/internal/sync"
	"github.com/sanbiv/private-sync/internal/ui"
)

// scriptedPrompter answers Password from a list and Confirm from a list
// (default when exhausted); every question is recorded.
type scriptedPrompter struct {
	passwords []string
	confirms  []bool
	asked     []string
}

func (p *scriptedPrompter) Password(_ context.Context, title string) ([]byte, error) {
	p.asked = append(p.asked, title)
	if len(p.passwords) == 0 {
		return nil, ui.ErrNonInteractive
	}
	v := p.passwords[0]
	p.passwords = p.passwords[1:]
	return []byte(v), nil
}

func (p *scriptedPrompter) Confirm(_ context.Context, title string, def bool) (bool, error) {
	p.asked = append(p.asked, title)
	if len(p.confirms) == 0 {
		return def, nil
	}
	v := p.confirms[0]
	p.confirms = p.confirms[1:]
	return v, nil
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// createVault runs the vault creation protocol (remote none, key file) and
// links proj as "myapp" through the session API (the `add` wizard is the TUI's).
func createVault(t *testing.T, dirs paths.Dirs, proj string) (projectID, vaultID string) {
	t.Helper()
	ctx := context.Background()
	a, err := app.Load(dirs.ConfigFile(), dirs, noGit())
	if err != nil {
		t.Fatalf("app.Load: %v", err)
	}
	s, err := a.Setup(ctx, ui.Silent{}, nil)
	if err != nil {
		t.Fatalf("app.Setup: %v", err)
	}
	defer s.Close()
	fps, _, err := s.Identify(ctx, proj)
	if err != nil {
		t.Fatalf("Identify: %v", err)
	}
	id, err := s.ProjectID(fps, false)
	if err != nil {
		t.Fatalf("ProjectID: %v", err)
	}
	if err := s.LinkProject(ctx, id, "myapp", proj, fps); err != nil {
		t.Fatalf("LinkProject: %v", err)
	}
	return id, s.Vault.ID()
}

func mustContain(t *testing.T, r result, wantCode int, wantOut ...string) {
	t.Helper()
	if r.code != wantCode {
		t.Fatalf("exit = %d, want %d\nstdout: %s\nstderr: %s", r.code, wantCode, r.out, r.err)
	}
	for _, w := range wantOut {
		if !strings.Contains(r.out, w) {
			t.Fatalf("stdout lacks %q:\n%s\nstderr: %s", w, r.out, r.err)
		}
	}
}

type statusJSON struct {
	Projects []struct {
		ID    string `json:"id"`
		Name  string `json:"name"`
		Badge string `json:"badge"`
		Items []struct {
			Path   string `json:"path"`
			Action string `json:"action"`
		} `json:"items"`
	} `json:"projects"`
}

func decode(t *testing.T, s string, v any) {
	t.Helper()
	if err := json.Unmarshal([]byte(s), v); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, s)
	}
}

func TestEndToEndSingleMachine(t *testing.T) {
	root, dirs := isolate(t)
	writeConfig(t, dirs, root, "alpha")
	keyPath := filepath.Join(root, "key")
	proj := filepath.Join(root, "proj")
	envPath := filepath.Join(proj, ".env")
	v1 := "DB_HOST=localhost\nDB_PORT=5432\n"
	writeFile(t, envPath, v1)
	writeFile(t, filepath.Join(proj, "config.yaml"), "name: myapp\n")
	writeFile(t, filepath.Join(proj, "secrets.json"), `{"token":"x"}`)
	writeFile(t, filepath.Join(proj, "README.md"), "hello\n")
	writeFile(t, filepath.Join(proj, "node_modules", "pkg", "settings.json"), "{}")

	// scan needs the config only (no vault yet).
	r := runCLI(t, runOpts{}, "scan", proj)
	mustContain(t, r, 0, ".env", "config.yaml", "secrets.json", "SCORE")
	if strings.Contains(r.out, "node_modules") || strings.Contains(r.out, "README.md") {
		t.Fatalf("excluded files listed:\n%s", r.out)
	}
	r = runCLI(t, runOpts{}, "--json", "scan", proj)
	var scanOut struct {
		Dir        string `json:"dir"`
		Candidates []struct {
			Path    string   `json:"path"`
			Score   string   `json:"score"`
			Reasons []string `json:"reasons"`
		} `json:"candidates"`
		GitInfo bool `json:"git_info"`
	}
	decode(t, r.out, &scanOut)
	if r.code != 0 || scanOut.Dir != proj || len(scanOut.Candidates) < 3 || scanOut.GitInfo {
		t.Fatalf("scan --json: %d %+v", r.code, scanOut)
	}
	r = runCLI(t, runOpts{}, "scan", filepath.Join(root, "nope"))
	if r.code != ExitError {
		t.Fatalf("scan of a missing dir: %d %q", r.code, r.err)
	}

	// The vault does not exist yet: commands that need it say so.
	r = runCLI(t, runOpts{}, "unlock")
	if r.code != ExitError || !strings.Contains(r.err, "init") {
		t.Fatalf("unlock without vault: %d %q", r.code, r.err)
	}

	projectID, vaultID := createVault(t, dirs, proj)

	r = runCLI(t, runOpts{}, "unlock")
	mustContain(t, r, 0, "vault "+vaultID+" opened")

	// files add: upload two files.
	r = runCLI(t, runOpts{}, "files", "add", "myapp", ".env", "config.yaml", "--no-remote")
	mustContain(t, r, 0, "↑ .env", "↑ config.yaml", "uploaded 2")
	r = runCLI(t, runOpts{}, "files", "add", "myapp", "missing.env", "--no-remote")
	if r.code != ExitError {
		t.Fatalf("files add of a missing file: %d", r.code)
	}

	// status: everything in sync, JSON and text.
	r = runCLI(t, runOpts{}, "status", "--json")
	var st statusJSON
	decode(t, r.out, &st)
	if r.code != 0 || len(st.Projects) != 1 || st.Projects[0].ID != projectID || st.Projects[0].Name != "myapp" || st.Projects[0].Badge != "synced" {
		t.Fatalf("status --json: %d %+v", r.code, st)
	}
	if len(st.Projects[0].Items) != 2 {
		t.Fatalf("items = %+v", st.Projects[0].Items)
	}
	for _, it := range st.Projects[0].Items {
		if it.Action != "in-sync" {
			t.Fatalf("item %+v not in sync", it)
		}
	}
	r = runCLI(t, runOpts{}, "status")
	mustContain(t, r, 0, "= .env", "= config.yaml", "myapp ("+projectID+")")

	// A local edit shows up in status and is uploaded by sync.
	v2 := "DB_HOST=db.internal\nDB_PORT=5432\n"
	writeFile(t, envPath, v2)
	r = runCLI(t, runOpts{}, "status")
	mustContain(t, r, 0, "↑ .env", "= config.yaml")
	r = runCLI(t, runOpts{}, "sync", "--no-remote")
	mustContain(t, r, 0, "↑ .env", "= config.yaml", "uploaded 1")
	r = runCLI(t, runOpts{}, "push", "--no-remote")
	mustContain(t, r, 0, "nothing to do")
	r = runCLI(t, runOpts{}, "--json", "sync", "--no-remote")
	var rep struct {
		Mode   string `json:"mode"`
		Report struct {
			Uploaded int `json:"uploaded"`
		} `json:"report"`
		Unresolved []any `json:"unresolved"`
	}
	decode(t, r.out, &rep)
	if r.code != 0 || rep.Mode != "sync" || rep.Report.Uploaded != 0 || len(rep.Unresolved) != 0 {
		t.Fatalf("sync --json: %d %+v", r.code, rep)
	}

	// projects list.
	r = runCLI(t, runOpts{}, "projects", "list")
	mustContain(t, r, 0, projectID, "myapp", "synced")
	r = runCLI(t, runOpts{}, "--json", "projects", "list")
	var pl struct {
		Linked   []struct{ ID, Name, Badge string } `json:"linked"`
		Unlinked []struct{ ID, Name string }        `json:"unlinked"`
	}
	decode(t, r.out, &pl)
	if len(pl.Linked) != 1 || pl.Linked[0].ID != projectID || pl.Linked[0].Badge != "synced" || len(pl.Unlinked) != 0 {
		t.Fatalf("projects list --json: %+v", pl)
	}

	// files rm: untracked everywhere, local copy kept.
	r = runCLI(t, runOpts{}, "files", "rm", "myapp", "config.yaml", "--no-remote")
	mustContain(t, r, 0, "untracked 1 file")
	if _, err := os.Stat(filepath.Join(proj, "config.yaml")); err != nil {
		t.Fatalf("local copy removed by files rm: %v", err)
	}
	r = runCLI(t, runOpts{}, "status", "--json")
	decode(t, r.out, &st)
	for _, it := range st.Projects[0].Items {
		switch it.Path {
		case ".env":
			if it.Action != "in-sync" {
				t.Fatalf(".env after rm: %+v", it)
			}
		case "config.yaml":
			if it.Action == "upload" || it.Action == "download" {
				t.Fatalf("config.yaml still synced after rm: %+v", it)
			}
		}
	}
	r = runCLI(t, runOpts{}, "files", "rm", "myapp", "never-tracked.txt", "--no-remote")
	if r.code != ExitError || !strings.Contains(r.err, "not tracked") {
		t.Fatalf("rm of an untracked path: %d %q", r.code, r.err)
	}
	r = runCLI(t, runOpts{}, "files", "rm", "nosuch", "x", "--no-remote")
	if r.code != ExitError || !strings.Contains(r.err, "not linked") {
		t.Fatalf("rm on an unknown project: %d %q", r.code, r.err)
	}

	// restore puts the modified local copy in the trash and writes the vault version.
	v3 := "DB_HOST=db.internal\nDB_PORT=9999\n"
	writeFile(t, envPath, v3)
	pp := &scriptedPrompter{confirms: []bool{false}}
	r = runCLI(t, runOpts{prompter: pp}, "restore", "myapp", "--no-remote")
	if r.code != ExitConflicts || len(pp.asked) != 1 || readFile(t, envPath) != v3 {
		t.Fatalf("declined restore: %d asked=%v content=%q stderr=%q", r.code, pp.asked, readFile(t, envPath), r.err)
	}
	r = runCLI(t, runOpts{}, "restore", "myapp", "--no-remote")
	mustContain(t, r, 0, "↓ .env")
	if got := readFile(t, envPath); got != v2 {
		t.Fatalf("restored .env = %q, want %q", got, v2)
	}
	r = runCLI(t, runOpts{}, "trash", "list")
	mustContain(t, r, 0, ".env", "myapp")
	r = runCLI(t, runOpts{}, "--json", "trash", "list")
	var trash struct {
		Entries []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
			Size int64  `json:"size"`
		} `json:"entries"`
	}
	decode(t, r.out, &trash)
	if len(trash.Entries) != 1 || trash.Entries[0].Path != ".env" || trash.Entries[0].Size != int64(len(v3)) {
		t.Fatalf("trash entries = %+v", trash.Entries)
	}
	entryID := trash.Entries[0].ID

	// trash restore refuses to overwrite a different file without --yes.
	r = runCLI(t, runOpts{}, "trash", "restore", entryID)
	if r.code != ExitError || !strings.Contains(r.err, "--yes") || readFile(t, envPath) != v2 {
		t.Fatalf("trash restore without --yes: %d %q", r.code, r.err)
	}
	r = runCLI(t, runOpts{}, "trash", "restore", entryID, "--yes")
	mustContain(t, r, 0, "restored .env")
	if got := readFile(t, envPath); got != v3 {
		t.Fatalf("trash restore wrote %q, want %q", got, v3)
	}
	r = runCLI(t, runOpts{}, "trash", "restore", entryID, "--yes")
	mustContain(t, r, 0, "already has the content")
	r = runCLI(t, runOpts{}, "trash", "restore", "no-such-id")
	if r.code != ExitError {
		t.Fatalf("trash restore of an unknown id: %d", r.code)
	}
	r = runCLI(t, runOpts{}, "sync", "--no-remote")
	mustContain(t, r, 0, "↑ .env")

	// A file missing locally is reported, and deleted everywhere only with --delete.
	if err := os.Remove(envPath); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, runOpts{}, "status")
	mustContain(t, r, 0, "? .env")
	r = runCLI(t, runOpts{}, "sync", "--no-remote")
	mustContain(t, r, 0, "? .env")
	r = runCLI(t, runOpts{}, "sync", "--no-remote", "--delete") // strategy ask, no terminal
	mustContain(t, r, 0, "↑ .env", "deleted 1")
	r = runCLI(t, runOpts{}, "status", "--json")
	decode(t, r.out, &st)
	for _, it := range st.Projects[0].Items {
		if it.Path == ".env" && (it.Action == "upload" || it.Action == "download" || it.Action == "missing-local") {
			t.Fatalf(".env after --delete: %+v", it)
		}
	}

	// Re-add, then delete everywhere: --yes accepts the confirmation but
	// never removes the local copy; --local does (pre-image in the trash).
	v4 := "DB_HOST=again\n"
	writeFile(t, envPath, v4)
	r = runCLI(t, runOpts{}, "files", "add", "myapp", ".env", "--no-remote")
	mustContain(t, r, 0, "↑ .env")
	r = runCLI(t, runOpts{}, "files", "delete", "myapp", ".env", "--no-remote")
	if r.code != ExitConflicts {
		t.Fatalf("files delete declined by the silent prompter: %d %q", r.code, r.err)
	}
	r = runCLI(t, runOpts{}, "files", "delete", "myapp", ".env", "--yes", "--no-remote")
	mustContain(t, r, 0, "deleted 1 file", "local copy kept")
	if readFile(t, envPath) != v4 {
		t.Fatal("--yes removed the local copy")
	}
	r = runCLI(t, runOpts{}, "status")
	mustContain(t, r, 0, ".env", "deleted in the vault")

	other := filepath.Join(proj, "other.env")
	writeFile(t, other, "X=1\n")
	r = runCLI(t, runOpts{}, "files", "add", "myapp", "other.env", "--no-remote")
	mustContain(t, r, 0, "↑ other.env")
	r = runCLI(t, runOpts{}, "files", "delete", "myapp", "other.env", "--yes", "--local", "--no-remote")
	mustContain(t, r, 0, "deleted 1 file", "copy in the trash")
	if _, err := os.Stat(other); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("--local did not remove the file: %v", err)
	}
	r = runCLI(t, runOpts{}, "--json", "trash", "list")
	decode(t, r.out, &trash)
	// other.env (delete --local), .env (overwritten by trash restore --yes), .env (restore)
	if len(trash.Entries) != 3 || trash.Entries[0].Path != "other.env" {
		t.Fatalf("trash after delete --local = %+v", trash.Entries)
	}

	// trash purge.
	r = runCLI(t, runOpts{}, "trash", "purge", "--older-than", "30d")
	mustContain(t, r, 0, "purged 0")
	r = runCLI(t, runOpts{}, "trash", "purge", "--older-than", "0s")
	mustContain(t, r, 0, "purged 3")
	r = runCLI(t, runOpts{}, "trash", "list")
	mustContain(t, r, 0, "trash is empty")

	// unlink (config only) and link back.
	r = runCLI(t, runOpts{}, "projects", "unlink", "myapp")
	mustContain(t, r, 0, "unlinked myapp")
	r = runCLI(t, runOpts{}, "projects", "unlink", "myapp")
	if r.code != ExitError {
		t.Fatalf("unlink twice: %d", r.code)
	}
	r = runCLI(t, runOpts{}, "projects", "list")
	mustContain(t, r, 0, "no linked projects", "not linked", projectID, "dir:proj")
	r = runCLI(t, runOpts{}, "projects", "link", "myapp", proj)
	mustContain(t, r, 0, "linked myapp ("+projectID+")", "pull "+projectID)
	r = runCLI(t, runOpts{}, "projects", "list")
	mustContain(t, r, 0, projectID, "myapp")
	if strings.Contains(r.out, "not linked") {
		t.Fatalf("still unlinked:\n%s", r.out)
	}
	r = runCLI(t, runOpts{}, "projects", "link", "ghost", proj)
	if r.code != ExitError || !strings.Contains(r.err, "project not found") {
		t.Fatalf("link of an unknown project: %d %q", r.code, r.err)
	}

	// restore --path links an unlinked vault project first.
	r = runCLI(t, runOpts{}, "projects", "unlink", projectID)
	mustContain(t, r, 0, "unlinked")
	proj2 := filepath.Join(root, "proj2")
	if err := os.MkdirAll(proj2, 0o755); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, runOpts{}, "--json", "restore", "myapp", "--path", proj2, "--no-remote", "--yes")
	var restored struct {
		Mode   string `json:"mode"`
		Linked *struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			Path string `json:"path"`
		} `json:"linked"`
	}
	decode(t, r.out, &restored) // one JSON document: no "linked ..." text line before it
	if r.code != 0 || restored.Mode != "restore" || restored.Linked == nil ||
		restored.Linked.ID != projectID || restored.Linked.Name != "myapp" || restored.Linked.Path != paths.ContractHome(proj2) {
		t.Fatalf("restore --json --path: %d %+v\n%s", r.code, restored, r.out)
	}
	if strings.Contains(r.err, "was linked to") {
		t.Fatalf("relink warning for an unlinked project: %q", r.err)
	}
	cfg, err := config.Load(dirs.ConfigFile())
	if err != nil {
		t.Fatal(err)
	}
	if p, ok := cfg.Project(projectID); !ok || p.Path != paths.ContractHome(proj2) {
		t.Fatalf("restore --path did not link: %+v", cfg.Projects)
	}
	// Already linked elsewhere: the mapping is replaced with the same
	// warning as projects link; the same directory again is no replacement.
	proj3 := filepath.Join(root, "proj3")
	if err := os.MkdirAll(proj3, 0o755); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, runOpts{}, "restore", "myapp", "--path", proj3, "--no-remote", "--yes")
	mustContain(t, r, 0, "linked myapp ("+projectID+") to "+paths.ContractHome(proj3))
	if !strings.Contains(r.err, "was linked to "+paths.ContractHome(proj2)) {
		t.Fatalf("no relink warning: %q", r.err)
	}
	r = runCLI(t, runOpts{}, "restore", "myapp", "--path", proj3, "--no-remote", "--yes")
	mustContain(t, r, 0, "linked myapp")
	if strings.Contains(r.err, "was linked to") {
		t.Fatalf("relink warning for the same directory: %q", r.err)
	}
	r = runCLI(t, runOpts{}, "projects", "link", "myapp", proj2)
	mustContain(t, r, 0, "linked myapp")
	if !strings.Contains(r.err, "was linked to "+paths.ContractHome(proj3)) {
		t.Fatalf("projects link: no relink warning: %q", r.err)
	}

	// passphrase change: twice through the prompter, then the key file is stale.
	pp = &scriptedPrompter{passwords: []string{"first", "second"}}
	r = runCLI(t, runOpts{prompter: pp}, "passphrase", "change", "--no-remote")
	if r.code != ExitError || !strings.Contains(r.err, "do not match") {
		t.Fatalf("mismatching passphrases: %d %q", r.code, r.err)
	}
	pp = &scriptedPrompter{passwords: []string{"  ", "  "}}
	r = runCLI(t, runOpts{prompter: pp}, "passphrase", "change", "--no-remote")
	if r.code != ExitError || !strings.Contains(r.err, "empty") {
		t.Fatalf("empty passphrase: %d %q", r.code, r.err)
	}
	pp = &scriptedPrompter{passwords: []string{"new-secret", "new-secret"}}
	r = runCLI(t, runOpts{prompter: pp}, "passphrase", "change", "--no-remote")
	mustContain(t, r, 0, "key file ", " updated", "passphrase changed for vault "+vaultID)
	if strings.Contains(r.err, "warning") {
		t.Fatalf("unexpected warning: %q", r.err)
	}
	// key.source is file: the key file now holds the new passphrase, so the
	// next command opens the vault without any manual step.
	if got := readFile(t, keyPath); got != "new-secret\n" {
		t.Fatalf("key file = %q, want the new passphrase", got)
	}
	r = runCLI(t, runOpts{}, "unlock")
	mustContain(t, r, 0, "vault "+vaultID+" opened")
	if err := os.WriteFile(keyPath, []byte("not-the-passphrase\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, runOpts{}, "unlock")
	if r.code != ExitError || !strings.Contains(r.err, "wrong passphrase") {
		t.Fatalf("unlock with a wrong key file: %d %q", r.code, r.err)
	}
}

// useMachine points the XDG directories at one machine's home below root
// and returns its dirs.
func useMachine(t *testing.T, root, name string) paths.Dirs {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(root, name, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(root, name, "state"))
	dirs, err := paths.Default()
	if err != nil {
		t.Fatal(err)
	}
	return dirs
}

func TestEndToEndConflicts(t *testing.T) {
	root, _ := isolate(t)

	// Machine A creates the vault and uploads .env.
	dirsA := useMachine(t, root, "a")
	writeConfig(t, dirsA, root, "alpha")
	projA := filepath.Join(root, "projA")
	envA := filepath.Join(projA, ".env")
	writeFile(t, envA, "DB_PORT=1\n")
	projectID, _ := createVault(t, dirsA, projA)
	r := runCLI(t, runOpts{}, "files", "add", "myapp", ".env", "--no-remote")
	mustContain(t, r, 0, "↑ .env")

	// Machine B opens the same vault, links the project and pulls.
	dirsB := useMachine(t, root, "b")
	writeConfig(t, dirsB, root, "beta")
	projB := filepath.Join(root, "projB")
	envB := filepath.Join(projB, ".env")
	if err := os.MkdirAll(projB, 0o755); err != nil {
		t.Fatal(err)
	}
	r = runCLI(t, runOpts{}, "projects", "list")
	mustContain(t, r, 0, "not linked", projectID)
	r = runCLI(t, runOpts{}, "projects", "link", "myapp", projB)
	mustContain(t, r, 0, "linked myapp")
	r = runCLI(t, runOpts{}, "pull", "--no-remote")
	mustContain(t, r, 0, "↓ .env", "downloaded 1")
	if readFile(t, envB) != "DB_PORT=1\n" {
		t.Fatalf("B .env = %q", readFile(t, envB))
	}

	// A and B change the same key.
	useMachine(t, root, "a")
	writeFile(t, envA, "DB_PORT=2\n")
	r = runCLI(t, runOpts{}, "sync", "--no-remote")
	mustContain(t, r, 0, "↑ .env")

	useMachine(t, root, "b")
	writeFile(t, envB, "DB_PORT=3\n")
	r = runCLI(t, runOpts{}, "status")
	mustContain(t, r, 0, "! .env", "conflicts")
	r = runCLI(t, runOpts{}, "status", "--json")
	var st statusJSON
	decode(t, r.out, &st)
	if st.Projects[0].Badge != "conflicts" || st.Projects[0].Items[0].Action != "conflict" {
		t.Fatalf("status --json = %+v", st)
	}

	// Non-interactive without --yes: strategy ask leaves the conflict → exit 3.
	r = runCLI(t, runOpts{}, "sync", "--no-remote")
	mustContain(t, r, ExitConflicts, "! .env", "unresolved 1")
	if !strings.Contains(r.err, "unresolved conflicts") {
		t.Fatalf("stderr = %q", r.err)
	}
	// --yes defaults to abort: skipped, still exit 3, nothing changed.
	r = runCLI(t, runOpts{}, "sync", "--no-remote", "--yes")
	mustContain(t, r, ExitConflicts, "! .env")
	if readFile(t, envB) != "DB_PORT=3\n" {
		t.Fatal("abort strategy touched the local file")
	}
	// pull and push must resolve conflicts too.
	r = runCLI(t, runOpts{}, "pull", "--no-remote")
	mustContain(t, r, ExitConflicts, "! .env")

	// Interactive: the front end's resolver is called; abort → exit 3.
	fe := &fakeFrontend{resolve: func(*sync.Plan) (sync.Resolutions, bool, error) { return nil, true, nil }}
	r = runCLI(t, runOpts{interactive: true, fe: fe}, "sync", "--no-remote")
	if r.code != ExitConflicts || fe.resolveCalls != 1 || !strings.Contains(r.err, "aborted") {
		t.Fatalf("aborted resolver: %d calls=%d stderr=%q", r.code, fe.resolveCalls, r.err)
	}
	if got := fe.resolvePlans[0].Unresolved(nil); len(got) != 1 || got[0].Path != ".env" {
		t.Fatalf("resolver saw %v", got)
	}
	fe = &fakeFrontend{resolve: func(*sync.Plan) (sync.Resolutions, bool, error) { return nil, false, errors.New("tui exploded") }}
	r = runCLI(t, runOpts{interactive: true, fe: fe}, "sync", "--no-remote")
	if r.code != ExitError || !strings.Contains(r.err, "tui exploded") {
		t.Fatalf("failing resolver: %d %q", r.code, r.err)
	}
	// Interactive with an answer: keep local → uploaded, exit 0.
	fe = &fakeFrontend{resolve: func(p *sync.Plan) (sync.Resolutions, bool, error) {
		res := sync.Resolutions{}
		for _, k := range p.Unresolved(nil) {
			res[k] = sync.Resolution{Kind: sync.ChooseLocal}
		}
		return res, false, nil
	}}
	r = runCLI(t, runOpts{interactive: true, fe: fe}, "sync", "--no-remote")
	mustContain(t, r, 0, "↑ .env", "kept the local version", "resolved 1")
	if fe.resolveCalls != 1 || readFile(t, envB) != "DB_PORT=3\n" {
		t.Fatalf("resolved sync: calls=%d content=%q", fe.resolveCalls, readFile(t, envB))
	}
	// --yes with the interactive flag never calls the resolver.
	fe = &fakeFrontend{}
	r = runCLI(t, runOpts{interactive: true, fe: fe}, "sync", "--no-remote", "--yes")
	if r.code != 0 || fe.resolveCalls != 0 {
		t.Fatalf("--yes sync: %d calls=%d", r.code, fe.resolveCalls)
	}

	// A pulls B's resolution.
	useMachine(t, root, "a")
	r = runCLI(t, runOpts{}, "pull", "--no-remote")
	mustContain(t, r, 0, "↓ .env")
	if readFile(t, envA) != "DB_PORT=3\n" {
		t.Fatalf("A .env = %q", readFile(t, envA))
	}

	// Second round: --strategy remote takes the vault version non-interactively.
	writeFile(t, envA, "DB_PORT=4\n")
	r = runCLI(t, runOpts{}, "sync", "--no-remote")
	mustContain(t, r, 0, "↑ .env")
	useMachine(t, root, "b")
	writeFile(t, envB, "DB_PORT=5\n")
	r = runCLI(t, runOpts{}, "sync", "--no-remote", "--strategy", "remote")
	mustContain(t, r, 0, "↓ .env", "took the vault version")
	if readFile(t, envB) != "DB_PORT=4\n" {
		t.Fatalf("B .env after --strategy remote = %q", readFile(t, envB))
	}
	r = runCLI(t, runOpts{}, "trash", "list")
	mustContain(t, r, 0, ".env") // B's pre-image was kept

	// Third round: --strategy local with --yes wins over the abort default.
	useMachine(t, root, "a")
	writeFile(t, envA, "DB_PORT=6\n")
	r = runCLI(t, runOpts{}, "push", "--no-remote")
	mustContain(t, r, 0, "↑ .env")
	useMachine(t, root, "b")
	writeFile(t, envB, "DB_PORT=7\n")
	r = runCLI(t, runOpts{}, "--yes", "--strategy", "local", "sync", "--no-remote")
	mustContain(t, r, 0, "↑ .env", "kept the local version")
	useMachine(t, root, "a")
	r = runCLI(t, runOpts{}, "sync", "--no-remote")
	mustContain(t, r, 0, "↓ .env")
	if readFile(t, envA) != "DB_PORT=7\n" {
		t.Fatalf("A .env = %q", readFile(t, envA))
	}
}

func TestRootAndInitUseTheFrontend(t *testing.T) {
	root, dirs := isolate(t)

	// First run: no config → RunSetup(nil) → vault created → dashboard.
	cfg := config.Default(dirs)
	cfg.Machine.Name = "wizard-box"
	cfg.Vault.Path = filepath.Join(root, "vault")
	cfg.Vault.Remote = config.RemoteConfig{Type: config.RemoteNone}
	cfg.Key = config.KeyConfig{Source: config.KeyFile, File: config.FileKey{Path: filepath.Join(root, "key")}}
	fe := &fakeFrontend{setupCfg: cfg}
	r := runCLI(t, runOpts{fe: fe})
	if r.code != 0 {
		t.Fatalf("first run: %d %q", r.code, r.err)
	}
	if len(fe.setupCalls) != 1 || fe.setupCalls[0] != nil || fe.setupDirs != dirs {
		t.Fatalf("RunSetup calls = %v dirs = %+v", fe.setupCalls, fe.setupDirs)
	}
	if fe.runCalls != 1 || len(fe.sessions) != 1 || fe.sessions[0].Vault == nil {
		t.Fatalf("Run calls = %d sessions = %v", fe.runCalls, fe.sessions)
	}
	if !strings.Contains(r.out, "config written to") || !strings.Contains(r.out, "vault ") {
		t.Fatalf("stdout = %q", r.out)
	}
	if _, err := os.Stat(dirs.ConfigFile()); err != nil {
		t.Fatalf("config not saved: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "key")); err != nil {
		t.Fatalf("key file not generated by Setup: %v", err)
	}

	// Second run: config exists → no wizard, dashboard with an open session.
	fe = &fakeFrontend{}
	r = runCLI(t, runOpts{fe: fe})
	if r.code != 0 || len(fe.setupCalls) != 0 || fe.runCalls != 1 {
		t.Fatalf("second run: %d setup=%v run=%d stderr=%q", r.code, fe.setupCalls, fe.runCalls, r.err)
	}
	// The dashboard's error becomes the exit code.
	fe = &fakeFrontend{runErr: errors.New("dashboard crashed")}
	r = runCLI(t, runOpts{fe: fe})
	if r.code != ExitError || !strings.Contains(r.err, "dashboard crashed") {
		t.Fatalf("dashboard error: %d %q", r.code, r.err)
	}

	// init passes the existing config to the wizard and opens the vault.
	cfg2 := *cfg
	cfg2.Machine.Name = "renamed-box"
	fe = &fakeFrontend{setupCfg: &cfg2}
	r = runCLI(t, runOpts{fe: fe}, "init")
	if r.code != 0 || len(fe.setupCalls) != 1 || fe.setupCalls[0] == nil || fe.setupCalls[0].Machine.Name != "wizard-box" || fe.runCalls != 0 {
		t.Fatalf("init: %d setup=%v run=%d stderr=%q", r.code, fe.setupCalls, fe.runCalls, r.err)
	}
	if !strings.Contains(r.out, "ready at") {
		t.Fatalf("init stdout = %q", r.out)
	}
	saved, err := config.Load(dirs.ConfigFile())
	if err != nil || saved.Machine.Name != "renamed-box" {
		t.Fatalf("init did not save the wizard config: %v %+v", err, saved)
	}

	// A cancelled wizard is an abort (exit 3); a failing one an error (exit 1).
	fe = &fakeFrontend{}
	r = runCLI(t, runOpts{fe: fe}, "init")
	if r.code != ExitConflicts || !strings.Contains(r.err, "cancelled") {
		t.Fatalf("cancelled wizard: %d %q", r.code, r.err)
	}
	fe = &fakeFrontend{setupErr: errors.New("form failed")}
	r = runCLI(t, runOpts{fe: fe}, "init")
	if r.code != ExitError || !strings.Contains(r.err, "form failed") {
		t.Fatalf("failing wizard: %d %q", r.code, r.err)
	}

	// add opens the session, fetches and hands the absolute directory to the wizard.
	proj := filepath.Join(root, "proj")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	fe = &fakeFrontend{}
	r = runCLI(t, runOpts{fe: fe}, "add", proj)
	if r.code != 0 || len(fe.addDirs) != 1 || fe.addDirs[0] != proj {
		t.Fatalf("add: %d dirs=%v stderr=%q", r.code, fe.addDirs, r.err)
	}
	fe = &fakeFrontend{addErr: errors.New("wizard failed")}
	r = runCLI(t, runOpts{fe: fe}, "add", proj)
	if r.code != ExitError || !strings.Contains(r.err, "wizard failed") {
		t.Fatalf("add error: %d %q", r.code, r.err)
	}
	r = runCLI(t, runOpts{fe: &fakeFrontend{}}, "add", filepath.Join(root, "missing"))
	if r.code != ExitError {
		t.Fatalf("add of a missing dir: %d", r.code)
	}
}

// captureMain runs Main with os.Stdout/os.Stderr redirected into a pipe.
func captureMain(t *testing.T, fe Frontend, args ...string) (int, string) {
	t.Helper()
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	oldOut, oldErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = pw, pw
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(pr)
		done <- string(b)
	}()
	code := Main(args, fe)
	os.Stdout, os.Stderr = oldOut, oldErr
	_ = pw.Close()
	out := <-done
	_ = pr.Close()
	t.Logf("$ private-sync %s → %d\n%s", strings.Join(args, " "), code, out)
	return code, out
}

func TestMainEntryPoint(t *testing.T) {
	root, dirs := isolate(t)
	writeConfig(t, dirs, root, "main-box")
	proj := filepath.Join(root, "proj")
	writeFile(t, filepath.Join(proj, ".env"), "A=1\n")
	projectID, vaultID := createVault(t, dirs, proj)

	code, out := captureMain(t, &fakeFrontend{}, "machine")
	if code != 0 || !strings.Contains(out, "main-box") {
		t.Fatalf("machine: %d %q", code, out)
	}
	code, out = captureMain(t, &fakeFrontend{}, "config", "path")
	if code != 0 || strings.TrimSpace(out) != dirs.ConfigFile() {
		t.Fatalf("config path: %d %q", code, out)
	}
	code, out = captureMain(t, &fakeFrontend{}, "unlock")
	if code != 0 || !strings.Contains(out, "vault "+vaultID+" opened") {
		t.Fatalf("unlock: %d %q", code, out)
	}
	code, out = captureMain(t, &fakeFrontend{}, "files", "add", "myapp", ".env", "--no-remote")
	if code != 0 || !strings.Contains(out, "↑ .env") {
		t.Fatalf("files add: %d %q", code, out)
	}
	code, out = captureMain(t, &fakeFrontend{}, "--json", "status")
	var st statusJSON
	decode(t, out, &st)
	if code != 0 || len(st.Projects) != 1 || st.Projects[0].ID != projectID {
		t.Fatalf("status: %d %+v", code, st)
	}
	code, out = captureMain(t, &fakeFrontend{}, "--strategy", "bogus", "status")
	if code != ExitUsage || !strings.Contains(out, "error: invalid --strategy") {
		t.Fatalf("usage error: %d %q", code, out)
	}
	code, out = captureMain(t, &fakeFrontend{}, "nope")
	if code != ExitUsage || !strings.Contains(out, "unknown command") {
		t.Fatalf("unknown command: %d %q", code, out)
	}
}
