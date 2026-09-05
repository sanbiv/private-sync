package sync

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/remote"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/vault"
)

const (
	mA     = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	mB     = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	mC     = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	projID = "0123456789abcdef"
)

func pass() []byte { return []byte("correct horse battery staple") }

// fastParams returns the cheapest KDF parameters the spec allows.
func fastParams(t *testing.T) crypto.KDFParams {
	t.Helper()
	p, err := crypto.DefaultKDFParams()
	if err != nil {
		t.Fatalf("DefaultKDFParams: %v", err)
	}
	p.Time = crypto.KDFMinTime
	p.Memory = crypto.KDFMinMemory
	p.Threads = crypto.KDFMinThreads
	return p
}

// machine is one simulated computer: its own vault handle, state dir, project
// dir and engine, all sharing one vault directory with the other machines.
type machine struct {
	t        *testing.T
	id       string
	v        *vault.Vault
	st       *state.Store
	cfg      *config.Config
	dir      string
	stateDir string
	eng      *Engine
}

// newVaultDir creates a fresh vault (writer mA) and returns its dir and the
// creating handle (used by machine A).
func newVaultDir(t *testing.T) (string, *vault.Vault) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "vault")
	v, err := vault.Create(dir, pass(), fastParams(t), mA)
	if err != nil {
		t.Fatalf("vault.Create: %v", err)
	}
	t.Cleanup(v.Close)
	return dir, v
}

// newMachine wires a machine around an open vault handle.
func newMachine(t *testing.T, id, name string, v *vault.Vault) *machine {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(root, "state")
	st, err := state.Open(stateDir, v.ID())
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	cfg := &config.Config{
		Version: config.CurrentVersion,
		Machine: config.MachineConfig{Name: name},
		Vault:   config.VaultConfig{Path: v.Dir(), Remote: config.RemoteConfig{Type: config.RemoteNone}},
		Projects: []config.ProjectConfig{
			{ID: projID, Name: "myapp", Path: dir},
		},
	}
	eng := New(v, st, cfg, remote.None{}, state.Machine{ID: id, CreatedAt: time.Unix(0, 0)})
	eng.hostname = func() string { return name + ".local" }
	return &machine{t: t, id: id, v: v, st: st, cfg: cfg, dir: dir, stateDir: stateDir, eng: eng}
}

// openMachine opens the shared vault as another machine.
func openMachine(t *testing.T, vaultDir, id, name string) *machine {
	t.Helper()
	v, err := vault.Open(vaultDir, pass(), id)
	if err != nil {
		t.Fatalf("vault.Open(%s): %v", id, err)
	}
	t.Cleanup(v.Close)
	return newMachine(t, id, name, v)
}

// twoMachines returns machines A and B sharing one vault directory.
func twoMachines(t *testing.T) (*machine, *machine) {
	t.Helper()
	dir, va := newVaultDir(t)
	a := newMachine(t, mA, "alpha", va)
	b := openMachine(t, dir, mB, "beta")
	return a, b
}

func (m *machine) abs(p string) string { return filepath.Join(m.dir, filepath.FromSlash(p)) }

func (m *machine) write(p, content string, mode ...fs.FileMode) {
	m.t.Helper()
	perm := fs.FileMode(0o600)
	if len(mode) > 0 {
		perm = mode[0]
	}
	abs := m.abs(p)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		m.t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(content), perm); err != nil {
		m.t.Fatal(err)
	}
	if err := os.Chmod(abs, perm); err != nil {
		m.t.Fatal(err)
	}
}

func (m *machine) remove(p string) {
	m.t.Helper()
	if err := os.Remove(m.abs(p)); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.t.Fatal(err)
	}
}

func (m *machine) exists(p string) bool {
	_, err := os.Lstat(m.abs(p))
	return err == nil
}

func (m *machine) read(p string) string {
	m.t.Helper()
	b, err := os.ReadFile(m.abs(p))
	if err != nil {
		m.t.Fatalf("read %s on %s: %v", p, m.id[:8], err)
	}
	return string(b)
}

func (m *machine) mode(p string) fs.FileMode {
	m.t.Helper()
	st, err := os.Lstat(m.abs(p))
	if err != nil {
		m.t.Fatal(err)
	}
	return st.Mode().Perm()
}

func (m *machine) plan(opts Options) *Plan {
	m.t.Helper()
	pl, err := m.eng.Plan(context.Background(), opts)
	if err != nil {
		m.t.Fatalf("Plan on %s: %v", m.id[:8], err)
	}
	return pl
}

func (m *machine) apply(pl *Plan, res Resolutions, opts Options) *Report {
	m.t.Helper()
	rep, err := m.eng.Apply(context.Background(), pl, res, opts)
	if err != nil {
		m.t.Fatalf("Apply on %s: %v", m.id[:8], err)
	}
	return rep
}

// sync plans, applies the strategy and applies; per-item errors are fatal.
func (m *machine) sync(opts Options) (*Plan, *Report) {
	m.t.Helper()
	pl := m.plan(opts)
	res := Resolutions{}
	ApplyStrategy(pl, opts.Strategy, res)
	rep := m.apply(pl, res, opts)
	for _, e := range rep.Errors {
		m.t.Fatalf("sync on %s: %s: %v", m.id[:8], e.Key.Path, e.Err)
	}
	return pl, rep
}

// item returns the plan item for a path of the test project (fatal when absent).
func (m *machine) item(pl *Plan, p string) *Item {
	m.t.Helper()
	it := pl.Find(ItemKey{Project: projID, Path: p})
	if it == nil {
		m.t.Fatalf("no item for %s on %s; items: %v", p, m.id[:8], describeItems(pl))
	}
	return it
}

func describeItems(pl *Plan) []string {
	var out []string
	for _, it := range pl.Items() {
		out = append(out, it.Key.Path+"="+it.Action.String())
	}
	return out
}

func (m *machine) base(p string) (state.BaseEntry, bool) { return m.st.Base(projID, p) }

func (m *machine) heads() map[string]vault.Head {
	m.t.Helper()
	js, _, err := m.v.ReadJournals(projID)
	if err != nil {
		m.t.Fatal(err)
	}
	return vault.ResolveHeads(js)
}

func (m *machine) head(p string) vault.Head {
	m.t.Helper()
	h, ok := m.heads()[p]
	if !ok {
		m.t.Fatalf("no head for %s", p)
	}
	return h
}

func (m *machine) trash() []state.TrashEntry {
	m.t.Helper()
	l, err := m.st.TrashList()
	if err != nil {
		m.t.Fatal(err)
	}
	return l
}

func (m *machine) journalFile() string {
	return filepath.Join(m.v.Dir(), "projects", projID, "state", m.id+".json.enc")
}

func track(paths ...string) map[ItemKey]bool {
	out := map[ItemKey]bool{}
	for _, p := range paths {
		out[ItemKey{Project: projID, Path: p}] = true
	}
	return out
}

// seed uploads files from A and syncs B so both are in sync.
func seed(a, b *machine, files map[string]string) {
	a.t.Helper()
	var paths []string
	for p, c := range files {
		a.write(p, c)
		paths = append(paths, p)
	}
	a.sync(Options{Track: track(paths...)})
	b.sync(Options{})
	for p, c := range files {
		if got := b.read(p); got != c {
			a.t.Fatalf("seed: %s on B = %q, want %q", p, got, c)
		}
	}
}

func wantAction(t *testing.T, it *Item, action Action) {
	t.Helper()
	if it.Action != action {
		t.Fatalf("%s: action = %v, want %v (reason: %s)", it.Key.Path, it.Action, action, it.Reason)
	}
}

func wantConflict(t *testing.T, it *Item, kind ConflictKind, needs bool) {
	t.Helper()
	wantAction(t, it, ActionConflict)
	if it.Conflict != kind {
		t.Fatalf("%s: conflict = %v, want %v", it.Key.Path, it.Conflict, kind)
	}
	if it.NeedsResolution != needs {
		t.Fatalf("%s: NeedsResolution = %v, want %v", it.Key.Path, it.NeedsResolution, needs)
	}
}

func noItem(t *testing.T, pl *Plan, p string) {
	t.Helper()
	if it := pl.Find(ItemKey{Project: projID, Path: p}); it != nil {
		t.Fatalf("unexpected item for %s: %v (%s)", p, it.Action, it.Reason)
	}
}

func noErrors(t *testing.T, rep *Report) {
	t.Helper()
	for _, e := range rep.Errors {
		t.Errorf("item error: %s: %v", e.Key.Path, e.Err)
	}
	if t.Failed() {
		t.FailNow()
	}
}
