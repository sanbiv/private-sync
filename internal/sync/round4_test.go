package sync

import (
	"strings"
	"testing"

	"github.com/sanbiv/private-sync/internal/merge"
)

// Regression tests for the fourth review round: every test here pins one
// finding.

// TestConcurrentMergeOrientationOnHighID pins the orientation of
// Item.Merge for a concurrent (row 2) conflict. The pre-merge folds the
// candidates in machine-id order so that every machine agrees on the merged
// head, but the Result handed to the resolver must be oriented to *this*
// machine: Hunk.Local is this machine's side (what the TUI labels "local")
// and Hunk.Remote is Item.HeadText (what it labels with Item.Remote's
// machine). Folding order used to leak through, swapping the two sides on
// every machine but the lowest id, so picking "local" in the resolver wrote
// and published the *other* machine's value.
func TestConcurrentMergeOrientationOnHighID(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{".env": "K=base\n"})
	diverge(t, a, b, ".env", "K=alpha\n", "K=beta\n")

	for _, tc := range []struct {
		m          *machine
		own, other string
	}{
		{a, "K=alpha\n", "K=beta\n"}, // lowest machine id: was already correct
		{b, "K=beta\n", "K=alpha\n"}, // higher machine id: used to be inverted
	} {
		pl := tc.m.plan(Options{})
		it := tc.m.item(pl, ".env")
		wantConflict(t, it, ConflictConcurrent, true)
		if string(it.LocalText) != tc.own || string(it.HeadText) != tc.other {
			t.Fatalf("%s: LocalText = %q HeadText = %q, want %q / %q",
				tc.m.id[:8], it.LocalText, it.HeadText, tc.own, tc.other)
		}
		if it.Merge == nil || len(it.Merge.Hunks) != 1 {
			t.Fatalf("%s: merge = %+v, want one hunk", tc.m.id[:8], it.Merge)
		}
		h := it.Merge.Hunks[0]
		if string(h.Local) != tc.own || string(h.Remote) != tc.other {
			t.Fatalf("%s: hunk local = %q remote = %q, want %q / %q",
				tc.m.id[:8], h.Local, h.Remote, tc.own, tc.other)
		}
		// The markers the resolver renders must agree with the labels.
		markers := string(merge.RenderMarkers(it.Merge, "local", "OTHER"))
		if !strings.Contains(markers, "<<<<<<< local\n"+tc.own) {
			t.Fatalf("%s: markers mislabel the local side:\n%s", tc.m.id[:8], markers)
		}
	}

	// Choosing "local" on B (the higher id) must keep B's own value, on B and
	// on everyone converging to it. This is exactly what the per-key dotenv
	// chooser in internal/tui/resolver.go feeds to merge.Resolve.
	pl := b.plan(Options{})
	it := b.item(pl, ".env")
	choices := make([]merge.Choice, len(it.Merge.Hunks))
	for i := range choices {
		choices[i] = merge.Choice{Side: merge.SideLocal}
	}
	merged, err := merge.Resolve(it.Merge, choices)
	if err != nil {
		t.Fatal(err)
	}
	noErrors(t, b.apply(pl, Resolutions{it.Key: {Kind: ChooseCustom, Content: merged}}, Options{}))
	if got := b.read(".env"); got != "K=beta\n" {
		t.Fatalf("choosing local on B: .env = %q, want B's own value", got)
	}
	a.sync(Options{})
	if got := a.read(".env"); got != "K=beta\n" {
		t.Fatalf("A converged to %q, want B's published value", got)
	}
}

// TestChooseRemoteEmptyVaultFile pins that taking the remote side works when
// the vault version is a zero-length file. A zero-length plaintext reads back
// as a nil slice, which used to be indistinguishable from "no head content":
// every ChooseRemote resolution errored out and the conflict was re-planned
// identically forever, so --strategy remote could never converge.
func TestChooseRemoteEmptyVaultFile(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{".env": "K=1\n"})
	a.write(".env", "") // truncating a secrets file is a normal operation
	a.sync(Options{})
	b.write(".env", "K=2\nX=9\n")

	pl := b.plan(Options{Strategy: StrategyRemote})
	it := b.item(pl, ".env")
	wantConflict(t, it, ConflictContent, true)
	if it.HeadText == nil {
		t.Fatal("HeadText is nil for an empty vault file; empty content must not read as absent")
	}
	res := Resolutions{}
	ApplyStrategy(pl, StrategyRemote, res)
	noErrors(t, b.apply(pl, res, Options{Strategy: StrategyRemote}))
	if got := b.read(".env"); got != "" {
		t.Fatalf("B .env = %q, want the empty vault version", got)
	}
	// And it stays converged: the path is no longer a conflict.
	wantAction(t, b.item(b.plan(Options{}), ".env"), ActionInSync)
}

// TestEmptyBaseStillMerges pins that a base version that is an empty file is
// still passed to merge.ThreeWay as a base. merge.ThreeWay reads a nil base as
// "no common ancestor"; the vault returns nil for a zero-length plaintext, so
// an empty base used to force a whole-file manual conflict where the two sides
// merge cleanly.
func TestEmptyBaseStillMerges(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{".env": ""})
	a.write(".env", "A=1\n")
	a.sync(Options{})
	b.write(".env", "B=2\n")

	it := b.item(b.plan(Options{}), ".env")
	if it.BaseText == nil {
		t.Fatal("BaseText is nil for an empty base; empty content must not read as no base")
	}
	if it.Merge == nil || !it.Merge.Clean || it.NeedsResolution {
		t.Fatalf("merge = %+v needs = %v, want a clean automatic merge", it.Merge, it.NeedsResolution)
	}
	if it.Merge.Note == "no common ancestor" {
		t.Fatalf("empty base discarded: note = %q", it.Merge.Note)
	}
	if got := string(it.Merge.Merged); !strings.Contains(got, "A=1") || !strings.Contains(got, "B=2") {
		t.Fatalf("merged = %q, want both keys", got)
	}
	b.sync(Options{})
	got := b.read(".env")
	if !strings.Contains(got, "A=1") || !strings.Contains(got, "B=2") {
		t.Fatalf("B .env = %q, want both keys", got)
	}
	a.sync(Options{})
	if a.read(".env") != got {
		t.Fatalf("A .env = %q, want %q", a.read(".env"), got)
	}
}

// TestEmptyBaseConcurrentHeadsMerge pins the same normalisation on the
// concurrent-head pre-merge (row 2), which reads the common base itself.
func TestEmptyBaseConcurrentHeadsMerge(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{".env": ""})
	diverge(t, a, b, ".env", "A=1\n", "B=2\n")

	for _, m := range []*machine{a, b} {
		it := m.item(m.plan(Options{}), ".env")
		if it.Action == ActionConflict || it.NeedsResolution {
			t.Fatalf("%s: empty base forced a manual conflict: %s", m.id[:8], it.Reason)
		}
		m.sync(Options{})
		got := m.read(".env")
		if !strings.Contains(got, "A=1") || !strings.Contains(got, "B=2") {
			t.Fatalf("%s: .env = %q, want both keys", m.id[:8], got)
		}
	}
}
