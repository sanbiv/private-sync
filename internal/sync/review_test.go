package sync

import (
	"context"
	"errors"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/remote"
	"github.com/sanbiv/private-sync/internal/vault"
)

// Regression tests for the review findings: every test here pins one bug.

// diverge makes a and b publish different versions of p without seeing each
// other's entry (both plan, then both apply), leaving concurrent vault heads.
func diverge(t *testing.T, a, b *machine, p, contentA, contentB string) {
	t.Helper()
	a.write(p, contentA)
	b.write(p, contentB)
	pa, pb := a.plan(Options{}), b.plan(Options{})
	noErrors(t, a.apply(pa, nil, Options{}))
	noErrors(t, b.apply(pb, nil, Options{}))
	if !a.head(p).Concurrent() {
		t.Fatalf("%s: expected concurrent heads", p)
	}
}

// key returns the item key of a test-project path.
func key(p string) ItemKey { return ItemKey{Project: projID, Path: p} }

// TestConcurrentRemoteIsTheOtherMachine pins the high finding: for a
// ConflictConcurrent item "remote" must be the OTHER machine's candidate,
// whatever the candidate order (machine-id order puts the lowest id first,
// so on that machine Candidates[0] is its own entry).
func TestConcurrentRemoteIsTheOtherMachine(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		path     string // one path per case: the cases share a machine pair
		resolver func(a, b *machine) (me, other *machine)
		strategy Strategy
		winner   func(me, other *machine) *machine
	}{
		{"remote on the lowest id keeps the other side", "remote-low.yaml", func(a, b *machine) (*machine, *machine) { return a, b }, StrategyRemote, func(me, other *machine) *machine { return other }},
		{"remote on the highest id keeps the other side", "remote-high.yaml", func(a, b *machine) (*machine, *machine) { return b, a }, StrategyRemote, func(me, other *machine) *machine { return other }},
		{"local on the lowest id keeps its own side", "local-low.yaml", func(a, b *machine) (*machine, *machine) { return a, b }, StrategyLocal, func(me, other *machine) *machine { return me }},
		{"local on the highest id keeps its own side", "local-high.yaml", func(a, b *machine) (*machine, *machine) { return b, a }, StrategyLocal, func(me, other *machine) *machine { return me }},
	}
	// All paths are seeded and diverged in one round (fsync-bound: fewer
	// applies); each case then resolves only its own path.
	a, b := twoMachines(t)
	files := map[string]string{}
	for _, tc := range tests {
		files[tc.path] = "port: 8080\n"
	}
	seed(a, b, files)
	for _, tc := range tests {
		a.write(tc.path, "port: 9090\n")
		b.write(tc.path, "port: 7070\n")
	}
	pa, pb := a.plan(Options{}), b.plan(Options{})
	noErrors(t, a.apply(pa, nil, Options{}))
	noErrors(t, b.apply(pb, nil, Options{}))
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := tc.path
			if !a.head(p).Concurrent() {
				t.Fatalf("%s: expected concurrent heads", p)
			}
			me, other := tc.resolver(a, b)
			mine, theirs := me.read(p), other.read(p)

			pl := me.plan(Options{})
			it := me.item(pl, p)
			wantConflict(t, it, ConflictConcurrent, true)
			if len(it.Candidates) != 2 || it.Head != nil {
				t.Fatalf("candidates = %d head = %+v", len(it.Candidates), it.Head)
			}
			if string(it.HeadText) != theirs || string(it.LocalText) != mine {
				t.Fatalf("HeadText = %q LocalText = %q, want %q / %q", it.HeadText, it.LocalText, theirs, mine)
			}
			if it.Remote == nil || it.Remote.Machine != other.id || it.Remote.Kind != vault.KindFile {
				t.Fatalf("Remote = %+v, want %s's file", it.Remote, other.id[:8])
			}
			if !strings.Contains(it.Reason, "remote = "+other.id[:8]) {
				t.Fatalf("reason = %q", it.Reason)
			}

			all := Resolutions{}
			ApplyStrategy(pl, tc.strategy, all)
			r, ok := all[it.Key]
			if !ok {
				t.Fatalf("strategy left %s unresolved", p)
			}
			rep := me.apply(pl, Resolutions{it.Key: r}, Options{}) // the other cases' paths stay unresolved
			noErrors(t, rep)
			if rep.Resolved != 1 || rep.Uploaded != 1 {
				t.Fatalf("report = %+v", rep)
			}
			want := tc.winner(me, other)
			wantContent := mine
			if want == other {
				wantContent = theirs
			}
			if got := me.read(p); got != wantContent {
				t.Fatalf("%s: content = %q, want %q", me.id[:8], got, wantContent)
			}
			h := me.head(p)
			if h.Entry == nil || h.Entry.Machine != me.id || len(h.Entry.Parents) != 2 {
				t.Fatalf("head after resolution = %+v", h.Entry)
			}
			_, rep = other.sync(Options{})
			if got := other.read(p); got != wantContent {
				t.Fatalf("%s: content = %q, want %q", other.id[:8], got, wantContent)
			}
			if want == other && rep.Downloaded != 0 {
				t.Fatalf("the winner's file was rewritten: %+v", rep)
			}
			for _, m := range []*machine{a, b} {
				wantAction(t, m.item(m.plan(Options{}), p), ActionInSync)
			}
		})
	}
}

// TestConcurrentRemoteDeleted: when the other machine's candidate is a
// tombstone, "remote" means the deletion — both through StrategyRemote and an
// explicit ChooseRemote — and the resolution is recorded so the vault head
// stops being concurrent.
func TestConcurrentRemoteDeleted(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	for _, explicit := range []bool{false, true} {
		name, p := "strategy remote", "f"
		if explicit {
			name, p = "explicit ChooseRemote", "g"
		}
		t.Run(name, func(t *testing.T) {
			seed(a, b, map[string]string{p: "v1\n"})
			b.write(p, "v2\n")
			stale := b.plan(Options{})
			if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{p}); err != nil {
				t.Fatal(err)
			}
			noErrors(t, b.apply(stale, nil, Options{}))

			pl := b.plan(Options{})
			it := b.item(pl, p)
			wantConflict(t, it, ConflictConcurrent, true)
			if it.Remote == nil || it.Remote.Kind != vault.KindDeleted || it.Remote.Machine != mA {
				t.Fatalf("Remote = %+v, want A's tombstone", it.Remote)
			}
			if !strings.Contains(it.Reason, "deleted on "+mA[:8]) {
				t.Fatalf("reason = %q", it.Reason)
			}
			res := Resolutions{}
			if explicit {
				res[it.Key] = Resolution{Kind: ChooseRemote}
			} else {
				ApplyStrategy(pl, StrategyRemote, res)
				if res[it.Key].Kind != ChooseDelete {
					t.Fatalf("res = %+v, want delete", res[it.Key])
				}
			}
			rep := b.apply(pl, res, Options{})
			noErrors(t, rep)
			if rep.Resolved != 1 || rep.Trashed != 1 || rep.Deleted != 1 || b.exists(p) {
				t.Fatalf("report = %+v exists = %v", rep, b.exists(p))
			}
			var trashed bool
			for _, tr := range b.trash() {
				trashed = trashed || tr.Path == p
			}
			if !trashed {
				t.Fatalf("%s not in the trash: %+v", p, b.trash())
			}
			h := b.head(p)
			if h.Entry == nil || h.Entry.Kind != vault.KindDeleted || h.Entry.Machine != mB || len(h.Entry.Parents) != 1 {
				t.Fatalf("head = %+v, want B's tombstone with B's blob as parent", h.Entry)
			}
			if bs, ok := b.base(p); !ok || bs.Kind != vault.KindDeleted || bs.Clock.Compare(h.Entry.Clock) != vault.Equal {
				t.Fatalf("base = %+v", bs)
			}
			// A still holds its (unchanged) copy with a tombstone base: row 9.
			pl = a.plan(Options{})
			it = a.item(pl, p)
			wantAction(t, it, ActionReportOnly)
			if it.Original != ActionMissingLocal {
				t.Fatalf("item = %+v", it)
			}
			// The vault head is no longer concurrent: nothing left to resolve on B.
			noItem(t, b.plan(Options{}), p)
			a.remove(p)
		})
	}
}

// TestConcurrentAmbiguousRemote: a third machine seeing two other machines
// disagree has no single "remote" side; StrategyRemote leaves the item
// unresolved and StrategyLocal keeps its own copy (uploaded as the merge).
func TestConcurrentAmbiguousRemote(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	c := openMachine(t, a.v.Dir(), mC, "gamma")
	seed(a, b, map[string]string{"f": "v1\n"})
	c.sync(Options{})
	if c.read("f") != "v1\n" {
		t.Fatal("C not seeded")
	}
	diverge(t, a, b, "f", "va\n", "vb\n")

	pl := c.plan(Options{})
	it := c.item(pl, "f")
	wantConflict(t, it, ConflictConcurrent, true)
	if it.Remote != nil || !strings.Contains(it.Reason, "no single remote side") {
		t.Fatalf("Remote = %+v reason = %q", it.Remote, it.Reason)
	}
	if string(it.HeadText) != "va\n" || string(it.LocalText) != "v1\n" || string(it.BaseText) != "v1\n" {
		t.Fatalf("texts = %q %q %q", it.HeadText, it.LocalText, it.BaseText)
	}
	res := Resolutions{}
	ApplyStrategy(pl, StrategyRemote, res)
	if _, ok := res[it.Key]; ok {
		t.Fatalf("remote strategy picked a side: %+v", res)
	}
	rep := c.apply(pl, res, Options{})
	noErrors(t, rep)
	if len(rep.Unresolved) != 1 || rep.Skipped != 1 || c.read("f") != "v1\n" {
		t.Fatalf("report = %+v", rep)
	}
	// An explicit ChooseRemote still takes HeadText (the first foreign version).
	pl = c.plan(Options{})
	noErrors(t, c.apply(pl, Resolutions{key("f"): {Kind: ChooseRemote}}, Options{}))
	if c.read("f") != "va\n" {
		t.Fatalf("C f = %q", c.read("f"))
	}
	h := c.head("f")
	if h.Entry == nil || h.Entry.Machine != mC || len(h.Entry.Parents) != 3 {
		t.Fatalf("head = %+v, want C's merge of A, B and the base", h.Entry)
	}
	for _, m := range []*machine{a, b} {
		m.sync(Options{})
		if m.read("f") != "va\n" {
			t.Fatalf("%s f = %q", m.id[:8], m.read("f"))
		}
	}
}

// TestConcurrentWithoutLocalStrategyLocal: "local" has nothing to keep when
// the file is absent here; the item stays unresolved instead of erroring.
func TestConcurrentWithoutLocalStrategyLocal(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{"f": "v1\n"})
	diverge(t, a, b, "f", "va\n", "vb\n")
	a.remove("f")

	pl := a.plan(Options{})
	it := a.item(pl, "f")
	wantConflict(t, it, ConflictConcurrent, true)
	if it.Local != nil || it.LocalText != nil {
		t.Fatalf("local = %+v", it.Local)
	}
	res := Resolutions{}
	ApplyStrategy(pl, StrategyLocal, res)
	if _, ok := res[it.Key]; ok {
		t.Fatalf("local strategy without a local file picked %+v", res[it.Key])
	}
	rep := a.apply(pl, res, Options{})
	noErrors(t, rep)
	if len(rep.Unresolved) != 1 || rep.Skipped != 1 || a.exists("f") {
		t.Fatalf("report = %+v", rep)
	}
	// Remote still works: B's version is written.
	pl = a.plan(Options{})
	res = Resolutions{}
	ApplyStrategy(pl, StrategyRemote, res)
	noErrors(t, a.apply(pl, res, Options{}))
	if a.read("f") != "vb\n" {
		t.Fatalf("A f = %q", a.read("f"))
	}
}

// TestPremergeBaseBlobMissingIsPending pins the medium finding: an
// unreadable merge base makes the item pending (spec §9.1), not a "no common
// base" conflict that a strategy would then resolve wrongly.
func TestPremergeBaseBlobMissingIsPending(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{"notes.txt": "l1\nl2\nl3\n"})
	diverge(t, a, b, "notes.txt", "L1\nl2\nl3\n", "l1\nl2\nL3\n")
	h := a.head("notes.txt")
	if h.Base == "" {
		t.Fatalf("no common base: %+v", h)
	}
	blobFile := a.v.Dir() + "/" + vault.BlobPath(h.Base)
	data, err := os.ReadFile(blobFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(blobFile); err != nil {
		t.Fatal(err)
	}

	pl := a.plan(Options{})
	it := a.item(pl, "notes.txt")
	wantAction(t, it, ActionPending)
	if it.NeedsResolution || it.Merge != nil || !strings.Contains(it.Reason, shortID(h.Base)) {
		t.Fatalf("item = %+v", it)
	}
	res := Resolutions{}
	ApplyStrategy(pl, StrategyRemote, res)
	if len(res) != 0 {
		t.Fatalf("strategy resolved a pending item: %+v", res)
	}
	before, _ := a.base("notes.txt")
	rep := a.apply(pl, res, Options{})
	noErrors(t, rep)
	after, _ := a.base("notes.txt")
	if rep.Pending != 1 || a.read("notes.txt") != "L1\nl2\nl3\n" || after.Blob != before.Blob {
		t.Fatalf("report = %+v", rep)
	}
	if !a.head("notes.txt").Concurrent() {
		t.Fatal("pending item wrote an entry")
	}

	// Once the base arrives the pair merges cleanly.
	if err := os.WriteFile(blobFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	pl = a.plan(Options{})
	it = a.item(pl, "notes.txt")
	wantAction(t, it, ActionDownload)
	if !it.Synthetic {
		t.Fatalf("item = %+v", it)
	}
	noErrors(t, a.apply(pl, nil, Options{}))
	if a.read("notes.txt") != "L1\nl2\nL3\n" {
		t.Fatalf("A notes.txt = %q", a.read("notes.txt"))
	}
}

// TestRestoreReconcilesMode: same bytes but a different mode is a download
// in restore mode, and download() then only fixes the mode (no trash entry).
func TestRestoreReconcilesMode(t *testing.T) {
	t.Parallel()
	_, v := newVaultDir(t)
	a := newMachine(t, mA, "alpha", v)
	a.write("key.pem", "secret\n", 0o600)
	a.sync(Options{Track: track("key.pem")})
	if err := os.Chmod(a.abs("key.pem"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A plain sync compares content only.
	wantAction(t, a.item(a.plan(Options{}), "key.pem"), ActionInSync)

	pl := a.plan(Options{Mode: ModeRestore})
	it := a.item(pl, "key.pem")
	wantAction(t, it, ActionDownload)
	if !strings.Contains(it.Reason, "mode") {
		t.Fatalf("reason = %q", it.Reason)
	}
	rep := a.apply(pl, nil, Options{Mode: ModeRestore})
	noErrors(t, rep)
	if rep.Downloaded != 1 || a.mode("key.pem") != 0o600 || a.read("key.pem") != "secret\n" {
		t.Fatalf("report = %+v mode = %v", rep, a.mode("key.pem"))
	}
	if len(a.trash()) != 0 {
		t.Fatal("unchanged content trashed")
	}
	wantAction(t, a.item(a.plan(Options{Mode: ModeRestore}), "key.pem"), ActionInSync)
}

// TestConflictWritesBlobBeforeLocalFile pins the §9.3 order: when writing the
// local file fails, the resolved content is already a vault blob and no
// journal entry or base was recorded for it.
func TestConflictWritesBlobBeforeLocalFile(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("directory permissions are not enforced on Windows")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	a, b := twoMachines(t)
	a.write("f", "theirs\n")
	a.sync(Options{Track: track("f")})
	b.write("f", "mine\n")
	pl := b.plan(Options{})
	it := b.item(pl, "f")
	wantConflict(t, it, ConflictNoBase, true)

	if err := os.Chmod(b.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chmod(b.dir, 0o755) }()
	custom := []byte("custom\n")
	rep := b.apply(pl, Resolutions{it.Key: {Kind: ChooseCustom, Content: custom}}, Options{})
	if len(rep.Errors) != 1 || rep.Errors[0].Key != it.Key || rep.Resolved != 0 {
		t.Fatalf("report = %+v", rep)
	}
	if !b.v.HasBlob(b.v.BlobID(custom)) {
		t.Fatal("blob not written before the local file")
	}
	if b.read("f") != "mine\n" {
		t.Fatalf("local file = %q", b.read("f"))
	}
	if h := b.head("f"); h.Entry == nil || h.Entry.Machine != mA {
		t.Fatalf("head = %+v, want A's entry untouched", h.Entry)
	}
	if _, ok := b.base("f"); ok {
		t.Fatal("base recorded for a failed item")
	}
	// The pre-image went to the trash before the overwrite was attempted.
	if tr := b.trash(); len(tr) != 1 || tr[0].Path != "f" {
		t.Fatalf("trash after the failed attempt = %+v", tr)
	}
	_ = os.Chmod(b.dir, 0o755)
	// Same resolution succeeds afterwards (and trashes the pre-image again).
	pl = b.plan(Options{})
	noErrors(t, b.apply(pl, Resolutions{it.Key: {Kind: ChooseCustom, Content: custom}}, Options{}))
	if b.read("f") != "custom\n" || len(b.trash()) != 2 {
		t.Fatalf("f = %q trash = %d", b.read("f"), len(b.trash()))
	}
	if h := b.head("f"); h.Entry == nil || h.Entry.Machine != mB || h.Entry.Blob != b.v.BlobID(custom) {
		t.Fatalf("head = %+v", h.Entry)
	}
}

// TestTooLargeLocalReportsVaultSide: a local file above max_file_size stays
// in the plan as report only, so vault-side changes are still visible.
func TestTooLargeLocalReportsVaultSide(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{"f": "v1\n", "g": "g1\n"})
	big := strings.Repeat("x", 17)
	b.cfg.Scan.MaxFileSize = "16"
	b.eng = New(b.v, b.st, b.cfg, remote.None{}, b.eng.machine)
	if b.eng.MaxFileSize() != 16 {
		t.Fatalf("MaxFileSize = %d", b.eng.MaxFileSize())
	}

	check := func(t *testing.T, p string, orig Action, sub string) *Plan {
		t.Helper()
		pl := b.plan(Options{})
		it := b.item(pl, p)
		wantAction(t, it, ActionReportOnly)
		if it.Original != orig || it.NeedsResolution || it.Local == nil || it.Local.Blob != "" || it.Local.Size != 17 || it.LocalText != nil {
			t.Fatalf("item = %+v", it)
		}
		if !strings.Contains(it.Reason, "larger than") || !strings.Contains(it.Reason, sub) {
			t.Fatalf("reason = %q", it.Reason)
		}
		if !hasWarning(pl.Projects[0].Warnings, p+": larger than") {
			t.Fatalf("warnings = %v", pl.Projects[0].Warnings)
		}
		before, _ := b.base(p)
		rep := b.apply(pl, nil, Options{})
		noErrors(t, rep)
		after, _ := b.base(p)
		if rep.Skipped == 0 || b.read(p) != big || after.Blob != before.Blob || after.Kind != before.Kind {
			t.Fatalf("apply changed something: %+v", rep)
		}
		return pl
	}

	t.Run("vault unchanged", func(t *testing.T) {
		b.write("f", big)
		check(t, "f", ActionUpload, "cannot be uploaded")
	})
	t.Run("vault modified", func(t *testing.T) {
		a.write("f", "v2\n")
		a.sync(Options{})
		check(t, "f", ActionDownload, "modified in the vault")
	})
	t.Run("vault deleted", func(t *testing.T) {
		if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"f"}); err != nil {
			t.Fatal(err)
		}
		check(t, "f", ActionTrashLocal, "deleted in the vault")
	})
	t.Run("restore", func(t *testing.T) {
		b.write("g", big)
		pl := b.plan(Options{Mode: ModeRestore})
		it := b.item(pl, "g")
		wantAction(t, it, ActionReportOnly)
		if it.Original != ActionDownload || !strings.Contains(it.Reason, "restore") {
			t.Fatalf("item = %+v", it)
		}
		noItem(t, pl, "f") // deleted heads are ignored by restore
	})
	t.Run("untracked head still drops the base", func(t *testing.T) {
		if err := a.eng.Untrack(context.Background(), projID, []string{"g"}); err != nil {
			t.Fatal(err)
		}
		pl := b.plan(Options{})
		wantAction(t, b.item(pl, "g"), ActionUntrack)
		noErrors(t, b.apply(pl, nil, Options{}))
		if _, ok := b.base("g"); ok {
			t.Fatal("base kept")
		}
		if b.read("g") != big {
			t.Fatal("file touched")
		}
		noItem(t, b.plan(Options{}), "g")
	})
	t.Run("summary and untracked new file", func(t *testing.T) {
		b.write("h", big)
		noItem(t, b.plan(Options{}), "h")
		pl := b.plan(Options{Track: track("h")})
		wantAction(t, b.item(pl, "h"), ActionReportOnly)
		if s := Summarize(&pl.Projects[0]); s.LocalChanges != 1 {
			t.Fatalf("summary = %+v", s)
		}
	})
}

// TestNilRemoteIsAnError: an engine wired without a remote must not report a
// successful fetch/push; remote.None{} is the explicit "no remote".
func TestNilRemoteIsAnError(t *testing.T) {
	t.Parallel()
	_, v := newVaultDir(t)
	a := newMachine(t, mA, "alpha", v)
	ctx := context.Background()
	eng := New(a.v, a.st, a.cfg, nil, a.eng.machine)
	if err := eng.Fetch(ctx, nil); err == nil || !strings.Contains(err.Error(), "no remote") {
		t.Fatalf("Fetch with nil remote = %v", err)
	}
	if err := eng.Push(ctx, nil); err == nil || !strings.Contains(err.Error(), "no remote") {
		t.Fatalf("Push with nil remote = %v", err)
	}
	var nilEngine *Engine
	if err := nilEngine.Fetch(ctx, nil); err == nil {
		t.Fatal("nil engine fetch succeeded")
	}
	// remote.None is fine and emits no events.
	events := 0
	progress := func(Event) { events++ }
	if err := a.eng.Fetch(ctx, progress); err != nil {
		t.Fatal(err)
	}
	if err := a.eng.Push(ctx, progress); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(a.eng.Fetch(canceled(), nil), context.Canceled) {
		t.Fatal("canceled context not honoured")
	}
}

func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
