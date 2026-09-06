package vault

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Regression tests for the fourth review round on internal/vault: WriteBlob
// did not record a blob that was already on disk (so the transport never
// pushed it), and checkEntry accepted a live file entry with no blob (which
// the sync engine reads as empty content and writes over the local file).

// ---- WriteBlob records deduped blobs ---------------------------------------

// Written() is the only list the transport pushes blobs from, so it must name
// every blob this session's journals can reference — including one an earlier,
// interrupted run already left on disk. Before the fix WriteBlob returned
// early on the dedupe path without recording, so a sync resumed after a
// cancelled Apply published a journal entry pointing at a blob the remote had
// never received, and every other machine saw that path as pending forever.
func TestWriteBlobRecordsDedupedBlob(t *testing.T) {
	v, dir := newVault(t)
	pt := []byte("hello f1")

	// First run: the blob is created and recorded.
	id, created, err := v.WriteBlob(pt)
	if err != nil || !created {
		t.Fatalf("first WriteBlob: id=%s created=%v err=%v", id, created, err)
	}
	rel := BlobPath(id)
	if !hasWarning(v.Written(), rel) {
		t.Fatalf("first Written() = %v, want it to name %s", v.Written(), rel)
	}
	// ...and then the process dies before the journal referencing it is
	// written, so nothing was ever pushed.
	v.Close()

	// Second run (a fresh handle, as a new process): the blob is already on
	// disk, so nothing is created — but the journal written now names it, so
	// Written() must still list it for the push.
	v2 := openAs(t, dir, mA)
	id2, created2, err := v2.WriteBlob(pt)
	if err != nil {
		t.Fatalf("second WriteBlob: %v", err)
	}
	if id2 != id || created2 {
		t.Errorf("second WriteBlob: id=%s created=%v, want %s, false", id2, created2, id)
	}
	if !hasWarning(v2.Written(), rel) {
		t.Fatalf("second Written() = %v, want it to name the deduped blob %s", v2.Written(), rel)
	}

	// The invariant the transport depends on: every blob named by the journal
	// this run publishes appears in Written().
	j := &Journal{Machine: mA, Entries: map[string]Entry{"f1.txt": entry("f1.txt", id, Clock{mA: 1})}}
	if err := v2.WriteJournal(projA, j); err != nil {
		t.Fatalf("WriteJournal: %v", err)
	}
	written := v2.Written()
	for _, e := range j.Entries {
		if !hasWarning(written, BlobPath(e.Blob)) {
			t.Errorf("Written() = %v does not name %s, referenced by the journal just published", written, BlobPath(e.Blob))
		}
	}
	if !hasWarning(written, statePath(projA, mA)) {
		t.Errorf("Written() = %v does not name the journal", written)
	}

	// Recording is deduped: a third write of the same plaintext adds nothing.
	before := len(v2.Written())
	if _, _, err := v2.WriteBlob(pt); err != nil {
		t.Fatalf("third WriteBlob: %v", err)
	}
	if got := len(v2.Written()); got != before {
		t.Errorf("Written() grew from %d to %d entries on a repeated blob", before, got)
	}
}

// A blob whose write fails is not recorded: Written() must never name a path
// that is not on disk, or the transport would try to push a missing file.
func TestWriteBlobFailedWriteNotRecorded(t *testing.T) {
	v, dir := newVault(t)
	pt := []byte("unwritable")
	id := v.BlobID(pt)
	rel := BlobPath(id)

	// Make the blob's directory a file so the atomic write cannot succeed.
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(filepath.Dir(abs)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Dir(abs), []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := v.WriteBlob(pt); err == nil {
		t.Fatalf("WriteBlob into a non-directory succeeded")
	}
	if hasWarning(v.Written(), rel) {
		t.Errorf("Written() = %v names a blob that was never written", v.Written())
	}
}

// ---- entry Blob / Kind agreement -------------------------------------------

// checkEntry must reject a live file entry with no blob and a tombstone that
// carries one. A KindFile head with an empty blob is not an empty file: head
// resolution hands it to the sync engine as content, which overwrites the
// local file with zero bytes and keeps no trash pre-image. WriteJournal
// refuses to publish such an entry and readJournal reports the whole journal
// as unreadable, exactly as it does for an unknown kind.
func TestJournalEntryBlobKindValidation(t *testing.T) {
	v, dir := newVault(t)
	rel := statePath(projA, mA)
	bad := []struct {
		name string
		kind Kind
		blob string
		want string
	}{
		{"file without blob", KindFile, "", "has no blob"},
		{"deleted with blob", KindDeleted, "ab", "has blob"},
		{"untracked with blob", KindUntracked, "ab", "has blob"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			// WriteJournal: refused, nothing modified, nothing written.
			j := &Journal{Machine: mA, Seq: 3, Entries: map[string]Entry{
				"ok":     entry("ok", "ab", Clock{mA: 1}),
				"f.txt":  {Path: "f.txt", Kind: tc.kind, Blob: tc.blob, Clock: Clock{mA: 1}, Machine: mA},
				"other":  entry("other", "cd", Clock{mA: 1}),
				"nested": entry("nested", "ef", Clock{mA: 1}),
			}}
			err := v.WriteJournal(projA, j)
			if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"f.txt"`) {
				t.Errorf("WriteJournal: err = %v, want a refusal naming \"f.txt\" and %q", err, tc.want)
			}
			if j.Seq != 3 || !j.UpdatedAt.IsZero() {
				t.Errorf("failed WriteJournal modified the journal: seq=%d updated=%v", j.Seq, j.UpdatedAt)
			}
			if got, err := v.ReadJournal(projA, mA); err != nil || got != nil {
				t.Errorf("ReadJournal after refused write = %+v, %v; want nil, nil", got, err)
			}
			if hasWarning(v.Written(), rel) {
				t.Errorf("Written() = %v lists the refused journal", v.Written())
			}

			// readJournal: the same entry arriving from another machine. The
			// blob key is omitted entirely for the empty case, which is what
			// `json:"blob,omitempty"` produces.
			e := map[string]any{"path": "f.txt", "kind": int(tc.kind), "clock": map[string]any{mA: 1}, "machine": mA}
			if tc.blob != "" {
				e["blob"] = tc.blob
			}
			doc, err := json.Marshal(map[string]any{
				"machine": mA, "seq": 1,
				"entries": map[string]any{
					"ok":    map[string]any{"path": "ok", "kind": 0, "blob": "ab", "clock": map[string]any{mA: 1}, "machine": mA},
					"f.txt": e,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			ct, err := v.SealDoc(rel, doc)
			if err != nil {
				t.Fatal(err)
			}
			writeRaw(t, dir, rel, ct)
			t.Cleanup(func() { _ = os.Remove(filepath.Join(dir, filepath.FromSlash(rel))) })
			_, err = v.ReadJournal(projA, mA)
			if !errors.Is(err, ErrUnreadableJournal) || !strings.Contains(err.Error(), rel) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("ReadJournal: err = %v, want ErrUnreadableJournal naming %s and %q", err, rel, tc.want)
			}
			// The whole project is unreadable rather than silently reduced to
			// a truncating head.
			if js, _, err := v.ReadJournals(projA); !errors.Is(err, ErrUnreadableJournal) || js != nil {
				t.Errorf("ReadJournals = %v, %v; want ErrUnreadableJournal (whole project unreadable)", js, err)
			}
		})
	}

	// The legitimate shapes still round-trip: a file with a blob, and both
	// tombstone kinds without one.
	empty, _, err := v.WriteBlob(nil)
	if err != nil {
		t.Fatalf("WriteBlob(nil): %v", err)
	}
	good := &Journal{Machine: mA, Entries: map[string]Entry{
		"file":    entry("file", "ab", Clock{mA: 1}),
		"empty":   entry("empty", empty, Clock{mA: 1}), // a real zero-byte file has a real blob id
		"gone":    {Path: "gone", Kind: KindDeleted, Clock: Clock{mA: 1}, Machine: mA},
		"ignored": {Path: "ignored", Kind: KindUntracked, Clock: Clock{mA: 1}, Machine: mA},
	}}
	if err := v.WriteJournal(projA, good); err != nil {
		t.Fatalf("WriteJournal(valid): %v", err)
	}
	got, err := v.ReadJournal(projA, mA)
	if err != nil {
		t.Fatalf("ReadJournal(valid): %v", err)
	}
	for k, want := range good.Entries {
		if g := got.Entries[k]; g.Kind != want.Kind || g.Blob != want.Blob {
			t.Errorf("entry %q read back as %s/%q, want %s/%q", k, g.Kind, g.Blob, want.Kind, want.Blob)
		}
	}
	if len(got.Entries) != len(good.Entries) {
		t.Errorf("read back %d entries, want %d", len(got.Entries), len(good.Entries))
	}
	if _, ok := got.Entries["empty"]; !ok {
		t.Error("the zero-byte file entry was dropped")
	}
}
