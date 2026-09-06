package sync

import (
	"context"
	"errors"
	"io/fs"
	"strings"
	"testing"
	"time"

	"github.com/sanbiv/private-sync/internal/vault"
)

// Regression tests for the third review round: every test here pins one
// finding.

// TestJournalSeqKey pins the composite key the engine stores journal
// high-water marks under: "<projectID>/<machineID>", built by the exported
// JournalSeqKey so other packages never reconstruct it, and never a bare
// machine id.
func TestJournalSeqKey(t *testing.T) {
	t.Parallel()
	if got, want := JournalSeqKey(projID, mA), projID+"/"+mA; got != want {
		t.Fatalf("JournalSeqKey = %q, want %q", got, want)
	}
	if seqKey(projID, mA) != JournalSeqKey(projID, mA) {
		t.Fatal("seqKey and JournalSeqKey disagree")
	}
	a, b := twoMachines(t)
	a.write("f", "v1\n")
	a.sync(Options{Track: track("f")})
	b.sync(Options{})
	for _, m := range []*machine{a, b} {
		marks := m.st.JournalSeqs()
		if got := marks[JournalSeqKey(projID, mA)]; got != 1 {
			t.Fatalf("%s: mark for A = %d (marks %v), want 1", m.id[:8], got, marks)
		}
		for k := range marks {
			if !strings.HasPrefix(k, projID+"/") {
				t.Fatalf("%s: mark stored under a non-composite key %q", m.id[:8], k)
			}
		}
	}
	// Untrack writes this machine's journal (Seq 2) and records it under
	// the same key.
	if err := a.eng.Untrack(context.Background(), projID, []string{"f"}); err != nil {
		t.Fatal(err)
	}
	if got := a.st.JournalSeq(JournalSeqKey(projID, mA)); got != 2 {
		t.Fatalf("mark for A after Untrack = %d, want 2", got)
	}
	if got := a.st.JournalSeq(mA); got != 0 {
		t.Fatalf("bare machine key holds %d", got)
	}
}

// TestRow6TombstoneRollbackByClock is the row 6 companion of
// TestRollbackByClock: a single tombstone head whose clock does not dominate
// the file this machine converged to (a journal rewritten with an older
// tombstone but a fresh Seq) reports ActionRollback instead of trashing an
// unchanged local copy; AcceptRollback then applies the trash.
func TestRow6TombstoneRollbackByClock(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{"f": "v1\n"})
	a.write("f", "v2\n")
	a.sync(Options{})
	b.sync(Options{})
	if bs, ok := b.base("f"); !ok || bs.Clock[mA] != 2 {
		t.Fatalf("base on B = %+v, %v", bs, ok)
	}
	// A's journal is replaced by an older tombstone with a fresh Seq: the
	// sequence check passes, only the clock comparison can see it.
	old := vault.Entry{Path: "f", Kind: vault.KindDeleted, Clock: vault.Clock{mA: 1}, Machine: mA, UpdatedAt: time.Now()}
	j := &vault.Journal{Machine: mA, Seq: 40, Entries: map[string]vault.Entry{"f": old}}
	if err := a.v.WriteJournal(projID, j); err != nil {
		t.Fatal(err)
	}
	pl := b.plan(Options{})
	it := b.item(pl, "f")
	wantAction(t, it, ActionRollback)
	if it.Original != ActionTrashLocal {
		t.Fatalf("Original = %v, want %v", it.Original, ActionTrashLocal)
	}
	if it.NeedsResolution || strings.Contains(it.Reason, "journal of") || !strings.Contains(it.Reason, "older") {
		t.Fatalf("item = %+v", it)
	}
	rep := b.apply(pl, nil, Options{})
	noErrors(t, rep)
	if rep.Skipped != 1 || rep.Trashed != 0 || b.read("f") != "v2\n" || len(b.trash()) != 0 {
		t.Fatalf("rollback applied: report %+v", rep)
	}
	if bs, ok := b.base("f"); !ok || bs.Kind != vault.KindFile {
		t.Fatalf("base changed: %+v, %v", bs, ok)
	}
	// Push mode keeps it a rollback (never a report-only trash).
	wantAction(t, b.item(b.plan(Options{Mode: ModePush}), "f"), ActionRollback)

	// Accepting the rollback trashes the local copy and records the tombstone.
	opts := Options{AcceptRollback: true}
	pl = b.plan(opts)
	it = b.item(pl, "f")
	wantAction(t, it, ActionTrashLocal)
	if !it.AcceptedRollback || !strings.HasPrefix(it.Reason, "accepting rollback") {
		t.Fatalf("item = %+v", it)
	}
	rep = b.apply(pl, nil, opts)
	noErrors(t, rep)
	if rep.Trashed != 1 || b.exists("f") {
		t.Fatalf("trash not applied: report %+v", rep)
	}
	tr := b.trash()
	if len(tr) != 1 || tr[0].Path != "f" {
		t.Fatalf("trash = %+v", tr)
	}
	if bs, ok := b.base("f"); !ok || bs.Kind != vault.KindDeleted {
		t.Fatalf("base = %+v, %v; want tombstone", bs, ok)
	}
	noItem(t, b.plan(Options{}), "f")
}

// TestModeDiffersOn covers the mode comparison used by restore and by
// download's mode-only reconciliation, including the Windows branch that the
// Unix test run cannot reach through the file system: there only the
// owner-write bit is meaningful, so a Unix-recorded 0600 must not re-plan a
// phantom download against a 0666 that Windows reports for every writable
// file.
func TestModeDiffersOn(t *testing.T) {
	t.Parallel()
	perm := uint32(fs.ModePerm)
	tests := []struct {
		name        string
		goos        string
		local, head uint32
		want        bool
	}{
		{"unix equal", "linux", 0o600, 0o600, false},
		{"unix differs", "linux", 0o644, 0o600, true},
		{"unix exec bit", "darwin", 0o600, 0o700, true},
		{"unix head unrecorded", "linux", 0o644, 0, false},
		{"unix ignores non-permission bits", "linux", 0o600 | ^perm, 0o600, false},
		{"unix head with non-permission bits", "linux", 0o600, 0o600 | ^perm, false},
		{"windows writable vs 0600", "windows", 0o666, 0o600, false},
		{"windows writable vs 0644", "windows", 0o666, 0o644, false},
		{"windows writable vs 0755", "windows", 0o666, 0o755, false},
		{"windows read-only vs 0600", "windows", 0o444, 0o600, true},
		{"windows writable vs read-only head", "windows", 0o666, 0o444, true},
		{"windows read-only vs 0400", "windows", 0o444, 0o400, false},
		{"windows head unrecorded", "windows", 0o444, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := modeDiffersOn(tc.goos, tc.local, tc.head); got != tc.want {
				t.Fatalf("modeDiffersOn(%s, %04o, %04o) = %v, want %v", tc.goos, tc.local, tc.head, got, tc.want)
			}
		})
	}
	// modeDiffers is the current-GOOS view; on the Unix test hosts it is the
	// full comparison.
	if modeDiffers(0o600, 0o600) || modeDiffers(0o644, 0) {
		t.Fatal("modeDiffers reports a difference for equal or unrecorded modes")
	}
}

// TestApplyCancelledBetweenItems pins the §9.3 guarantee: a context cancelled
// between two items leaves the blob of the applied item in the vault but no
// journal, no base and no report count, and the next Plan/Apply with a live
// context finishes the work reusing that blob.
func TestApplyCancelledBetweenItems(t *testing.T) {
	t.Parallel()
	a, _ := twoMachines(t)
	a.write("f1", "one\n")
	a.write("f2", "two\n")
	opts := Options{Track: track("f1", "f2")}
	pl := a.plan(opts)
	wantAction(t, a.item(pl, "f1"), ActionUpload)
	wantAction(t, a.item(pl, "f2"), ActionUpload)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	perItem := 0
	opts.Progress = func(ev Event) {
		if ev.Stage == "apply" && ev.Path != "" && ev.Err == nil {
			perItem++
			if perItem == 1 {
				cancel() // after the first item started: it completes, the second must not
			}
		}
	}
	rep, err := a.eng.Apply(ctx, pl, nil, opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply err = %v, want context.Canceled", err)
	}
	if perItem != 1 {
		t.Fatalf("%d items started after cancellation, want 1", perItem)
	}
	if rep == nil || rep.Uploaded != 0 || len(rep.Errors) != 0 {
		t.Fatalf("report counts uncommitted work: %+v", rep)
	}
	if hs := a.heads(); len(hs) != 0 {
		t.Fatalf("journal written after cancellation: %v", hs)
	}
	for _, p := range []string{"f1", "f2"} {
		if _, ok := a.base(p); ok {
			t.Fatalf("base of %s set after cancellation", p)
		}
	}
	blob1, blob2 := a.v.BlobID([]byte("one\n")), a.v.BlobID([]byte("two\n"))
	if !a.v.HasBlob(blob1) {
		t.Fatal("blob of the item applied before cancellation is missing")
	}
	if a.v.HasBlob(blob2) {
		t.Fatal("second item applied after cancellation")
	}

	// Recovery: a fresh plan still uploads both, reusing the first blob.
	opts = Options{Track: track("f1", "f2")}
	pl = a.plan(opts)
	wantAction(t, a.item(pl, "f1"), ActionUpload)
	wantAction(t, a.item(pl, "f2"), ActionUpload)
	rep = a.apply(pl, nil, opts)
	noErrors(t, rep)
	if rep.Uploaded != 2 {
		t.Fatalf("report = %+v", rep)
	}
	hs := a.heads()
	if h := hs["f1"]; h.Entry == nil || h.Entry.Blob != blob1 || h.Entry.Clock[mA] != 1 {
		t.Fatalf("head f1 = %+v", h)
	}
	if h := hs["f2"]; h.Entry == nil || h.Entry.Blob != blob2 {
		t.Fatalf("head f2 = %+v", h)
	}
	for p, blob := range map[string]string{"f1": blob1, "f2": blob2} {
		if bs, ok := a.base(p); !ok || bs.Blob != blob {
			t.Fatalf("base of %s = %+v, %v", p, bs, ok)
		}
	}
	j, err := a.v.ReadJournal(projID, mA)
	if err != nil || j == nil || j.Seq != 1 {
		t.Fatalf("journal = %+v, %v; want a single write (Seq 1)", j, err)
	}
}
