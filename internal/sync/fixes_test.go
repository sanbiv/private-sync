package sync

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/config"
	"github.com/sanbiv/private-sync/internal/identity"
	"github.com/sanbiv/private-sync/internal/vault"
)

// Regression tests for the second review round: every test here pins one
// finding.

const projID2 = "fedcba9876543210"

// addProject links a second project on a machine and returns its directory.
func (m *machine) addProject(id, name string) string {
	m.t.Helper()
	dir := filepath.Join(m.t.TempDir(), "project-"+name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		m.t.Fatal(err)
	}
	m.cfg.Projects = append(m.cfg.Projects, config.ProjectConfig{ID: id, Name: name, Path: dir})
	return dir
}

func writeIn(t *testing.T, dir, p, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readIn(t *testing.T, dir, p string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(p)))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func only(id string) []string { return []string{id} }

// TestJournalSeqScopedPerProject pins the high finding: journals and their
// Seq are per (project, machine), so the high-water mark must be too. With
// the mark keyed by machine alone, the seq of one project poisoned the
// rollback check of every other project linked on the same machine.
func TestJournalSeqScopedPerProject(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	dirA2 := a.addProject(projID2, "second")
	dirB2 := b.addProject(projID2, "second")
	g := ItemKey{Project: projID2, Path: "g"}

	// Project 1: four journal writes on A (seq 4).
	a.write("f", "v1\n")
	a.sync(Options{Projects: only(projID), Track: track("f")})
	for _, c := range []string{"v2\n", "v3\n", "v4\n"} {
		a.write("f", c)
		a.sync(Options{Projects: only(projID)})
	}
	// Project 2: one journal write on A (seq 1).
	writeIn(t, dirA2, "g", "g1\n")
	a.sync(Options{Projects: only(projID2), Track: map[ItemKey]bool{g: true}})
	j1, err := a.v.ReadJournal(projID, mA)
	if err != nil || j1 == nil {
		t.Fatalf("project 1 journal: %+v, %v", j1, err)
	}
	j2, err := a.v.ReadJournal(projID2, mA)
	if err != nil || j2 == nil {
		t.Fatalf("project 2 journal: %+v, %v", j2, err)
	}
	if j1.Seq != 4 || j2.Seq != 1 {
		t.Fatalf("seqs = %d, %d; want 4, 1", j1.Seq, j2.Seq)
	}

	// B syncs project 1 first (mark 4 for A there), then plans project 2.
	_, rep := b.sync(Options{Projects: only(projID)})
	if rep.Downloaded != 1 || b.read("f") != "v4\n" {
		t.Fatalf("B project 1 report = %+v", rep)
	}
	if got := b.st.JournalSeq(seqKey(projID, mA)); got != 4 {
		t.Fatalf("mark for A in project 1 = %d, want 4", got)
	}
	pl := b.plan(Options{Projects: only(projID2)})
	if len(pl.Projects) != 1 || pl.Projects[0].ID != projID2 {
		t.Fatalf("plan projects = %+v", pl.Projects)
	}
	it := pl.Find(g)
	if it == nil {
		t.Fatalf("no item for g: %v", describeItems(pl))
	}
	wantAction(t, it, ActionDownload)
	if strings.Contains(it.Reason, "rolled back") || hasWarning(pl.Projects[0].Warnings, "rolled back") {
		t.Fatalf("project 2 tripped project 1's mark: reason %q warnings %v", it.Reason, pl.Projects[0].Warnings)
	}
	rep = b.apply(pl, nil, Options{Projects: only(projID2)})
	noErrors(t, rep)
	if rep.Downloaded != 1 || readIn(t, dirB2, "g") != "g1\n" {
		t.Fatalf("B project 2 report = %+v", rep)
	}
	if got := b.st.JournalSeq(seqKey(projID2, mA)); got != 1 {
		t.Fatalf("mark for A in project 2 = %d, want 1", got)
	}
	if got := b.st.JournalSeq(seqKey(projID, mA)); got != 4 {
		t.Fatalf("mark for A in project 1 changed to %d", got)
	}
	// Neither project trips afterwards.
	pl = b.plan(Options{})
	if len(pl.Projects) != 2 {
		t.Fatalf("projects planned = %d", len(pl.Projects))
	}
	for _, pp := range pl.Projects {
		if hasWarning(pp.Warnings, "rolled back") {
			t.Fatalf("%s: warnings = %v", pp.ID, pp.Warnings)
		}
		for i := range pp.Items {
			wantAction(t, &pp.Items[i], ActionInSync)
		}
	}

	// A real rollback of project 2 is still detected, and project 1 stays
	// untouched by it.
	journal2 := filepath.Join(a.v.Dir(), "projects", projID2, "state", mA+".json.enc")
	old, err := os.ReadFile(journal2)
	if err != nil {
		t.Fatal(err)
	}
	writeIn(t, dirA2, "g", "g2\n")
	a.sync(Options{Projects: only(projID2)})
	if _, rep = b.sync(Options{Projects: only(projID2)}); rep.Downloaded != 1 {
		t.Fatalf("B project 2 update report = %+v", rep)
	}
	if err := os.WriteFile(journal2, old, 0o600); err != nil {
		t.Fatal(err)
	}
	pl = b.plan(Options{})
	for _, pp := range pl.Projects {
		switch pp.ID {
		case projID2:
			it := pl.Find(g)
			if it == nil {
				t.Fatalf("no item for g: %v", describeItems(pl))
			}
			wantAction(t, it, ActionRollback)
			if !hasWarning(pp.Warnings, "rolled back") {
				t.Fatalf("project 2 warnings = %v", pp.Warnings)
			}
		case projID:
			if hasWarning(pp.Warnings, "rolled back") {
				t.Fatalf("project 1 warnings = %v", pp.Warnings)
			}
			wantAction(t, b.item(pl, "f"), ActionInSync)
		}
	}
}

// TestTombstoneRefusedOnUntrackedPath pins the low finding on
// writeTombstones: a path whose head is already a tombstone is not tracked,
// so files delete after files rm (and vice versa) is refused instead of
// writing a KindDeleted entry that turns every local copy the untrack left
// in place into a modify/delete conflict.
func TestTombstoneRefusedOnUntrackedPath(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	ctx := context.Background()
	seed(a, b, map[string]string{"f": "v1\n", "g": "v1\n", "h": "v1\n"})

	// Untrack, then DeleteEverywhere (and Untrack again) on the same machine.
	if err := a.eng.Untrack(ctx, projID, []string{"f"}); err != nil {
		t.Fatal(err)
	}
	if err := a.eng.DeleteEverywhere(ctx, projID, []string{"f"}); !errors.Is(err, ErrNotTracked) {
		t.Fatalf("DeleteEverywhere after Untrack: err = %v, want ErrNotTracked", err)
	}
	if err := a.eng.Untrack(ctx, projID, []string{"f"}); !errors.Is(err, ErrNotTracked) {
		t.Fatalf("Untrack twice: err = %v, want ErrNotTracked", err)
	}
	if h := a.head("f"); h.Entry == nil || h.Entry.Kind != vault.KindUntracked {
		t.Fatalf("head = %+v, want the untracked entry untouched", h)
	}
	// B syncs the untrack (base dropped, file kept): it cannot tombstone the
	// path either, and its lingering copy stays out of every later plan.
	if _, rep := b.sync(Options{}); rep.Untracked != 1 || !b.exists("f") {
		t.Fatalf("B untrack report = %+v", rep)
	}
	if err := b.eng.DeleteEverywhere(ctx, projID, []string{"f"}); !errors.Is(err, ErrNotTracked) {
		t.Fatalf("DeleteEverywhere on B: err = %v, want ErrNotTracked", err)
	}
	b.write("f", "edited\n")
	noItem(t, b.plan(Options{}), "f")
	noItem(t, a.plan(Options{}), "f")

	// A deleted head with a tombstone base (already deleted here) is not
	// tracked either; a machine that has not synced the deletion yet (file
	// base) may still write its own tombstone, which is harmless.
	if err := a.eng.DeleteEverywhere(ctx, projID, []string{"g"}); err != nil {
		t.Fatal(err)
	}
	a.remove("g")
	if err := a.eng.DeleteEverywhere(ctx, projID, []string{"g"}); !errors.Is(err, ErrNotTracked) {
		t.Fatalf("DeleteEverywhere twice: err = %v, want ErrNotTracked", err)
	}
	if err := a.eng.Untrack(ctx, projID, []string{"g"}); !errors.Is(err, ErrNotTracked) {
		t.Fatalf("Untrack of a deleted path: err = %v, want ErrNotTracked", err)
	}
	if err := b.eng.Untrack(ctx, projID, []string{"g"}); err != nil {
		t.Fatalf("Untrack on the machine that has not seen the deletion: %v", err)
	}
	if h := b.head("g"); h.Entry == nil || h.Entry.Kind != vault.KindUntracked || h.Entry.Machine != mB {
		t.Fatalf("head = %+v, want B's untracked entry dominating the tombstone", h)
	}
	if _, rep := b.sync(Options{}); rep.Uploaded != 0 || !b.exists("g") {
		t.Fatalf("B report = %+v", rep)
	}
	if _, rep := a.sync(Options{}); rep.Untracked != 1 {
		t.Fatalf("A report = %+v", rep)
	}
	if _, ok := a.base("g"); ok {
		t.Fatal("A base kept")
	}

	// Nothing is written when any path of a list is refused.
	if err := a.eng.Untrack(ctx, projID, []string{"h", "f"}); !errors.Is(err, ErrNotTracked) {
		t.Fatalf("mixed list: err = %v, want ErrNotTracked", err)
	}
	if h := a.head("h"); h.Entry == nil || h.Entry.Kind != vault.KindFile {
		t.Fatalf("head of h = %+v, want untouched", h)
	}
	tracked, err := a.eng.TrackedPaths(projID)
	if err != nil || len(tracked) != 1 || !tracked["h"] {
		t.Fatalf("tracked = %v, %v", tracked, err)
	}
}

// TestRow5ConvergeCountsAsInSync pins the spec deviation on row 5: the
// converge onto a tombstone still exists as an item (Apply records the base)
// but status must not show activity for a file already gone on both sides.
func TestRow5ConvergeCountsAsInSync(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{"f": "v1\n", "g": "v1\n"})
	if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"f"}); err != nil {
		t.Fatal(err)
	}
	b.remove("f")
	pl := b.plan(Options{})
	it := b.item(pl, "f")
	wantAction(t, it, ActionConverge)
	if it.Head == nil || it.Head.Kind != vault.KindDeleted {
		t.Fatalf("head = %+v", it.Head)
	}
	wantAction(t, b.item(pl, "g"), ActionInSync)
	if s := Summarize(&pl.Projects[0]); s != (Summary{InSync: 2}) {
		t.Fatalf("summary = %+v, want both in sync", s)
	}
	rep := b.apply(pl, nil, Options{})
	noErrors(t, rep)
	if rep.Converged != 1 {
		t.Fatalf("report = %+v", rep)
	}
	if bs, ok := b.base("f"); !ok || bs.Kind != vault.KindDeleted {
		t.Fatalf("base = %+v", bs)
	}
	noItem(t, b.plan(Options{}), "f")
	// A converge of a live file is still a remote change.
	b.write("g", "v2\n")
	b.sync(Options{})
	a.write("g", "v2\n")
	pl = a.plan(Options{})
	wantAction(t, a.item(pl, "g"), ActionConverge)
	if s := Summarize(&pl.Projects[0]); s.RemoteChanges != 1 {
		t.Fatalf("summary = %+v", s)
	}
}

// TestApplyWritesProjectMeta pins the §9.3 "meta (if changed)" write: with
// the Options.Meta hook Apply refreshes this machine's ProjectMeta after the
// journal, and only when the description changed.
func TestApplyWritesProjectMeta(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	fp := identity.Fingerprint{Kind: "git", Value: "github.com/me/app", Level: identity.LevelStrong}
	weak := identity.Fingerprint{Kind: "dir", Value: "app", Level: identity.LevelDir}
	describe := func(fps ...identity.Fingerprint) func(string) (vault.ProjectMeta, bool) {
		return func(id string) (vault.ProjectMeta, bool) {
			if id != projID {
				t.Errorf("hook called for %s", id)
			}
			return vault.ProjectMeta{Name: "myapp", Fingerprints: fps}, true
		}
	}
	metaFile := filepath.Join(a.v.Dir(), "projects", projID, "meta", mA+".json.enc")

	a.write("f", "v1\n")
	opts := Options{Track: track("f"), Meta: describe(fp)}
	pl := a.plan(opts)
	noErrors(t, a.apply(pl, nil, opts))
	meta, err := a.v.ReadProjectMeta(projID, mA)
	if err != nil || meta == nil {
		t.Fatalf("ReadProjectMeta = %+v, %v", meta, err)
	}
	if meta.Name != "myapp" || len(meta.Fingerprints) != 1 || meta.Fingerprints[0] != fp || meta.CreatedAt.IsZero() {
		t.Fatalf("meta = %+v", meta)
	}
	created := meta.CreatedAt
	first, err := os.ReadFile(metaFile)
	if err != nil {
		t.Fatal(err)
	}

	// The same description is not rewritten (the stored creation time is kept
	// even though the hook supplies none).
	opts = Options{Meta: describe(fp)}
	noErrors(t, a.apply(a.plan(opts), nil, opts))
	same, err := os.ReadFile(metaFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, same) {
		t.Fatal("unchanged meta was rewritten")
	}
	// A changed description is written, keeping the creation time.
	opts = Options{Meta: describe(fp, weak)}
	noErrors(t, a.apply(a.plan(opts), nil, opts))
	meta, err = a.v.ReadProjectMeta(projID, mA)
	if err != nil || meta == nil || len(meta.Fingerprints) != 2 || !meta.CreatedAt.Equal(created) {
		t.Fatalf("meta after change = %+v, %v", meta, err)
	}
	changed, err := os.ReadFile(metaFile)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first, changed) {
		t.Fatal("changed meta not rewritten")
	}
	// A declining hook, and no hook at all, write nothing: B never gets a
	// meta file through Apply on its own.
	decline := func(string) (vault.ProjectMeta, bool) { return vault.ProjectMeta{Name: "other"}, false }
	opts = Options{Meta: decline}
	noErrors(t, b.apply(b.plan(opts), nil, opts))
	b.sync(Options{})
	if m, err := b.v.ReadProjectMeta(projID, mB); err != nil || m != nil {
		t.Fatalf("B meta = %+v, %v; want none", m, err)
	}
	// The merged project view carries the description.
	p, _, err := a.v.ReadProject(projID)
	if err != nil || p == nil || p.Name != "myapp" || len(p.Fingerprints) != 2 {
		t.Fatalf("ReadProject = %+v, %v", p, err)
	}
	// Missing projects are not described.
	a.cfg.Projects[0].Path = filepath.Join(t.TempDir(), "gone")
	calls := 0
	opts = Options{Meta: func(string) (vault.ProjectMeta, bool) { calls++; return vault.ProjectMeta{}, true }}
	pl = a.plan(opts)
	if !pl.Projects[0].Missing {
		t.Fatal("project not missing")
	}
	noErrors(t, a.apply(pl, nil, opts))
	if calls != 0 {
		t.Fatalf("hook called %d times for a missing project", calls)
	}
}

// TestWarningHandler pins the low finding on discarded ReadJournals
// warnings: Untrack, DeleteEverywhere and TrackedPaths forward them to the
// handler installed with SetWarningHandler, while Plan keeps reporting them
// in ProjectPlan.Warnings.
func TestWarningHandler(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	ctx := context.Background()
	seed(a, b, map[string]string{"f": "v1\n", "g": "v1\n"})
	stray := filepath.Join(a.v.Dir(), "projects", projID, "state", mA+" (1).json.enc")
	if err := os.WriteFile(stray, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	var got []string
	a.eng.SetWarningHandler(func(id, w string) {
		if id != projID {
			t.Errorf("warning for project %s", id)
		}
		got = append(got, w)
	})
	check := func(t *testing.T, what string) {
		t.Helper()
		if len(got) != 1 || !strings.Contains(got[0], "(1)") || !strings.Contains(got[0], "skipped") {
			t.Fatalf("%s: warnings = %v, want the stray journal named once", what, got)
		}
		got = nil
	}
	if err := a.eng.Untrack(ctx, projID, []string{"f"}); err != nil {
		t.Fatal(err)
	}
	check(t, "Untrack")
	if err := a.eng.DeleteEverywhere(ctx, projID, []string{"g"}); err != nil {
		t.Fatal(err)
	}
	check(t, "DeleteEverywhere")
	// Forwarded even when the operation is then refused.
	if err := a.eng.DeleteEverywhere(ctx, projID, []string{"f"}); !errors.Is(err, ErrNotTracked) {
		t.Fatalf("err = %v", err)
	}
	check(t, "refused DeleteEverywhere")
	if _, err := a.eng.TrackedPaths(projID); err != nil {
		t.Fatal(err)
	}
	check(t, "TrackedPaths")
	// Plan reports it by itself.
	pl := a.plan(Options{})
	if !hasWarning(pl.Projects[0].Warnings, "(1)") {
		t.Fatalf("plan warnings = %v", pl.Projects[0].Warnings)
	}
	// Removing the handler, and a nil engine, are harmless.
	a.eng.SetWarningHandler(nil)
	if _, err := a.eng.TrackedPaths(projID); err != nil || len(got) != 0 {
		t.Fatalf("after removing the handler: %v, %v", got, err)
	}
	var nilEngine *Engine
	nilEngine.SetWarningHandler(func(string, string) {})
	nilEngine.warnAll(projID, []string{"x"})
}
