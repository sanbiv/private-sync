package sync

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sanbiv/private-sync/internal/identity"
	"github.com/sanbiv/private-sync/internal/merge"
	"github.com/sanbiv/private-sync/internal/state"
	"github.com/sanbiv/private-sync/internal/vault"
)

// emptyReport reports whether nothing was counted or recorded.
func emptyReport(rep *Report) bool {
	return rep.Uploaded == 0 && rep.Downloaded == 0 && rep.Converged == 0 && rep.Trashed == 0 &&
		rep.Untracked == 0 && rep.Deleted == 0 && rep.Skipped == 0 && rep.Pending == 0 &&
		rep.Resolved == 0 && len(rep.Unresolved) == 0 && len(rep.Errors) == 0
}

// TestDecisionTable exercises every row of spec §9.2 through real vault,
// state and project directories.
func TestDecisionTable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name string
		run  func(t *testing.T, a, b *machine)
	}{
		{"row1 untracked head drops base and leaves the file", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			if err := a.eng.Untrack(context.Background(), projID, []string{"f"}); err != nil {
				t.Fatal(err)
			}
			if _, ok := a.base("f"); ok {
				t.Fatal("A still has a base after Untrack")
			}
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantAction(t, it, ActionUntrack)
			rep := b.apply(pl, nil, Options{})
			noErrors(t, rep)
			if rep.Untracked != 1 {
				t.Fatalf("Untracked = %d", rep.Untracked)
			}
			if _, ok := b.base("f"); ok {
				t.Fatal("B base not dropped")
			}
			if b.read("f") != "v1\n" {
				t.Fatal("B file touched")
			}
			// Later local edits produce no upload (untrack is sticky).
			b.write("f", "edited\n")
			noItem(t, b.plan(Options{}), "f")
			// Untracked and no base on a second plan: nothing to report.
			noItem(t, b.plan(Options{}), "f")
		}},
		{"row1 Track re-adds an untracked path with a ticked clock", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			if err := a.eng.Untrack(context.Background(), projID, []string{"f"}); err != nil {
				t.Fatal(err)
			}
			b.sync(Options{})
			b.write("f", "again\n")
			pl := b.plan(Options{Track: track("f")})
			it := b.item(pl, "f")
			wantAction(t, it, ActionUpload)
			noErrors(t, b.apply(pl, nil, Options{Track: track("f")}))
			h := b.head("f")
			if h.Entry == nil || h.Entry.Kind != vault.KindFile || h.Entry.Machine != mB {
				t.Fatalf("head after re-add = %+v", h)
			}
			if h.Entry.Clock[mB] != 1 || h.Entry.Clock[mA] < 2 {
				t.Fatalf("clock = %v, want ticked merge of the untracked entry", h.Entry.Clock)
			}
			// A still holds its old copy with no base: two-way conflict (row 10);
			// without a local copy the re-added file simply downloads (row 12).
			wantConflict(t, a.item(a.plan(Options{}), "f"), ConflictNoBase, true)
			a.remove("f")
			wantAction(t, a.item(a.plan(Options{}), "f"), ActionDownload)
		}},
		{"row1 Track without a local file warns", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			if err := a.eng.Untrack(context.Background(), projID, []string{"f"}); err != nil {
				t.Fatal(err)
			}
			b.remove("f")
			pl := b.plan(Options{Track: track("f")})
			noItem(t, pl, "f")
			if !hasWarning(pl.Projects[0].Warnings, "not found locally") {
				t.Fatalf("warnings = %v", pl.Projects[0].Warnings)
			}
		}},
		{"row2 concurrent heads merge cleanly into a synthetic head", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{".env": "A=1\nB=1\n"})
			// B edits and plans while A's journal is still old.
			b.write(".env", "A=1\nB=2\n")
			stale := b.plan(Options{})
			wantAction(t, b.item(stale, ".env"), ActionUpload)
			// A edits and applies first.
			a.write(".env", "A=2\nB=1\n")
			a.sync(Options{})
			// B applies its stale plan: two concurrent entries now exist.
			noErrors(t, b.apply(stale, nil, Options{}))
			if h := a.head(".env"); !h.Concurrent() || len(h.Candidates) != 2 || h.Base == "" {
				t.Fatalf("head = %+v, want concurrent with a base", h)
			}
			pl := a.plan(Options{})
			it := a.item(pl, ".env")
			wantAction(t, it, ActionDownload)
			if !it.Synthetic || it.Head == nil || len(it.Candidates) != 2 {
				t.Fatalf("item = %+v, want synthetic head with 2 candidates", it)
			}
			if string(it.HeadText) != "A=2\nB=2\n" {
				t.Fatalf("HeadText = %q", it.HeadText)
			}
			if !strings.Contains(it.Reason, "merged cleanly") {
				t.Fatalf("reason = %q", it.Reason)
			}
			rep := a.apply(pl, nil, Options{})
			noErrors(t, rep)
			if rep.Downloaded != 1 {
				t.Fatalf("report = %+v", rep)
			}
			if a.read(".env") != "A=2\nB=2\n" {
				t.Fatalf("A .env = %q", a.read(".env"))
			}
			h := a.head(".env")
			if h.Entry == nil || len(h.Entry.Parents) != 2 || h.Entry.Machine != mA {
				t.Fatalf("head after merge = %+v", h)
			}
			// A's pre-image equals its base: nothing trashed.
			if len(a.trash()) != 0 {
				t.Fatalf("trash = %v", a.trash())
			}
			// B converges to the merged version.
			pl = b.plan(Options{})
			wantAction(t, b.item(pl, ".env"), ActionDownload)
			noErrors(t, b.apply(pl, nil, Options{}))
			if b.read(".env") != "A=2\nB=2\n" {
				t.Fatalf("B .env = %q", b.read(".env"))
			}
			wantAction(t, b.item(b.plan(Options{}), ".env"), ActionInSync)
			wantAction(t, a.item(a.plan(Options{}), ".env"), ActionInSync)
		}},
		{"row2 synthetic head equal to local converges and writes the entry", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"notes.txt": "one\ntwo\nthree\n"})
			b.write("notes.txt", "one\ntwo\nthree\nfour\n")
			stale := b.plan(Options{})
			a.write("notes.txt", "zero\none\ntwo\nthree\n")
			a.sync(Options{})
			noErrors(t, b.apply(stale, nil, Options{}))
			// A's local already holds the merged text.
			a.write("notes.txt", "zero\none\ntwo\nthree\nfour\n")
			pl := a.plan(Options{})
			it := a.item(pl, "notes.txt")
			wantAction(t, it, ActionConverge)
			if !it.Synthetic {
				t.Fatal("not synthetic")
			}
			noErrors(t, a.apply(pl, nil, Options{}))
			if h := a.head("notes.txt"); h.Entry == nil || len(h.Entry.Parents) != 2 {
				t.Fatalf("head = %+v", h)
			}
		}},
		{"row2 concurrent heads that do not merge are ConflictConcurrent", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{".env": "A=1\n"})
			b.write(".env", "A=3\n")
			stale := b.plan(Options{})
			a.write(".env", "A=2\n")
			a.sync(Options{})
			noErrors(t, b.apply(stale, nil, Options{}))
			pl := a.plan(Options{})
			it := a.item(pl, ".env")
			wantConflict(t, it, ConflictConcurrent, true)
			if it.Merge == nil || it.Merge.Clean || len(it.Merge.Hunks) != 1 || it.Merge.Hunks[0].Key != "A" {
				t.Fatalf("merge = %+v", it.Merge)
			}
			if it.Head != nil || len(it.Candidates) != 2 {
				t.Fatalf("head = %+v candidates = %d", it.Head, len(it.Candidates))
			}
			// HeadText is the OTHER machine's version, never A's own entry
			// (candidates come in machine-id order, A first).
			if string(it.HeadText) != "A=3\n" {
				t.Fatalf("HeadText = %q, want B's version", it.HeadText)
			}
			if it.Remote == nil || it.Remote.Machine != mB || it.Remote.Kind != vault.KindFile || !strings.Contains(it.Reason, "remote = "+mB[:8]) {
				t.Fatalf("Remote = %+v reason = %q", it.Remote, it.Reason)
			}
			if string(it.BaseText) != "A=1\n" || string(it.LocalText) != "A=2\n" {
				t.Fatalf("BaseText = %q LocalText = %q", it.BaseText, it.LocalText)
			}
			// Unresolved: skipped and listed.
			rep := a.apply(pl, nil, Options{})
			noErrors(t, rep)
			if len(rep.Unresolved) != 1 || rep.Skipped != 1 {
				t.Fatalf("report = %+v", rep)
			}
			// Custom resolution converges everybody.
			res := Resolutions{it.Key: {Kind: ChooseCustom, Content: []byte("A=23\n")}}
			pl = a.plan(Options{})
			rep = a.apply(pl, res, Options{})
			noErrors(t, rep)
			if rep.Resolved != 1 || rep.Uploaded != 1 {
				t.Fatalf("report = %+v", rep)
			}
			if a.read(".env") != "A=23\n" {
				t.Fatal("custom content not written")
			}
			if len(a.trash()) != 0 {
				t.Fatal("A's pre-image equals its base, must not be trashed")
			}
			h := a.head(".env")
			if h.Entry == nil || len(h.Entry.Parents) != 2 {
				t.Fatalf("head = %+v, want single with 2 parents (A's blob is also the base)", h)
			}
			b.sync(Options{})
			if b.read(".env") != "A=23\n" {
				t.Fatalf("B .env = %q", b.read(".env"))
			}
		}},
		{"row2 tombstone candidate never merges", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			b.write("f", "v2\n")
			stale := b.plan(Options{})
			if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"f"}); err != nil {
				t.Fatal(err)
			}
			noErrors(t, b.apply(stale, nil, Options{}))
			pl := a.plan(Options{})
			it := a.item(pl, "f")
			wantConflict(t, it, ConflictConcurrent, true)
			if it.Merge != nil {
				t.Fatal("merge result for a tombstone candidate")
			}
			if !strings.Contains(it.Reason, "deleted on") || !strings.Contains(it.Reason, "modified on") {
				t.Fatalf("reason = %q", it.Reason)
			}
			if string(it.HeadText) != "v2\n" {
				t.Fatalf("HeadText = %q, want the file candidate", it.HeadText)
			}
			// Remote wins: the file candidate is restored on A.
			res := Resolutions{}
			ApplyStrategy(pl, StrategyRemote, res)
			noErrors(t, a.apply(pl, res, Options{}))
			if a.read("f") != "v2\n" {
				t.Fatalf("A f = %q", a.read("f"))
			}
			if h := a.head("f"); h.Entry == nil || h.Entry.Kind != vault.KindFile {
				t.Fatalf("head = %+v", h)
			}
		}},
		{"row3 new local file uploads only when tracked", func(t *testing.T, a, b *machine) {
			a.write("f", "v1\n", 0o640)
			noItem(t, a.plan(Options{}), "f")
			pl := a.plan(Options{Track: track("f")})
			it := a.item(pl, "f")
			wantAction(t, it, ActionUpload)
			if it.Local == nil || it.Local.Mode != 0o640 || it.Base != nil || it.Head != nil {
				t.Fatalf("item = %+v", it)
			}
			rep := a.apply(pl, nil, Options{Track: track("f")})
			noErrors(t, rep)
			if rep.Uploaded != 1 {
				t.Fatalf("report = %+v", rep)
			}
			h := a.head("f")
			if h.Entry == nil || h.Entry.Mode != 0o640 || len(h.Entry.Parents) != 0 || h.Entry.Clock[mA] != 1 {
				t.Fatalf("head = %+v", h.Entry)
			}
			if bs, ok := a.base("f"); !ok || bs.Blob != h.Entry.Blob {
				t.Fatalf("base = %+v", bs)
			}
			if !a.v.HasBlob(h.Entry.Blob) {
				t.Fatal("blob missing")
			}
		}},
		{"row3 base present without a head re-publishes", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			if err := os.Remove(a.journalFile()); err != nil {
				t.Fatal(err)
			}
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantAction(t, it, ActionUpload)
			if !strings.Contains(it.Reason, "re-publishing") {
				t.Fatalf("reason = %q", it.Reason)
			}
			noErrors(t, b.apply(pl, nil, Options{}))
			if h := b.head("f"); h.Entry == nil || h.Entry.Machine != mB {
				t.Fatalf("head = %+v", h)
			}
		}},
		{"row4 no head and no local file drops the base", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			if err := os.Remove(a.journalFile()); err != nil {
				t.Fatal(err)
			}
			b.remove("f")
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantAction(t, it, ActionUntrack)
			noErrors(t, b.apply(pl, nil, Options{}))
			if _, ok := b.base("f"); ok {
				t.Fatal("base kept")
			}
			noItem(t, b.plan(Options{}), "f")
		}},
		{"row5 deleted head and absent local converge to a tombstone", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
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
			rep := b.apply(pl, nil, Options{})
			noErrors(t, rep)
			if rep.Converged != 1 {
				t.Fatalf("report = %+v", rep)
			}
			bs, ok := b.base("f")
			if !ok || bs.Kind != vault.KindDeleted || bs.Blob != "" {
				t.Fatalf("base = %+v", bs)
			}
			noItem(t, b.plan(Options{}), "f")
		}},
		{"row6 deleted head and unchanged local goes to the trash", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"f"}); err != nil {
				t.Fatal(err)
			}
			pl := b.plan(Options{})
			wantAction(t, b.item(pl, "f"), ActionTrashLocal)
			rep := b.apply(pl, nil, Options{})
			noErrors(t, rep)
			if rep.Trashed != 1 {
				t.Fatalf("report = %+v", rep)
			}
			if b.exists("f") {
				t.Fatal("file still present")
			}
			tr := b.trash()
			if len(tr) != 1 || tr[0].Path != "f" || tr[0].Project != projID {
				t.Fatalf("trash = %+v", tr)
			}
			content, _, err := b.st.TrashRead(b.v.Keys(), tr[0].ID)
			if err != nil || string(content) != "v1\n" {
				t.Fatalf("TrashRead = %q, %v", content, err)
			}
			if bs, ok := b.base("f"); !ok || bs.Kind != vault.KindDeleted {
				t.Fatalf("base = %+v", bs)
			}
			noItem(t, b.plan(Options{}), "f")
		}},
		{"row7 deleted head and modified local: keep re-uploads", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"f"}); err != nil {
				t.Fatal(err)
			}
			a.remove("f")
			b.write("f", "v2\n")
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantConflict(t, it, ConflictModifyDelete, true)
			if it.Merge != nil || string(it.LocalText) != "v2\n" {
				t.Fatalf("item = %+v", it)
			}
			res := Resolutions{it.Key: {Kind: ChooseKeep}}
			rep := b.apply(pl, res, Options{})
			noErrors(t, rep)
			if rep.Uploaded != 1 || rep.Resolved != 1 {
				t.Fatalf("report = %+v", rep)
			}
			h := b.head("f")
			if h.Entry == nil || h.Entry.Kind != vault.KindFile || h.Entry.Machine != mB {
				t.Fatalf("head = %+v", h)
			}
			if h.Entry.Clock[mA] < 2 || h.Entry.Clock[mB] != 1 {
				t.Fatalf("clock = %v, want ticked from the tombstone", h.Entry.Clock)
			}
			if len(h.Entry.Parents) != 1 { // the base blob
				t.Fatalf("parents = %v", h.Entry.Parents)
			}
			pl = a.plan(Options{})
			wantAction(t, a.item(pl, "f"), ActionDownload)
			noErrors(t, a.apply(pl, nil, Options{}))
			if a.read("f") != "v2\n" {
				t.Fatalf("A f = %q", a.read("f"))
			}
		}},
		{"row7 deleted head and modified local: delete trashes", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"f"}); err != nil {
				t.Fatal(err)
			}
			b.write("f", "v2\n")
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantConflict(t, it, ConflictModifyDelete, true)
			// ChooseRemote is mapped to ChooseDelete for modify/delete.
			rep := b.apply(pl, Resolutions{it.Key: {Kind: ChooseRemote}}, Options{})
			noErrors(t, rep)
			if rep.Trashed != 1 || rep.Resolved != 1 || b.exists("f") {
				t.Fatalf("report = %+v exists = %v", rep, b.exists("f"))
			}
			tr := b.trash()
			if len(tr) != 1 {
				t.Fatalf("trash = %+v", tr)
			}
			if c, _, _ := b.st.TrashRead(b.v.Keys(), tr[0].ID); string(c) != "v2\n" {
				t.Fatalf("trashed = %q", c)
			}
		}},
		{"row8 deleted head with local file and no base is modify/delete", func(t *testing.T, a, b *machine) {
			a.write("f", "v1\n")
			a.sync(Options{Track: track("f")})
			if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"f"}); err != nil {
				t.Fatal(err)
			}
			b.write("f", "mine\n")
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantConflict(t, it, ConflictModifyDelete, true)
			if it.Base != nil || !strings.Contains(it.Reason, "no base") {
				t.Fatalf("item = %+v", it)
			}
			// Skip leaves everything alone.
			rep := b.apply(pl, Resolutions{it.Key: {Kind: ChooseSkip}}, Options{})
			noErrors(t, rep)
			if rep.Skipped != 1 || len(rep.Unresolved) != 0 || !b.exists("f") {
				t.Fatalf("report = %+v", rep)
			}
		}},
		{"row9 deleted head with tombstone base is report only", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"f"}); err != nil {
				t.Fatal(err)
			}
			pl := a.plan(Options{})
			it := a.item(pl, "f")
			wantAction(t, it, ActionReportOnly)
			if it.Original != ActionMissingLocal || !strings.Contains(it.Reason, "left in place") || strings.Contains(it.Reason, "mode") {
				t.Fatalf("item = %+v", it)
			}
			// Status counts it as a tracked path with nothing applied, not as in sync.
			if s := Summarize(&pl.Projects[0]); s.Missing != 1 || s.InSync != 0 {
				t.Fatalf("summary = %+v", s)
			}
			rep := a.apply(pl, nil, Options{})
			noErrors(t, rep)
			if rep.Skipped != 1 || !a.exists("f") {
				t.Fatalf("report = %+v", rep)
			}
			// Re-tracking works.
			pl = a.plan(Options{Track: track("f")})
			wantAction(t, a.item(pl, "f"), ActionUpload)
		}},
		{"row10 same content without a base converges", func(t *testing.T, a, b *machine) {
			a.write("f", "same\n")
			a.sync(Options{Track: track("f")})
			b.write("f", "same\n")
			pl := b.plan(Options{})
			wantAction(t, b.item(pl, "f"), ActionConverge)
			rep := b.apply(pl, nil, Options{})
			noErrors(t, rep)
			if rep.Converged != 1 {
				t.Fatalf("report = %+v", rep)
			}
			bs, ok := b.base("f")
			if !ok || bs.Blob != a.head("f").Entry.Blob {
				t.Fatalf("base = %+v", bs)
			}
			wantAction(t, b.item(b.plan(Options{}), "f"), ActionInSync)
		}},
		{"row10 different content without a base is a two-way conflict", func(t *testing.T, a, b *machine) {
			a.write("f", "theirs\n")
			a.sync(Options{Track: track("f")})
			b.write("f", "mine\n")
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantConflict(t, it, ConflictNoBase, true)
			if it.Merge == nil || it.Merge.Clean || len(it.Merge.Hunks) != 1 {
				t.Fatalf("merge = %+v", it.Merge)
			}
			if string(it.HeadText) != "theirs\n" || string(it.LocalText) != "mine\n" || it.BaseText != nil {
				t.Fatalf("texts = %q %q %q", it.LocalText, it.HeadText, it.BaseText)
			}
			res := Resolutions{}
			ApplyStrategy(pl, StrategyLocal, res)
			rep := b.apply(pl, res, Options{})
			noErrors(t, rep)
			if rep.Resolved != 1 || b.read("f") != "mine\n" || len(b.trash()) != 0 {
				t.Fatalf("report = %+v", rep)
			}
			h := b.head("f")
			if h.Entry == nil || len(h.Entry.Parents) != 1 || h.Entry.Machine != mB {
				t.Fatalf("head = %+v", h)
			}
			a.sync(Options{})
			if a.read("f") != "mine\n" {
				t.Fatalf("A f = %q", a.read("f"))
			}
		}},
		{"row10 remote choice rewrites the local file and trashes it", func(t *testing.T, a, b *machine) {
			a.write("f", "theirs\n")
			a.sync(Options{Track: track("f")})
			b.write("f", "mine\n")
			pl := b.plan(Options{})
			res := Resolutions{}
			ApplyStrategy(pl, StrategyRemote, res)
			noErrors(t, b.apply(pl, res, Options{}))
			if b.read("f") != "theirs\n" {
				t.Fatalf("B f = %q", b.read("f"))
			}
			tr := b.trash()
			if len(tr) != 1 {
				t.Fatalf("trash = %+v", tr)
			}
			if c, _, _ := b.st.TrashRead(b.v.Keys(), tr[0].ID); string(c) != "mine\n" {
				t.Fatalf("trashed = %q", c)
			}
		}},
		{"row11 in sync", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantAction(t, it, ActionInSync)
			if it.LocalText != nil || it.HeadText != nil {
				t.Fatal("texts loaded for an in-sync item")
			}
			rep := b.apply(pl, nil, Options{})
			noErrors(t, rep)
			if !emptyReport(rep) {
				t.Fatalf("report = %+v", rep)
			}
			for _, w := range b.v.Written() {
				if strings.HasPrefix(w, "projects/") {
					t.Fatalf("written = %v, want no journal for an in-sync apply", b.v.Written())
				}
			}
		}},
		{"row11 local change uploads", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			b.write("f", "v2\n")
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantAction(t, it, ActionUpload)
			noErrors(t, b.apply(pl, nil, Options{}))
			h := b.head("f")
			if h.Entry == nil || h.Entry.Machine != mB || len(h.Entry.Parents) != 1 || h.Entry.Parents[0] != it.Base.Blob {
				t.Fatalf("head = %+v", h.Entry)
			}
			if h.Entry.Clock[mA] != 1 || h.Entry.Clock[mB] != 1 {
				t.Fatalf("clock = %v", h.Entry.Clock)
			}
			if b.st.JournalSeq(mB) != 1 || b.st.JournalSeq(mA) != 1 {
				t.Fatalf("journal seqs = %v", b.st.JournalSeqs())
			}
		}},
		{"row11 vault change downloads without trashing the base", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			a.write("f", "v2\n", 0o640)
			a.sync(Options{})
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantAction(t, it, ActionDownload)
			if string(it.HeadText) != "v2\n" {
				t.Fatalf("HeadText = %q", it.HeadText)
			}
			rep := b.apply(pl, nil, Options{})
			noErrors(t, rep)
			if rep.Downloaded != 1 || b.read("f") != "v2\n" || b.mode("f") != 0o640 {
				t.Fatalf("report = %+v content = %q mode = %v", rep, b.read("f"), b.mode("f"))
			}
			if len(b.trash()) != 0 {
				t.Fatal("unchanged local copy was trashed")
			}
			if bs, _ := b.base("f"); bs.Blob != a.head("f").Entry.Blob {
				t.Fatalf("base = %+v", bs)
			}
		}},
		{"row11 same change on both sides converges", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			a.write("f", "v2\n")
			a.sync(Options{})
			b.write("f", "v2\n")
			pl := b.plan(Options{})
			wantAction(t, b.item(pl, "f"), ActionConverge)
			noErrors(t, b.apply(pl, nil, Options{}))
			wantAction(t, b.item(b.plan(Options{}), "f"), ActionInSync)
		}},
		{"row11 dotenv edits of different keys merge automatically", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{".env": "A=1\nB=1\n"})
			a.write(".env", "A=2\nB=1\n")
			a.sync(Options{})
			b.write(".env", "A=1\nB=2\n")
			pl := b.plan(Options{})
			it := b.item(pl, ".env")
			wantConflict(t, it, ConflictContent, false)
			if it.Merge == nil || !it.Merge.Clean || it.Merge.Kind != merge.KindDotenv || string(it.Merge.Merged) != "A=2\nB=2\n" {
				t.Fatalf("merge = %+v", it.Merge)
			}
			if string(it.BaseText) != "A=1\nB=1\n" {
				t.Fatalf("BaseText = %q", it.BaseText)
			}
			rep := b.apply(pl, nil, Options{})
			noErrors(t, rep)
			if rep.Resolved != 1 || rep.Uploaded != 1 || len(rep.Unresolved) != 0 {
				t.Fatalf("report = %+v", rep)
			}
			if b.read(".env") != "A=2\nB=2\n" {
				t.Fatalf("B .env = %q", b.read(".env"))
			}
			tr := b.trash()
			if len(tr) != 1 {
				t.Fatalf("trash = %+v, want B's pre-image", tr)
			}
			h := b.head(".env")
			if h.Entry == nil || len(h.Entry.Parents) != 2 {
				t.Fatalf("head = %+v", h.Entry)
			}
			a.sync(Options{})
			if a.read(".env") != "A=2\nB=2\n" {
				t.Fatalf("A .env = %q", a.read(".env"))
			}
		}},
		{"row11 dotenv edits of the same key need a resolution", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{".env": "A=1\n"})
			a.write(".env", "A=2\n")
			a.sync(Options{})
			b.write(".env", "A=3\n")
			pl := b.plan(Options{})
			it := b.item(pl, ".env")
			wantConflict(t, it, ConflictContent, true)
			if it.Merge == nil || it.Merge.Clean || len(it.Merge.Hunks) != 1 {
				t.Fatalf("merge = %+v", it.Merge)
			}
			if !strings.Contains(it.Reason, "1 conflicting hunk") {
				t.Fatalf("reason = %q", it.Reason)
			}
			rep := b.apply(pl, nil, Options{})
			noErrors(t, rep)
			if len(rep.Unresolved) != 1 || rep.Unresolved[0] != it.Key {
				t.Fatalf("report = %+v", rep)
			}
			// ChooseMerged is refused when the merge is not clean.
			rep = b.apply(pl, Resolutions{it.Key: {Kind: ChooseMerged}}, Options{})
			if len(rep.Errors) != 1 {
				t.Fatalf("errors = %v", rep.Errors)
			}
		}},
		{"row12 new vault file downloads with its mode", func(t *testing.T, a, b *machine) {
			a.write("dir/f", "v1\n", 0o640)
			a.sync(Options{Track: track("dir/f")})
			pl := b.plan(Options{})
			it := b.item(pl, "dir/f")
			wantAction(t, it, ActionDownload)
			if it.Local != nil || it.Base != nil {
				t.Fatalf("item = %+v", it)
			}
			rep := b.apply(pl, nil, Options{})
			noErrors(t, rep)
			if rep.Downloaded != 1 || b.read("dir/f") != "v1\n" || b.mode("dir/f") != 0o640 {
				t.Fatalf("report = %+v", rep)
			}
			if len(b.v.Written()) != 1 || !strings.HasPrefix(b.v.Written()[0], "machines/") {
				t.Fatalf("written = %v, want only the machine info", b.v.Written())
			}
		}},
		{"row13 missing local file is report only", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			b.remove("f")
			pl := b.plan(Options{})
			it := b.item(pl, "f")
			wantAction(t, it, ActionMissingLocal)
			if it.NeedsResolution {
				t.Fatal("NeedsResolution")
			}
			seq := b.head("f").Entry.Clock
			rep := b.apply(pl, nil, Options{})
			noErrors(t, rep)
			if rep.Skipped != 1 {
				t.Fatalf("report = %+v", rep)
			}
			if h := b.head("f"); h.Entry == nil || h.Entry.Clock.Compare(seq) != vault.Equal {
				t.Fatal("vault changed")
			}
			if _, ok := b.base("f"); !ok {
				t.Fatal("base dropped")
			}
		}},
		{"row13 PropagateDeletes writes a tombstone after confirmation", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			b.remove("f")
			opts := Options{PropagateDeletes: true}
			pl := b.plan(opts)
			it := b.item(pl, "f")
			wantAction(t, it, ActionDeleteRemote)
			if !it.NeedsResolution {
				t.Fatal("NeedsResolution false")
			}
			rep := b.apply(pl, nil, opts)
			noErrors(t, rep)
			if len(rep.Unresolved) != 1 {
				t.Fatalf("report = %+v", rep)
			}
			res := Resolutions{}
			ApplyStrategy(pl, StrategyLocal, res)
			if res[it.Key].Kind != ChooseConfirm {
				t.Fatalf("res = %+v", res)
			}
			rep = b.apply(pl, res, opts)
			noErrors(t, rep)
			if rep.Deleted != 1 {
				t.Fatalf("report = %+v", rep)
			}
			h := b.head("f")
			if h.Entry == nil || h.Entry.Kind != vault.KindDeleted || len(h.Entry.Parents) != 1 {
				t.Fatalf("head = %+v", h.Entry)
			}
			if bs, _ := b.base("f"); bs.Kind != vault.KindDeleted {
				t.Fatalf("base = %+v", bs)
			}
			pl = a.plan(Options{})
			wantAction(t, a.item(pl, "f"), ActionTrashLocal)
			noErrors(t, a.apply(pl, nil, Options{}))
			if a.exists("f") || len(a.trash()) != 1 {
				t.Fatal("A did not trash the file")
			}
		}},
		{"row14 missing local file with a newer vault version downloads", func(t *testing.T, a, b *machine) {
			seed(a, b, map[string]string{"f": "v1\n"})
			a.write("f", "v2\n")
			a.sync(Options{})
			b.remove("f")
			pl := b.plan(Options{})
			wantAction(t, b.item(pl, "f"), ActionDownload)
			noErrors(t, b.apply(pl, nil, Options{}))
			if b.read("f") != "v2\n" {
				t.Fatalf("B f = %q", b.read("f"))
			}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			a, b := twoMachines(t)
			tc.run(t, a, b)
		})
	}
}

func hasWarning(ws []string, sub string) bool {
	for _, w := range ws {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

func TestRollbackByJournalSequence(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{"f": "v1\n"})
	old, err := os.ReadFile(a.journalFile())
	if err != nil {
		t.Fatal(err)
	}
	a.write("f", "v2\n")
	a.sync(Options{})
	b.sync(Options{})
	if b.read("f") != "v2\n" {
		t.Fatal("B not at v2")
	}
	// A's journal is replaced by its older copy (seq 1 < seen 2).
	if err := os.WriteFile(a.journalFile(), old, 0o600); err != nil {
		t.Fatal(err)
	}
	pl := b.plan(Options{})
	it := b.item(pl, "f")
	wantAction(t, it, ActionRollback)
	if it.Original != ActionDownload || !strings.Contains(it.Reason, "rolled back") {
		t.Fatalf("item = %+v", it)
	}
	if !hasWarning(pl.Projects[0].Warnings, "rolled back") {
		t.Fatalf("warnings = %v", pl.Projects[0].Warnings)
	}
	rep := b.apply(pl, nil, Options{})
	noErrors(t, rep)
	if rep.Skipped != 1 || b.read("f") != "v2\n" {
		t.Fatalf("report = %+v content = %q", rep, b.read("f"))
	}
	if b.st.JournalSeq(mA) != 2 {
		t.Fatalf("stored seq lowered to %d", b.st.JournalSeq(mA))
	}
	if bs, _ := b.base("f"); bs.Clock[mA] != 2 {
		t.Fatalf("base changed: %+v", bs)
	}
	// Accepting the rollback applies the older version.
	opts := Options{AcceptRollback: true}
	pl = b.plan(opts)
	it = b.item(pl, "f")
	wantAction(t, it, ActionDownload)
	if !strings.Contains(it.Reason, "accepting rollback") {
		t.Fatalf("reason = %q", it.Reason)
	}
	noErrors(t, b.apply(pl, nil, opts))
	if b.read("f") != "v1\n" {
		t.Fatalf("B f = %q", b.read("f"))
	}
	if len(b.trash()) != 1 {
		t.Fatal("pre-image v2 not trashed")
	}
	if b.st.JournalSeq(mA) != 1 {
		t.Fatalf("stored seq = %d, want reset to the accepted journal", b.st.JournalSeq(mA))
	}
	wantAction(t, b.item(b.plan(Options{}), "f"), ActionInSync)
}

func TestRollbackByClock(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{"f": "v1\n"})
	v1 := a.head("f").Entry.Blob
	a.write("f", "v2\n")
	a.sync(Options{})
	b.sync(Options{})
	// A rewrites its journal with a high Seq but the old entry: the sequence
	// check passes, only the clock comparison can detect the rollback.
	old := vault.Entry{Path: "f", Kind: vault.KindFile, Blob: v1, Clock: vault.Clock{mA: 1}, Machine: mA, UpdatedAt: time.Now()}
	j := &vault.Journal{Machine: mA, Seq: 40, Entries: map[string]vault.Entry{"f": old}}
	if err := a.v.WriteJournal(projID, j); err != nil {
		t.Fatal(err)
	}
	pl := b.plan(Options{})
	it := b.item(pl, "f")
	wantAction(t, it, ActionRollback)
	if strings.Contains(it.Reason, "journal of") || !strings.Contains(it.Reason, "older") {
		t.Fatalf("reason = %q", it.Reason)
	}
	noErrors(t, b.apply(pl, nil, Options{}))
	if b.read("f") != "v2\n" {
		t.Fatal("file touched")
	}
	// Restore mode ignores the rollback.
	opts := Options{Mode: ModeRestore}
	pl = b.plan(opts)
	wantAction(t, b.item(pl, "f"), ActionDownload)
	noErrors(t, b.apply(pl, nil, opts))
	if b.read("f") != "v1\n" {
		t.Fatal("restore did not apply")
	}
}

func TestPendingBlob(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	a.write("f", "v1\n")
	a.sync(Options{Track: track("f")})
	blob := a.head("f").Entry.Blob
	file := filepath.Join(a.v.Dir(), filepath.FromSlash(vault.BlobPath(blob)))
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	pl := b.plan(Options{})
	it := b.item(pl, "f")
	wantAction(t, it, ActionPending)
	if it.NeedsResolution || !strings.Contains(it.Reason, "not available") {
		t.Fatalf("item = %+v", it)
	}
	rep := b.apply(pl, nil, Options{})
	noErrors(t, rep)
	if rep.Pending != 1 || b.exists("f") {
		t.Fatalf("report = %+v", rep)
	}
	if _, ok := b.base("f"); ok {
		t.Fatal("base written for a pending item")
	}
	// The blob arrives: the download proceeds.
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	pl = b.plan(Options{})
	wantAction(t, b.item(pl, "f"), ActionDownload)
	noErrors(t, b.apply(pl, nil, Options{}))
	if b.read("f") != "v1\n" {
		t.Fatal("not downloaded")
	}

	// A missing base blob makes a content conflict pending too.
	a.write("f", "v2\n")
	a.sync(Options{})
	b.write("f", "v3\n")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	wantAction(t, b.item(b.plan(Options{}), "f"), ActionPending)
}

func TestModesPushPull(t *testing.T) {
	t.Parallel()
	setup := func(t *testing.T) (*machine, *machine) {
		a, b := twoMachines(t)
		seed(a, b, map[string]string{"f": "f1\n", "g": "g1\n"})
		a.write("f", "f2\n")
		a.sync(Options{})
		b.write("g", "g2\n")
		return a, b
	}
	t.Run("push", func(t *testing.T) {
		_, b := setup(t)
		opts := Options{Mode: ModePush}
		pl := b.plan(opts)
		if pl.Mode != ModePush {
			t.Fatalf("plan mode = %v", pl.Mode)
		}
		f, g := b.item(pl, "f"), b.item(pl, "g")
		wantAction(t, f, ActionReportOnly)
		if f.Original != ActionDownload || !strings.Contains(f.Reason, "push mode") {
			t.Fatalf("f = %+v", f)
		}
		wantAction(t, g, ActionUpload)
		rep := b.apply(pl, nil, opts)
		noErrors(t, rep)
		if rep.Uploaded != 1 || rep.Skipped != 1 || rep.Downloaded != 0 {
			t.Fatalf("report = %+v", rep)
		}
		if b.read("f") != "f1\n" {
			t.Fatal("download applied in push mode")
		}
		if h := b.head("g"); h.Entry == nil || h.Entry.Machine != mB {
			t.Fatalf("g head = %+v", h.Entry)
		}
		s := Summarize(&pl.Projects[0])
		if s.LocalChanges != 1 || s.RemoteChanges != 1 {
			t.Fatalf("summary = %+v", s)
		}
	})
	t.Run("pull", func(t *testing.T) {
		a, b := setup(t)
		opts := Options{Mode: ModePull}
		pl := b.plan(opts)
		f, g := b.item(pl, "f"), b.item(pl, "g")
		wantAction(t, f, ActionDownload)
		wantAction(t, g, ActionReportOnly)
		if g.Original != ActionUpload || g.NeedsResolution {
			t.Fatalf("g = %+v", g)
		}
		rep := b.apply(pl, nil, opts)
		noErrors(t, rep)
		if rep.Downloaded != 1 || rep.Skipped != 1 || rep.Uploaded != 0 {
			t.Fatalf("report = %+v", rep)
		}
		if b.read("f") != "f2\n" {
			t.Fatal("download not applied in pull mode")
		}
		if h := b.head("g"); h.Entry == nil || h.Entry.Machine != mA {
			t.Fatalf("g head = %+v", h.Entry)
		}
		// Pull mode also turns delete-remote into report only.
		b.remove("f")
		b.sync(Options{}) // records the deletion? no: missing-local is report only
		if a.read("f") != "f2\n" {
			t.Fatal("A changed")
		}
		pl = b.plan(Options{Mode: ModePull, PropagateDeletes: true})
		it := b.item(pl, "f")
		wantAction(t, it, ActionReportOnly)
		if it.Original != ActionDeleteRemote || it.NeedsResolution {
			t.Fatalf("f = %+v", it)
		}
	})
	t.Run("push keeps trash-local report only", func(t *testing.T) {
		a, b := twoMachines(t)
		seed(a, b, map[string]string{"f": "v1\n"})
		if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"f"}); err != nil {
			t.Fatal(err)
		}
		opts := Options{Mode: ModePush}
		pl := b.plan(opts)
		it := b.item(pl, "f")
		wantAction(t, it, ActionReportOnly)
		if it.Original != ActionTrashLocal {
			t.Fatalf("f = %+v", it)
		}
		noErrors(t, b.apply(pl, nil, opts))
		if !b.exists("f") {
			t.Fatal("trashed in push mode")
		}
	})
	t.Run("conflicts are resolved in every mode", func(t *testing.T) {
		a, b := twoMachines(t)
		seed(a, b, map[string]string{".env": "A=1\n"})
		a.write(".env", "A=2\n")
		a.sync(Options{})
		b.write(".env", "A=3\n")
		opts := Options{Mode: ModePull, Strategy: StrategyLocal}
		pl := b.plan(opts)
		wantConflict(t, b.item(pl, ".env"), ConflictContent, true)
		res := Resolutions{}
		ApplyStrategy(pl, StrategyLocal, res)
		rep := b.apply(pl, res, opts)
		noErrors(t, rep)
		if rep.Resolved != 1 || rep.Uploaded != 1 {
			t.Fatalf("report = %+v", rep)
		}
		if h := b.head(".env"); h.Entry == nil || h.Entry.Machine != mB || len(h.Entry.Parents) != 2 {
			t.Fatalf("head = %+v", h.Entry)
		}
	})
}

func TestModeRestore(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{"f": "f1\n", "g": "g1\n", "h": "h1\n"})
	if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"h"}); err != nil {
		t.Fatal(err)
	}
	b.write("f", "local edit\n")
	b.remove("g")
	b.write("new", "untracked\n")
	opts := Options{Mode: ModeRestore}
	pl := b.plan(opts)
	f, g := b.item(pl, "f"), b.item(pl, "g")
	wantAction(t, f, ActionDownload)
	wantAction(t, g, ActionDownload)
	if !strings.Contains(f.Reason, "restore") {
		t.Fatalf("reason = %q", f.Reason)
	}
	noItem(t, pl, "h")   // tombstone heads are ignored
	noItem(t, pl, "new") // nothing without a head
	rep := b.apply(pl, nil, opts)
	noErrors(t, rep)
	if rep.Downloaded != 2 {
		t.Fatalf("report = %+v", rep)
	}
	if b.read("f") != "f1\n" || b.read("g") != "g1\n" || b.read("new") != "untracked\n" {
		t.Fatal("restore result wrong")
	}
	tr := b.trash()
	if len(tr) != 1 || tr[0].Path != "f" {
		t.Fatalf("trash = %+v, want the pre-image of f", tr)
	}
	if c, _, _ := b.st.TrashRead(b.v.Keys(), tr[0].ID); string(c) != "local edit\n" {
		t.Fatalf("trashed = %q", c)
	}
	// A second restore is a no-op.
	pl = b.plan(opts)
	wantAction(t, b.item(pl, "f"), ActionInSync)
	wantAction(t, b.item(pl, "g"), ActionInSync)
	// Restore with no base records the base.
	c := openMachine(t, a.v.Dir(), mC, "gamma")
	c.write("f", "f1\n")
	pl = c.plan(opts)
	wantAction(t, c.item(pl, "f"), ActionConverge)
	wantAction(t, c.item(pl, "g"), ActionDownload)
	noErrors(t, c.apply(pl, nil, opts))
	if c.read("g") != "g1\n" {
		t.Fatal("C restore failed")
	}
	if _, ok := c.base("f"); !ok {
		t.Fatal("C base for f missing")
	}
}

func TestApplyStrategy(t *testing.T) {
	t.Parallel()
	key := func(p string) ItemKey { return ItemKey{Project: projID, Path: p} }
	local := &FileRef{Kind: vault.KindFile, Blob: "l"}
	remoteFile := &FileRef{Kind: vault.KindFile, Blob: "r", Machine: mB}
	remoteGone := &FileRef{Kind: vault.KindDeleted, Machine: mB}
	newPlan := func() *Plan {
		return &Plan{Projects: []ProjectPlan{{ID: projID, Items: []Item{
			{Key: key("content"), Action: ActionConflict, Conflict: ConflictContent, NeedsResolution: true, Local: local},
			{Key: key("nobase"), Action: ActionConflict, Conflict: ConflictNoBase, NeedsResolution: true, Local: local},
			{Key: key("moddel"), Action: ActionConflict, Conflict: ConflictModifyDelete, NeedsResolution: true, Local: local},
			{Key: key("concurrent"), Action: ActionConflict, Conflict: ConflictConcurrent, NeedsResolution: true, Local: local, Remote: remoteFile},
			// Concurrent head the other machine deleted: "remote" is the deletion.
			{Key: key("concurrent-deleted"), Action: ActionConflict, Conflict: ConflictConcurrent, NeedsResolution: true, Local: local, Remote: remoteGone},
			// Several other machines disagree: "remote" names no side.
			{Key: key("concurrent-ambiguous"), Action: ActionConflict, Conflict: ConflictConcurrent, NeedsResolution: true, Local: local},
			// No local copy: "local" has nothing to keep.
			{Key: key("concurrent-nolocal"), Action: ActionConflict, Conflict: ConflictConcurrent, NeedsResolution: true, Remote: remoteFile},
			{Key: key("delete"), Action: ActionDeleteRemote, NeedsResolution: true},
			{Key: key("clean"), Action: ActionConflict, Conflict: ConflictContent, NeedsResolution: false, Merge: &merge.Result{Clean: true}, Local: local},
			{Key: key("upload"), Action: ActionUpload, Local: local},
			{Key: key("preset"), Action: ActionConflict, Conflict: ConflictContent, NeedsResolution: true, Local: local},
		}}}}
	}
	const needing = 9 // items with NeedsResolution
	tests := []struct {
		strategy Strategy
		want     map[string]ChoiceKind
	}{
		{StrategyAsk, map[string]ChoiceKind{}},
		{StrategyLocal, map[string]ChoiceKind{"content": ChooseLocal, "nobase": ChooseLocal, "moddel": ChooseKeep, "concurrent": ChooseLocal, "concurrent-deleted": ChooseLocal, "concurrent-ambiguous": ChooseLocal, "delete": ChooseConfirm}},
		{StrategyRemote, map[string]ChoiceKind{"content": ChooseRemote, "nobase": ChooseRemote, "moddel": ChooseDelete, "concurrent": ChooseRemote, "concurrent-deleted": ChooseDelete, "concurrent-nolocal": ChooseRemote, "delete": ChooseConfirm}},
		{StrategyAbort, map[string]ChoiceKind{"content": ChooseSkip, "nobase": ChooseSkip, "moddel": ChooseSkip, "concurrent": ChooseSkip, "concurrent-deleted": ChooseSkip, "concurrent-ambiguous": ChooseSkip, "concurrent-nolocal": ChooseSkip, "delete": ChooseSkip}},
	}
	for _, tc := range tests {
		t.Run(strategyName(tc.strategy), func(t *testing.T) {
			pl := newPlan()
			res := Resolutions{key("preset"): {Kind: ChooseCustom, Content: []byte("x")}}
			ApplyStrategy(pl, tc.strategy, res)
			if got := res[key("preset")]; got.Kind != ChooseCustom || string(got.Content) != "x" {
				t.Errorf("preset resolution replaced: %+v", got)
			}
			delete(res, key("preset"))
			if len(res) != len(tc.want) {
				t.Errorf("res = %+v, want %+v", res, tc.want)
			}
			for p, kind := range tc.want {
				if got, ok := res[key(p)]; !ok || got.Kind != kind {
					t.Errorf("%s: got %+v (present %v), want %v", p, got, ok, kind)
				}
			}
			unresolved := pl.Unresolved(res)
			wantUnresolved := needing - len(tc.want) // preset was deleted from res above
			if len(unresolved) != wantUnresolved {
				t.Errorf("unresolved = %v, want %d", unresolved, wantUnresolved)
			}
			for _, k := range unresolved {
				if _, ok := tc.want[k.Path]; ok {
					t.Errorf("%s resolved and unresolved", k.Path)
				}
			}
		})
	}
	// Nil inputs are harmless.
	ApplyStrategy(nil, StrategyLocal, Resolutions{})
	ApplyStrategy(newPlan(), StrategyLocal, nil)
}

func strategyName(s Strategy) string {
	return [...]string{"ask", "local", "remote", "abort"}[s]
}

func TestErrStale(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{"f": "v1\n", "g": "g1\n"})
	t.Run("upload", func(t *testing.T) {
		b.write("f", "v2\n")
		pl := b.plan(Options{})
		wantAction(t, b.item(pl, "f"), ActionUpload)
		b.write("f", "v3\n")
		rep := b.apply(pl, nil, Options{})
		if len(rep.Errors) != 1 || !errors.Is(rep.Errors[0].Err, ErrStale) || rep.Uploaded != 0 {
			t.Fatalf("report = %+v", rep)
		}
		if h := b.head("f"); h.Entry.Machine != mA {
			t.Fatal("journal written despite the stale item")
		}
		if bs, _ := b.base("f"); bs.Blob != a.head("f").Entry.Blob {
			t.Fatal("base changed")
		}
		b.sync(Options{}) // a fresh plan uploads v3
		if h := b.head("f"); h.Entry.Machine != mB {
			t.Fatal("re-plan did not upload")
		}
	})
	t.Run("download", func(t *testing.T) {
		a.sync(Options{})
		a.write("g", "g2\n")
		a.sync(Options{})
		pl := b.plan(Options{})
		wantAction(t, b.item(pl, "g"), ActionDownload)
		b.write("g", "local\n")
		rep := b.apply(pl, nil, Options{})
		if len(rep.Errors) != 1 || !errors.Is(rep.Errors[0].Err, ErrStale) {
			t.Fatalf("report = %+v", rep)
		}
		if b.read("g") != "local\n" || len(b.trash()) != 0 {
			t.Fatal("local edit overwritten")
		}
	})
	t.Run("trash-local vanished file is fine", func(t *testing.T) {
		a.write("h", "h1\n")
		a.sync(Options{Track: track("h")})
		b.sync(Options{})
		if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"h"}); err != nil {
			t.Fatal(err)
		}
		pl := b.plan(Options{})
		wantAction(t, b.item(pl, "h"), ActionTrashLocal)
		b.remove("h")
		rep := b.apply(pl, nil, Options{})
		noErrors(t, rep)
		if rep.Trashed != 1 {
			t.Fatalf("report = %+v", rep)
		}
		if bs, _ := b.base("h"); bs.Kind != vault.KindDeleted {
			t.Fatalf("base = %+v", bs)
		}
	})
}

func TestSummarize(t *testing.T) {
	t.Parallel()
	pp := &ProjectPlan{Items: []Item{
		{Action: ActionInSync},
		{Action: ActionUpload},
		{Action: ActionDeleteRemote},
		{Action: ActionDownload},
		{Action: ActionTrashLocal},
		{Action: ActionConverge},
		{Action: ActionUntrack},
		{Action: ActionConflict},
		{Action: ActionPending},
		{Action: ActionRollback},
		{Action: ActionMissingLocal},
		{Action: ActionReportOnly, Original: ActionDownload},
		{Action: ActionReportOnly, Original: ActionUpload},
		{Action: ActionReportOnly, Original: ActionMissingLocal}, // row 9
	}}
	got := Summarize(pp)
	want := Summary{InSync: 1, LocalChanges: 3, RemoteChanges: 5, Conflicts: 1, Pending: 1, Rollback: 1, Missing: 2}
	if got != want {
		t.Fatalf("Summarize = %+v, want %+v", got, want)
	}
	if Summarize(nil) != (Summary{}) {
		t.Fatal("nil summary")
	}
}

func TestValidPath(t *testing.T) {
	t.Parallel()
	tests := map[string]bool{
		"f": true, "dir/f": true, ".env": true, "a/b/c.txt": true,
		"": false, "/abs": false, "../x": false, "a/../b": false, "./a": false,
		"a//b": false, "a/": false, "a\\b": false, "a\x00b": false, ".": false, "..": false,
	}
	for p, want := range tests {
		if got := ValidPath(p); got != want {
			t.Errorf("ValidPath(%q) = %v, want %v", p, got, want)
		}
	}
}

func TestPlanWarningsAndSkips(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	t.Run("unknown project id", func(t *testing.T) {
		pl := a.plan(Options{Projects: []string{"nope"}})
		if len(pl.Projects) != 0 || !hasWarning(pl.Warnings, "not linked") {
			t.Fatalf("plan = %+v", pl)
		}
		pl = a.plan(Options{Projects: []string{projID, "myapp"}})
		if len(pl.Projects) != 1 {
			t.Fatalf("projects = %d", len(pl.Projects))
		}
	})
	t.Run("missing project path", func(t *testing.T) {
		a.cfg.Projects[0].Path = filepath.Join(t.TempDir(), "gone")
		defer func() { a.cfg.Projects[0].Path = a.dir }()
		pl := a.plan(Options{})
		if !pl.Projects[0].Missing || !hasWarning(pl.Projects[0].Warnings, "path missing") {
			t.Fatalf("plan = %+v", pl.Projects[0])
		}
		rep := a.apply(pl, nil, Options{})
		if len(rep.Errors) != 0 {
			t.Fatalf("errors = %v", rep.Errors)
		}
	})
	t.Run("too large file", func(t *testing.T) {
		a.cfg.Scan.MaxFileSize = "16"
		eng := New(a.v, a.st, a.cfg, nil, a.eng.machine)
		if eng.MaxFileSize() != 16 {
			t.Fatalf("MaxFileSize = %d", eng.MaxFileSize())
		}
		a.write("big", strings.Repeat("x", 17))
		pl, err := eng.Plan(context.Background(), Options{Track: track("big")})
		if err != nil {
			t.Fatal(err)
		}
		// Present but not comparable: reported, never uploaded.
		it := a.item(pl, "big")
		wantAction(t, it, ActionReportOnly)
		if it.Original != ActionUpload || it.Local == nil || it.Local.Blob != "" || it.Local.Size != 17 || it.LocalText != nil {
			t.Fatalf("item = %+v", it)
		}
		if !hasWarning(pl.Projects[0].Warnings, "larger than") {
			t.Fatalf("warnings = %v", pl.Projects[0].Warnings)
		}
		rep, err := eng.Apply(context.Background(), pl, nil, Options{})
		if err != nil || len(rep.Errors) != 0 || rep.Skipped != 1 || rep.Uploaded != 0 {
			t.Fatalf("report = %+v, %v", rep, err)
		}
		noItem(t, a.plan(Options{}), "big") // untracked and never uploaded
		a.cfg.Scan.MaxFileSize = ""
		a.remove("big")
	})
	t.Run("symlink and directory", func(t *testing.T) {
		a.write("target", "x\n")
		if err := os.Symlink(a.abs("target"), a.abs("link")); err != nil {
			t.Skip("symlinks unsupported")
		}
		if err := os.MkdirAll(a.abs("subdir"), 0o755); err != nil {
			t.Fatal(err)
		}
		pl := a.plan(Options{Track: track("link", "subdir", "target")})
		noItem(t, pl, "link")
		noItem(t, pl, "subdir")
		wantAction(t, a.item(pl, "target"), ActionUpload)
		if !hasWarning(pl.Projects[0].Warnings, "symbolic link") || !hasWarning(pl.Projects[0].Warnings, "not a regular file") {
			t.Fatalf("warnings = %v", pl.Projects[0].Warnings)
		}
		a.remove("link")
		a.remove("target")
		_ = os.Remove(a.abs("subdir"))
	})
	t.Run("invalid vault path is skipped", func(t *testing.T) {
		bad := vault.Entry{Path: "../escape", Kind: vault.KindFile, Blob: a.v.BlobID([]byte("x")), Clock: vault.Clock{mA: 1}, Machine: mA}
		j := &vault.Journal{Machine: mA, Entries: map[string]vault.Entry{"../escape": bad}}
		if err := a.v.WriteJournal(projID, j); err != nil {
			t.Fatal(err)
		}
		pl := b.plan(Options{})
		noItem(t, pl, "../escape")
		if !hasWarning(pl.Projects[0].Warnings, "invalid path") {
			t.Fatalf("warnings = %v", pl.Projects[0].Warnings)
		}
		if err := a.v.WriteJournal(projID, &vault.Journal{Machine: mA, Entries: map[string]vault.Entry{}}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("unreadable journal", func(t *testing.T) {
		seed(a, b, map[string]string{"f": "v1\n"})
		if err := os.WriteFile(a.journalFile(), []byte("garbage"), 0o600); err != nil {
			t.Fatal(err)
		}
		pl := b.plan(Options{})
		pp := pl.Projects[0]
		if !pp.Unreadable || len(pp.Items) != 0 || !hasWarning(pp.Warnings, "unreadable journal") {
			t.Fatalf("plan = %+v", pp)
		}
		rep := b.apply(pl, nil, Options{})
		if !emptyReport(rep) {
			t.Fatalf("report = %+v", rep)
		}
	})
	t.Run("nil plan and unready engine", func(t *testing.T) {
		if _, err := a.eng.Apply(context.Background(), nil, nil, Options{}); err == nil {
			t.Fatal("nil plan accepted")
		}
		var e *Engine
		if _, err := e.Plan(context.Background(), Options{}); err == nil {
			t.Fatal("nil engine planned")
		}
		if _, err := New(nil, nil, nil, nil, state.Machine{}).Plan(context.Background(), Options{}); err == nil {
			t.Fatal("engine without a vault planned")
		}
	})
}

func TestDuplicateDetection(t *testing.T) {
	t.Parallel()
	a, _ := twoMachines(t)
	fp := identity.Fingerprint{Kind: "git", Value: "github.com/me/app", Level: identity.LevelStrong}
	other := "fedcba9876543210"
	for _, id := range []string{projID, other} {
		if err := a.v.WriteProjectMeta(id, mA, vault.ProjectMeta{Name: "app", Fingerprints: []identity.Fingerprint{fp}, CreatedAt: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	pl := a.plan(Options{})
	if pl.Projects[0].DuplicateOf != other || !hasWarning(pl.Projects[0].Warnings, "duplicate of") {
		t.Fatalf("plan = %+v", pl.Projects[0])
	}
	// A dir-only match is not a duplicate.
	weak := identity.Fingerprint{Kind: "dir", Value: "app", Level: identity.LevelDir}
	if err := a.v.WriteProjectMeta(other, mA, vault.ProjectMeta{Name: "app", Fingerprints: []identity.Fingerprint{weak}}); err != nil {
		t.Fatal(err)
	}
	if err := a.v.WriteProjectMeta(projID, mA, vault.ProjectMeta{Name: "app", Fingerprints: []identity.Fingerprint{fp, weak}}); err != nil {
		t.Fatal(err)
	}
	if pl := a.plan(Options{}); pl.Projects[0].DuplicateOf != "" {
		t.Fatalf("DuplicateOf = %q", pl.Projects[0].DuplicateOf)
	}
}

func TestContextCancellation(t *testing.T) {
	t.Parallel()
	a, _ := twoMachines(t)
	a.write("f", "v1\n")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.eng.Plan(ctx, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Plan err = %v", err)
	}
	pl := a.plan(Options{Track: track("f")})
	if _, err := a.eng.Apply(ctx, pl, nil, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply err = %v", err)
	}
	if _, ok := a.heads()["f"]; ok {
		t.Fatal("journal written after cancellation")
	}
	if err := a.eng.Fetch(ctx, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("Fetch err = %v", err)
	}
	if err := a.eng.Untrack(ctx, projID, []string{"f"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Untrack err = %v", err)
	}
}

func TestProgressEvents(t *testing.T) {
	t.Parallel()
	a, _ := twoMachines(t)
	a.write("f", "v1\n")
	var events []Event
	opts := Options{Track: track("f"), Progress: func(ev Event) { events = append(events, ev) }}
	pl := a.plan(opts)
	noErrors(t, a.apply(pl, nil, opts))
	stages := map[string]int{}
	for _, ev := range events {
		stages[ev.Stage]++
	}
	if stages["plan"] < 2 || stages["apply"] < 2 {
		t.Fatalf("events = %+v", events)
	}
	var seen bool
	for _, ev := range events {
		if ev.Stage == "apply" && ev.Path == "f" && ev.Project == projID && ev.Total == 1 {
			seen = true
		}
	}
	if !seen {
		t.Fatalf("no per-item apply event: %+v", events)
	}
	if err := a.eng.Fetch(context.Background(), opts.Progress); err != nil {
		t.Fatal(err)
	}
	if err := a.eng.Push(context.Background(), opts.Progress); err != nil {
		t.Fatal(err)
	}
}

func TestMachineInfoAndTempCleanup(t *testing.T) {
	t.Parallel()
	a, _ := twoMachines(t)
	a.write("f", "v1\n")
	stale := a.abs("f") + ".psv-tmp-" + mA[:8]
	if err := os.WriteFile(stale, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	vaultStale := filepath.Join(a.v.Dir(), "blobs", "zz.psv-tmp-"+mA[:8])
	if err := os.MkdirAll(filepath.Dir(vaultStale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(vaultStale, []byte("junk"), 0o600); err != nil {
		t.Fatal(err)
	}
	a.sync(Options{Track: track("f")})
	for _, p := range []string{stale, vaultStale} {
		if _, err := os.Lstat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s still exists", p)
		}
	}
	ms, _, err := a.v.ListMachines()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].ID != mA || ms[0].Name != "alpha" || ms[0].Hostname != "alpha.local" || ms[0].LastSeen.IsZero() {
		t.Fatalf("machines = %+v", ms)
	}
	if a.eng.MachineID() != mA {
		t.Fatal("MachineID")
	}
}

func TestUntrackAndDeleteEverywhereErrors(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	ctx := context.Background()
	if err := a.eng.Untrack(ctx, projID, []string{"nope"}); !errors.Is(err, ErrNotTracked) {
		t.Fatalf("Untrack unknown: %v", err)
	}
	if err := a.eng.DeleteEverywhere(ctx, projID, []string{"../x"}); !errors.Is(err, ErrBadPath) {
		t.Fatalf("DeleteEverywhere bad path: %v", err)
	}
	if err := a.eng.Untrack(ctx, projID, nil); err != nil {
		t.Fatalf("empty list: %v", err)
	}
	if err := a.eng.Untrack(ctx, "", []string{"f"}); err == nil {
		t.Fatal("empty project accepted")
	}
	seed(a, b, map[string]string{"f": "v1\n", "g": "v1\n"})
	// A tracked and an unknown path: nothing is written.
	if err := a.eng.Untrack(ctx, projID, []string{"f", "nope"}); !errors.Is(err, ErrNotTracked) {
		t.Fatalf("err = %v", err)
	}
	if h := a.head("f"); h.Entry.Kind != vault.KindFile {
		t.Fatal("partial write")
	}
	// Base-only paths (no head) can be untracked too.
	if err := os.Remove(a.journalFile()); err != nil {
		t.Fatal(err)
	}
	if err := b.eng.Untrack(ctx, projID, []string{"f", "f"}); err != nil {
		t.Fatal(err)
	}
	if h := b.head("f"); h.Entry == nil || h.Entry.Kind != vault.KindUntracked || h.Entry.Machine != mB {
		t.Fatalf("head = %+v", h)
	}
	if _, ok := b.base("f"); ok {
		t.Fatal("base kept")
	}
	// DeleteEverywhere records parents and a tombstone base.
	if err := b.eng.DeleteEverywhere(ctx, projID, []string{"g"}); err != nil {
		t.Fatal(err)
	}
	h := b.head("g")
	if h.Entry == nil || h.Entry.Kind != vault.KindDeleted || len(h.Entry.Parents) != 1 {
		t.Fatalf("head = %+v", h.Entry)
	}
	if bs, ok := b.base("g"); !ok || bs.Kind != vault.KindDeleted || bs.Clock.Compare(h.Entry.Clock) != vault.Equal {
		t.Fatalf("base = %+v", bs)
	}
	tracked, err := b.eng.TrackedPaths(projID)
	if err != nil {
		t.Fatal(err)
	}
	if len(tracked) != 0 {
		t.Fatalf("tracked = %v", tracked)
	}
}

func TestTrackedPaths(t *testing.T) {
	t.Parallel()
	a, b := twoMachines(t)
	seed(a, b, map[string]string{"f": "v1\n", "g": "v1\n", "h": "v1\n"})
	if err := a.eng.Untrack(context.Background(), projID, []string{"g"}); err != nil {
		t.Fatal(err)
	}
	if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"h"}); err != nil {
		t.Fatal(err)
	}
	got, err := b.eng.TrackedPaths(projID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !got["f"] {
		t.Fatalf("tracked = %v", got)
	}
	// A concurrent head with a file candidate counts as tracked.
	b.write("f", "b\n")
	stale := b.plan(Options{})
	if err := a.eng.DeleteEverywhere(context.Background(), projID, []string{"f"}); err != nil {
		t.Fatal(err)
	}
	noErrors(t, b.apply(stale, nil, Options{}))
	if !b.head("f").Concurrent() {
		t.Fatal("not concurrent")
	}
	got, err = b.eng.TrackedPaths(projID)
	if err != nil || !got["f"] {
		t.Fatalf("tracked = %v, %v", got, err)
	}
	if _, err := b.eng.TrackedPaths(""); err == nil {
		t.Fatal("empty project accepted")
	}
	empty, err := b.eng.TrackedPaths("0000000000000000")
	if err != nil || len(empty) != 0 {
		t.Fatalf("unknown project: %v, %v", empty, err)
	}
}

func TestTrashPurgeOnApply(t *testing.T) {
	t.Parallel()
	a, _ := twoMachines(t)
	if _, err := a.st.TrashPut(a.v.Keys(), projID, "old", []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Age the entry by rewriting the index.
	entries := a.trash()
	if len(entries) != 1 {
		t.Fatal("no trash entry")
	}
	idx := filepath.Join(a.st.Dir(), "trash", "index.json")
	data, err := os.ReadFile(idx)
	if err != nil {
		t.Fatal(err)
	}
	stamp := entries[0].Time.Format(time.RFC3339Nano)
	oldStamp := entries[0].Time.Add(-40 * 24 * time.Hour).Format(time.RFC3339Nano)
	if !strings.Contains(string(data), stamp) {
		t.Skipf("trash index does not store RFC3339Nano timestamps: %s", data)
	}
	if err := os.WriteFile(idx, []byte(strings.Replace(string(data), stamp, oldStamp, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	a.write("f", "v1\n")
	a.sync(Options{Track: track("f")})
	if got := a.trash(); len(got) != 0 {
		t.Fatalf("trash after apply = %+v, want purged", got)
	}
}
