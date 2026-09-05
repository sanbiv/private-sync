package sync

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/vault"
)

// TestTwoMachineIntegration walks two machines (two state dirs, two project
// dirs, one shared vault dir, remote none) through the scenarios of spec §12.
func TestTwoMachineIntegration(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	ctx := context.Background()

	env1 := "DB_HOST=localhost\nDB_PORT=5432\n"
	cfg1 := "name: myapp\nport: 8080\nlog: info\n"

	// 1. A adds .env + config.yaml.
	a.write(".env", env1, 0o600)
	a.write("config.yaml", cfg1, 0o644)
	_, rep := a.sync(Options{Track: track(".env", "config.yaml")})
	if rep.Uploaded != 2 {
		t.Fatalf("initial upload report = %+v", rep)
	}

	// 2. B links and pulls: byte-identical files with the same modes.
	pl, rep := b.sync(Options{})
	if rep.Downloaded != 2 {
		t.Fatalf("B initial pull report = %+v (%v)", rep, describeItems(pl))
	}
	if b.read(".env") != env1 || b.read("config.yaml") != cfg1 {
		t.Fatal("B files differ from A's")
	}
	if b.mode(".env") != 0o600 || b.mode("config.yaml") != 0o644 {
		t.Fatalf("modes on B: %v %v", b.mode(".env"), b.mode("config.yaml"))
	}
	for _, m := range []*machine{a, b} {
		for _, it := range m.plan(Options{}).Items() {
			wantAction(t, it, ActionInSync)
		}
	}

	// 3. One-sided edit: A edits .env, B pulls.
	env2 := "DB_HOST=db.internal\nDB_PORT=5432\n"
	a.write(".env", env2)
	if _, rep = a.sync(Options{}); rep.Uploaded != 1 {
		t.Fatalf("A edit report = %+v", rep)
	}
	if _, rep = b.sync(Options{}); rep.Downloaded != 1 || b.read(".env") != env2 {
		t.Fatalf("B pull report = %+v content = %q", rep, b.read(".env"))
	}
	if len(b.trash()) != 0 {
		t.Fatal("unchanged pre-image trashed")
	}

	// 4. Different keys on both sides: A pushes, B merges automatically, A pulls.
	a.write(".env", "DB_HOST=db.internal\nDB_PORT=5433\n")
	b.write(".env", "DB_HOST=db.internal\nDB_PORT=5432\nDB_USER=app\n")
	if _, rep = a.sync(Options{Mode: ModePush}); rep.Uploaded != 1 {
		t.Fatalf("A push report = %+v", rep)
	}
	pl = b.plan(Options{})
	it := b.item(pl, ".env")
	wantConflict(t, it, ConflictContent, false)
	if it.Merge == nil || !it.Merge.Clean {
		t.Fatalf("merge = %+v", it.Merge)
	}
	rep = b.apply(pl, nil, Options{})
	noErrors(t, rep)
	merged := "DB_HOST=db.internal\nDB_PORT=5433\nDB_USER=app\n"
	if rep.Resolved != 1 || b.read(".env") != merged {
		t.Fatalf("B merge report = %+v content = %q", rep, b.read(".env"))
	}
	if _, rep = a.sync(Options{Mode: ModePull}); rep.Downloaded != 1 || a.read(".env") != merged {
		t.Fatalf("A pull report = %+v content = %q", rep, a.read(".env"))
	}
	if h := a.head(".env"); h.Entry == nil || len(h.Entry.Parents) != 2 {
		t.Fatalf("head after merge = %+v", h)
	}

	// 5. Same key on both sides: conflict on B, local strategy keeps B's.
	a.write(".env", "DB_HOST=db.internal\nDB_PORT=6000\nDB_USER=app\n")
	a.sync(Options{})
	bEnv := "DB_HOST=db.internal\nDB_PORT=7000\nDB_USER=app\n"
	b.write(".env", bEnv)
	pl = b.plan(Options{})
	it = b.item(pl, ".env")
	wantConflict(t, it, ConflictContent, true)
	res := Resolutions{}
	ApplyStrategy(pl, StrategyLocal, res)
	rep = b.apply(pl, res, Options{})
	noErrors(t, rep)
	if rep.Resolved != 1 || b.read(".env") != bEnv || len(rep.Unresolved) != 0 {
		t.Fatalf("B conflict report = %+v", rep)
	}
	h := b.head(".env")
	if h.Entry == nil || h.Entry.Machine != mB || len(h.Entry.Parents) != 2 {
		t.Fatalf("head after resolution = %+v", h)
	}
	pl = a.plan(Options{})
	wantAction(t, a.item(pl, ".env"), ActionDownload)
	rep = a.apply(pl, nil, Options{})
	noErrors(t, rep)
	if a.read(".env") != bEnv {
		t.Fatalf("A .env = %q", a.read(".env"))
	}

	// 6a. Concurrent offline edits of config.yaml on different lines: both
	// apply before seeing the other; the next plan merges the heads cleanly.
	a.write("config.yaml", "name: myapp2\nport: 8080\nlog: info\n")
	b.write("config.yaml", cfg1+"debug: true\n")
	pa, pb := a.plan(Options{}), b.plan(Options{})
	noErrors(t, a.apply(pa, nil, Options{}))
	noErrors(t, b.apply(pb, nil, Options{}))
	if !a.head("config.yaml").Concurrent() {
		t.Fatal("expected concurrent heads")
	}
	pl = a.plan(Options{})
	it = a.item(pl, "config.yaml")
	wantAction(t, it, ActionDownload)
	if !it.Synthetic {
		t.Fatalf("item = %+v", it)
	}
	noErrors(t, a.apply(pl, nil, Options{}))
	mergedCfg := "name: myapp2\nport: 8080\nlog: info\ndebug: true\n"
	if a.read("config.yaml") != mergedCfg {
		t.Fatalf("A config.yaml = %q", a.read("config.yaml"))
	}
	b.sync(Options{})
	if b.read("config.yaml") != mergedCfg {
		t.Fatalf("B config.yaml = %q", b.read("config.yaml"))
	}
	if h := a.head("config.yaml"); h.Concurrent() || len(h.Entry.Parents) != 2 {
		t.Fatalf("head after concurrent merge = %+v", h)
	}

	// 6b. Concurrent offline edits of the same line: ConflictConcurrent,
	// resolved with the remote strategy, converges everywhere.
	a.write("config.yaml", strings.Replace(mergedCfg, "8080", "9090", 1))
	b.write("config.yaml", strings.Replace(mergedCfg, "8080", "7070", 1))
	pa, pb = a.plan(Options{}), b.plan(Options{})
	noErrors(t, a.apply(pa, nil, Options{}))
	noErrors(t, b.apply(pb, nil, Options{}))
	pl = a.plan(Options{})
	it = a.item(pl, "config.yaml")
	wantConflict(t, it, ConflictConcurrent, true)
	if it.Merge == nil || it.Merge.Clean || len(it.Candidates) != 2 {
		t.Fatalf("item = %+v", it)
	}
	res = Resolutions{}
	ApplyStrategy(pl, StrategyRemote, res)
	rep = a.apply(pl, res, Options{})
	noErrors(t, rep)
	if rep.Resolved != 1 {
		t.Fatalf("A concurrent resolution report = %+v", rep)
	}
	// "remote" on A (the lowest machine id, so its own entry is Candidates[0])
	// must be B's side: B's 7070 wins, A's 9090 is the one discarded.
	wantCfg := strings.Replace(mergedCfg, "8080", "7070", 1)
	if a.read("config.yaml") != wantCfg {
		t.Fatalf("A config.yaml = %q, want B's version %q", a.read("config.yaml"), wantCfg)
	}
	b.sync(Options{})
	if a.read("config.yaml") != b.read("config.yaml") {
		t.Fatalf("not converged: A %q B %q", a.read("config.yaml"), b.read("config.yaml"))
	}
	if h := a.head("config.yaml"); h.Concurrent() || h.Entry.Machine != mA {
		t.Fatalf("head after resolution = %+v", h)
	}
	for _, m := range []*machine{a, b} {
		for _, it := range m.plan(Options{}).Items() {
			wantAction(t, it, ActionInSync)
		}
	}

	// 7. DeleteEverywhere on A: B's next sync trashes its copy.
	if err := a.eng.DeleteEverywhere(ctx, projID, []string{"config.yaml"}); err != nil {
		t.Fatal(err)
	}
	a.remove("config.yaml")
	_, rep = b.sync(Options{})
	if rep.Trashed != 1 || b.exists("config.yaml") {
		t.Fatalf("B delete report = %+v exists = %v", rep, b.exists("config.yaml"))
	}
	tr := b.trash()
	if len(tr) == 0 || tr[0].Path != "config.yaml" {
		t.Fatalf("trash = %+v", tr)
	}

	// 8. Untrack on A: B keeps the file, drops the base, never uploads again.
	if err := a.eng.Untrack(ctx, projID, []string{".env"}); err != nil {
		t.Fatal(err)
	}
	_, rep = b.sync(Options{})
	if rep.Untracked != 1 || !b.exists(".env") {
		t.Fatalf("B untrack report = %+v", rep)
	}
	if _, ok := b.base(".env"); ok {
		t.Fatal("B base kept after untrack")
	}
	b.write(".env", "LOCAL_ONLY=1\n")
	pl, rep = b.sync(Options{})
	noItem(t, pl, ".env")
	if rep.Uploaded != 0 {
		t.Fatalf("upload after untrack: %+v", rep)
	}
	if h := b.head(".env"); h.Entry == nil || h.Entry.Kind != vault.KindUntracked {
		t.Fatalf("head = %+v", h)
	}
	tracked, err := b.eng.TrackedPaths(projID)
	if err != nil || len(tracked) != 0 {
		t.Fatalf("tracked = %v, %v", tracked, err)
	}

	// 9. Missing locally: deleting the file on A is report only; with
	// PropagateDeletes and the local strategy a tombstone is written and B
	// trashes its copy.
	a.write("token.txt", "t0k3n\n")
	a.sync(Options{Track: track("token.txt")})
	b.sync(Options{})
	if b.read("token.txt") != "t0k3n\n" {
		t.Fatal("token not synced")
	}
	a.remove("token.txt")
	pl = a.plan(Options{})
	wantAction(t, a.item(pl, "token.txt"), ActionMissingLocal)
	before := a.head("token.txt").Entry.Clock
	rep = a.apply(pl, nil, Options{})
	noErrors(t, rep)
	if rep.Skipped != 1 || a.head("token.txt").Entry.Kind != vault.KindFile || a.head("token.txt").Entry.Clock.Compare(before) != vault.Equal {
		t.Fatalf("missing-local apply changed the vault: %+v", rep)
	}
	_, rep = a.sync(Options{PropagateDeletes: true, Strategy: StrategyLocal})
	if rep.Deleted != 1 {
		t.Fatalf("propagate report = %+v", rep)
	}
	if h := a.head("token.txt"); h.Entry == nil || h.Entry.Kind != vault.KindDeleted {
		t.Fatalf("head = %+v", h)
	}
	_, rep = b.sync(Options{})
	if rep.Trashed != 1 || b.exists("token.txt") {
		t.Fatalf("B report = %+v", rep)
	}

	// 10. Rollback: A's journal is replaced by an older copy after B synced
	// the newer version; B reports it and leaves the file alone.
	a.write("key.pem", "key v1\n")
	a.sync(Options{Track: track("key.pem")})
	b.sync(Options{})
	snapshot, err := os.ReadFile(a.journalFile())
	if err != nil {
		t.Fatal(err)
	}
	a.write("key.pem", "key v2\n")
	a.sync(Options{})
	b.sync(Options{})
	if b.read("key.pem") != "key v2\n" {
		t.Fatal("B not at v2")
	}
	if err := os.WriteFile(a.journalFile(), snapshot, 0o600); err != nil {
		t.Fatal(err)
	}
	pl = b.plan(Options{})
	it = b.item(pl, "key.pem")
	wantAction(t, it, ActionRollback)
	if !strings.Contains(it.Reason, "rolled back") {
		t.Fatalf("reason = %q", it.Reason)
	}
	rep = b.apply(pl, nil, Options{})
	noErrors(t, rep)
	if rep.Skipped != 1 || b.read("key.pem") != "key v2\n" {
		t.Fatalf("rollback apply report = %+v content = %q", rep, b.read("key.pem"))
	}
	if s := Summarize(&pl.Projects[0]); s.Rollback != 1 {
		t.Fatalf("summary = %+v", s)
	}

	// 11. Pending blob: a blob that has not arrived yet is skipped and the
	// base stays untouched.
	a.write("late.txt", "late\n")
	a.sync(Options{Track: track("late.txt")})
	blobFile := a.v.Dir() + "/" + vault.BlobPath(a.head("late.txt").Entry.Blob)
	blobData, err := os.ReadFile(blobFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blobFile); err != nil {
		t.Fatal(err)
	}
	pl = b.plan(Options{})
	wantAction(t, b.item(pl, "late.txt"), ActionPending)
	rep = b.apply(pl, nil, Options{})
	noErrors(t, rep)
	if rep.Pending != 1 || b.exists("late.txt") {
		t.Fatalf("pending report = %+v", rep)
	}
	if err := os.WriteFile(blobFile, blobData, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, rep = b.sync(Options{}); rep.Downloaded != 1 || b.read("late.txt") != "late\n" {
		t.Fatalf("late download report = %+v", rep)
	}

	// Machines are recorded with their names.
	ms, _, err := a.v.ListMachines()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 2 || ms[0].Name != "alpha" || ms[1].Name != "beta" {
		t.Fatalf("machines = %+v", ms)
	}
}
