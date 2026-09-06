package vault

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression tests for the second review round on internal/vault: journal
// entry paths were not validated, Head.Candidates aliased the journals,
// a Close racing an operation surfaced crypto's not-ready error instead of
// ErrClosed, WriteBlob read the whole existing blob before comparing sizes,
// validBlobID accepted any hex string of length >= 2, and vault.json was fully
// decoded before its version was examined.

// ---- journal entry paths ----------------------------------------------------

// Journal entries are validated beyond Kind: the key must be a non-empty,
// relative, slash-separated project path and Entry.Path (when set) must equal
// it. WriteJournal refuses such a journal without touching it; readJournal
// refuses the sealed document as unreadable (whole project unreadable), so the
// sync engine can never be handed "../x" or "/abs" as a file to write.
func TestJournalEntryPathValidation(t *testing.T) {
	v, dir := newVault(t)
	rel := statePath(projA, mA)
	bad := []struct{ name, key, path string }{
		{"path field differs from key", "a.txt", "b.txt"},
		{"parent traversal", "../x", ""},
		{"parent traversal with matching path", "../x", "../x"},
		{"absolute", "/etc/passwd", ""},
		{"backslash", `a\b`, ""},
		{"empty key", "", ""},
		{"dot element", "./a", ""},
		{"dotdot element", "a/../b", ""},
		{"empty element", "a//b", ""},
		{"trailing slash", "a/", ""},
		{"nul byte", "a\x00b", ""},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			// WriteJournal: refused, nothing modified, nothing written.
			j := &Journal{Machine: mA, Seq: 3, Entries: map[string]Entry{
				"ok":   entry("ok", "ab", Clock{mA: 1}),
				tc.key: {Path: tc.path, Kind: KindFile, Blob: "cd", Clock: Clock{mA: 1}, Machine: mA},
			}}
			err := v.WriteJournal(projA, j)
			if !errors.Is(err, ErrBadEntryPath) || !strings.Contains(err.Error(), fmt.Sprintf("%q", tc.key)) {
				t.Errorf("WriteJournal: err = %v, want ErrBadEntryPath naming %q", err, tc.key)
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

			// readJournal: the same entry arriving from another machine.
			doc, err := json.Marshal(map[string]any{
				"machine": mA, "seq": 1,
				"entries": map[string]any{
					"ok":   map[string]any{"path": "ok", "kind": 0, "blob": "ab", "clock": map[string]any{mA: 1}, "machine": mA},
					tc.key: map[string]any{"path": tc.path, "kind": 0, "blob": "cd", "clock": map[string]any{mA: 1}, "machine": mA},
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
			if !errors.Is(err, ErrUnreadableJournal) || !errors.Is(err, ErrBadEntryPath) || !strings.Contains(err.Error(), rel) {
				t.Errorf("ReadJournal: err = %v, want ErrUnreadableJournal wrapping ErrBadEntryPath and naming %s", err, rel)
			}
			if js, _, err := v.ReadJournals(projA); !errors.Is(err, ErrUnreadableJournal) || js != nil {
				t.Errorf("ReadJournals = %v, %v; want ErrUnreadableJournal (whole project unreadable)", js, err)
			}
		})
	}

	// Well-formed paths of every shape are accepted; an unset Path is filled
	// from the key on the way out (in memory too) and on the way back.
	keys := []string{"dir/sub/.env", "spaces in name.txt", "ünïcode/файл.txt", ".hidden", "a.b.c", "deep/er/and/deeper/x"}
	j := &Journal{Machine: mA, Entries: map[string]Entry{}}
	for i, k := range keys {
		e := Entry{Kind: KindFile, Blob: "ab", Clock: Clock{mA: uint64(i + 1)}, Machine: mA}
		if i%2 == 0 {
			e.Path = k // explicitly set and matching
		}
		j.Entries[k] = e
	}
	if err := v.WriteJournal(projA, j); err != nil {
		t.Fatalf("WriteJournal with valid paths: %v", err)
	}
	for _, k := range keys {
		if j.Entries[k].Path != k {
			t.Errorf("after WriteJournal, in-memory entry %q has Path %q", k, j.Entries[k].Path)
		}
	}
	got, err := v.ReadJournal(projA, mA)
	if err != nil || got == nil || len(got.Entries) != len(keys) {
		t.Fatalf("ReadJournal = %+v, %v", got, err)
	}
	for _, k := range keys {
		if got.Entries[k].Path != k {
			t.Errorf("read back entry %q has Path %q", k, got.Entries[k].Path)
		}
	}
}

// checkEntry is the single rule set shared by the reader and the writer.
func TestCheckEntry(t *testing.T) {
	tests := []struct {
		name string
		key  string
		e    Entry
		want error
	}{
		{"ok", "a/b", Entry{Path: "a/b", Kind: KindFile, Blob: "ab"}, nil},
		{"ok empty path", "a/b", Entry{Kind: KindDeleted}, nil},
		{"unknown kind", "a", Entry{Kind: Kind(5)}, nil}, // an error, but not ErrBadEntryPath
		{"mismatch", "a", Entry{Path: "b", Kind: KindFile}, ErrBadEntryPath},
		{"bad key", "../a", Entry{Kind: KindFile}, ErrBadEntryPath},
		{"empty key", "", Entry{Kind: KindFile}, ErrBadEntryPath},
	}
	for _, tc := range tests {
		err := checkEntry(tc.key, tc.e)
		switch {
		case tc.name == "unknown kind":
			if err == nil || errors.Is(err, ErrBadEntryPath) || !strings.Contains(err.Error(), "kind 5") {
				t.Errorf("%s: err = %v", tc.name, err)
			}
		case tc.want == nil && err != nil:
			t.Errorf("%s: err = %v, want nil", tc.name, err)
		case tc.want != nil && !errors.Is(err, tc.want):
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	// Document paths keep their own sentinel.
	if err := checkDocPath("../x"); !errors.Is(err, ErrBadDocPath) || errors.Is(err, ErrBadEntryPath) {
		t.Errorf("checkDocPath: err = %v, want ErrBadDocPath only", err)
	}
	if err := checkEntryPath("../x"); !errors.Is(err, ErrBadEntryPath) || errors.Is(err, ErrBadDocPath) {
		t.Errorf("checkEntryPath: err = %v, want ErrBadEntryPath only", err)
	}
}

// ---- Head.Candidates aliasing ----------------------------------------------

// Candidates are detached from the journals they were read from, exactly
// like Entry: writing into a candidate's Clock map or Parents slice must not
// edit the journal, and a second ResolveHeads over the same journals must
// give the same answer.
func TestResolveHeadsCandidatesDetached(t *testing.T) {
	t1 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	js := journalsOf("f",
		he{mA, KindFile, "x", Clock{mA: 2}, []string{"p", "q"}, t1},
		he{mB, KindFile, "y", Clock{mB: 2}, []string{"p"}, t1},
	)
	h := ResolveHeads(js)["f"]
	if h.Entry != nil || len(h.Candidates) != 2 || h.Base != "p" {
		t.Fatalf("head = %+v, want two concurrent candidates with base p", h)
	}
	for i := range h.Candidates {
		c := &h.Candidates[i]
		c.Clock[mC] = 99
		c.Clock[c.Machine] = 0
		c.Parents[0] = "zzz"
		c.Parents = append(c.Parents, "extra")
	}
	for m, wantParents := range map[string][]string{mA: {"p", "q"}, mB: {"p"}} {
		e := js[m].Entries["f"]
		if !reflect.DeepEqual(e.Clock, Clock{m: 2}) {
			t.Errorf("mutating candidate %s changed the journal clock: %v", m, e.Clock)
		}
		if !reflect.DeepEqual(e.Parents, wantParents) {
			t.Errorf("mutating candidate %s changed the journal parents: %v", m, e.Parents)
		}
	}
	again := ResolveHeads(js)["f"]
	if again.Base != "p" || len(again.Candidates) != 2 {
		t.Errorf("second ResolveHeads = %+v, want the original result", again)
	}
	for _, c := range again.Candidates {
		if !reflect.DeepEqual(c.Clock, Clock{c.Machine: 2}) {
			t.Errorf("second ResolveHeads candidate %s clock = %v", c.Machine, c.Clock)
		}
	}

	// A resolved head's Entry is detached as well (it always was; guard it).
	single := journalsOf("g", he{mA, KindFile, "x", Clock{mA: 1}, []string{"p"}, t1})
	e := ResolveHeads(single)["g"].Entry
	e.Clock[mA] = 42
	e.Parents[0] = "zzz"
	if got := single[mA].Entries["g"]; got.Clock[mA] != 1 || got.Parents[0] != "p" {
		t.Errorf("mutating Entry changed the journal: %+v", got)
	}

	// nil stays nil: detaching never invents empty maps or slices.
	bare := map[string]*Journal{mA: {Machine: mA, Entries: map[string]Entry{"d": {Kind: KindDeleted}}}}
	bh := ResolveHeads(bare)["d"]
	if bh.Candidates[0].Clock != nil || bh.Candidates[0].Parents != nil {
		t.Errorf("nil Clock/Parents became non-nil: %+v", bh.Candidates[0])
	}
}

// The entries map key is the path: Head.Path, Entry.Path and every
// candidate's Path follow it, even for an in-memory journal whose Path field
// disagrees (on disk, readJournal refuses such a journal).
func TestResolveHeadsPathFollowsKey(t *testing.T) {
	js := map[string]*Journal{
		mA: {Machine: mA, Entries: map[string]Entry{"real": {Path: "other", Kind: KindFile, Blob: "x", Clock: Clock{mA: 1}}}},
		mB: {Machine: mB, Entries: map[string]Entry{"real": {Path: "", Kind: KindFile, Blob: "y", Clock: Clock{mB: 1}}}},
	}
	heads := ResolveHeads(js)
	if _, stray := heads["other"]; stray || len(heads) != 1 {
		t.Fatalf("heads = %v, want only \"real\"", heads)
	}
	h := heads["real"]
	if h.Path != "real" || len(h.Candidates) != 2 {
		t.Fatalf("head = %+v", h)
	}
	for _, c := range h.Candidates {
		if c.Path != "real" {
			t.Errorf("candidate %s Path = %q, want the key", c.Machine, c.Path)
		}
	}
	if js[mA].Entries["real"].Path != "other" {
		t.Error("ResolveHeads modified the journal's Path field")
	}
}

// ---- ErrClosed for operations racing Close -----------------------------------

// closedOr maps the key set's not-ready error to ErrClosed only when the
// vault is closed; other errors and nil pass through untouched.
func TestClosedOrMapsKeyErrors(t *testing.T) {
	v, _ := newVault(t)
	plain := errors.New("plain")
	if got := v.closedOr(plain); got != plain {
		t.Errorf("open vault: closedOr(plain) = %v, want the same error", got)
	}
	if got := v.closedOr(nil); got != nil {
		t.Errorf("open vault: closedOr(nil) = %v", got)
	}
	keys := v.Keys()
	v.Close()
	_, _, kerr := keys.SealBlob([]byte("x")) // crypto's own not-ready error
	if kerr == nil {
		t.Fatal("SealBlob on zeroed keys succeeded")
	}
	got := v.closedOr(kerr)
	if !errors.Is(got, ErrClosed) || !errors.Is(got, kerr) {
		t.Errorf("closed vault: closedOr = %v, want ErrClosed wrapping %v", got, kerr)
	}
	if got := v.closedOr(nil); got != nil {
		t.Errorf("closed vault: closedOr(nil) = %v", got)
	}
	var nv *Vault
	if got := nv.closedOr(plain); !errors.Is(got, ErrClosed) {
		t.Errorf("nil vault: closedOr = %v, want ErrClosed", got)
	}
}

// Whatever an operation racing Close returns, it is ErrClosed: never crypto's
// unexported not-ready error, never ErrBlobMissing or ErrUnreadableJournal
// for a blob or journal that is perfectly fine. The window between ready()
// and the key operation is hit often enough under -race; the assertion holds
// whether or not it is.
func TestCloseRacingOperationsReportErrClosed(t *testing.T) {
	for round := 0; round < 4; round++ {
		v, _ := newVault(t)
		id, _, err := v.WriteBlob([]byte("seed"))
		if err != nil {
			t.Fatal(err)
		}
		if err := v.WriteJournal(projA, &Journal{Machine: mA, Entries: map[string]Entry{"f": entry("f", id, Clock{mA: 1})}}); err != nil {
			t.Fatal(err)
		}
		if err := v.WriteProjectMeta(projA, mA, ProjectMeta{Name: "p", CreatedAt: time.Unix(1, 0)}); err != nil {
			t.Fatal(err)
		}
		if err := v.WriteMachine(MachineInfo{ID: mA, Name: "m"}); err != nil {
			t.Fatal(err)
		}
		doc, err := v.SealDoc("x/y", []byte("d"))
		if err != nil {
			t.Fatal(err)
		}

		const workers = 10
		var wg sync.WaitGroup
		errs := make(chan error, workers)
		start := make(chan struct{})
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				for n := 0; ; n++ {
					var err error
					switch n % 10 {
					case 0:
						_, _, err = v.WriteBlob([]byte(fmt.Sprintf("w%d-%d", i, n)))
					case 1:
						_, err = v.ReadBlob(id)
					case 2:
						_, err = v.SealDoc("x/y", []byte("d"))
					case 3:
						_, err = v.OpenDoc("x/y", doc)
					case 4:
						_, err = v.ReadJournal(projA, mA)
					case 5:
						err = v.WriteJournal(projA, &Journal{Machine: mA, Entries: map[string]Entry{"f": entry("f", id, Clock{mA: uint64(n + 2)})}})
					case 6:
						_, _, err = v.ReadJournals(projA)
					case 7:
						_, _, err = v.ReadProject(projA)
					case 8:
						_, _, err = v.ListMachines()
					case 9:
						_, _, err = v.ListProjects()
					}
					if err != nil {
						errs <- err
						return
					}
				}
			}(i)
		}
		close(start)
		time.Sleep(time.Duration(round+1) * time.Millisecond)
		v.Close()
		wg.Wait()
		close(errs)
		for err := range errs {
			if !errors.Is(err, ErrClosed) {
				t.Errorf("round %d: operation racing Close returned %v, want ErrClosed", round, err)
			}
			for _, wrong := range []error{ErrBlobMissing, ErrUnreadableJournal, ErrNoProject} {
				if errors.Is(err, wrong) {
					t.Errorf("round %d: closed vault reported %v: %v", round, wrong, err)
				}
			}
		}
	}
}

// ---- WriteBlob dedupe -----------------------------------------------------------

// The dedupe check stats the existing file first and reads it only when the
// size already matches: an absent, truncated or over-long file is never
// loaded into memory just to learn that it differs.
func TestWriteBlobDedupeStatsBeforeReading(t *testing.T) {
	v, dir := newVault(t)
	var (
		mu    sync.Mutex
		reads int
	)
	orig := blobReadFile
	blobReadFile = func(p string, max int64) ([]byte, error) {
		mu.Lock()
		reads++
		mu.Unlock()
		return orig(p, max)
	}
	t.Cleanup(func() { blobReadFile = orig })
	count := func() int {
		mu.Lock()
		defer mu.Unlock()
		n := reads
		reads = 0
		return n
	}

	pt := []byte("dedupe me, but cheaply")
	id, created, err := v.WriteBlob(pt)
	if err != nil || !created {
		t.Fatalf("first WriteBlob: created=%v err=%v", created, err)
	}
	if n := count(); n != 0 {
		t.Errorf("absent blob: %d reads before writing, want 0", n)
	}
	rel := BlobPath(id)
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	ct := mustRead(t, abs)

	if _, created, err := v.WriteBlob(pt); err != nil || created {
		t.Fatalf("identical WriteBlob: created=%v err=%v", created, err)
	}
	if n := count(); n != 1 {
		t.Errorf("identical blob: %d reads, want exactly 1", n)
	}

	for name, existing := range map[string][]byte{
		"truncated": ct[:len(ct)-1],
		"longer":    append(append([]byte(nil), ct...), 0),
		"empty":     {},
	} {
		writeRaw(t, dir, rel, existing)
		count()
		_, created, err := v.WriteBlob(pt)
		if err != nil || !created {
			t.Errorf("%s: WriteBlob created=%v err=%v, want a rewrite", name, created, err)
		}
		if n := count(); n != 0 {
			t.Errorf("%s: existing file of a different size was read %d times, want 0", name, n)
		}
		if !bytes.Equal(mustRead(t, abs), ct) {
			t.Errorf("%s: blob not healed", name)
		}
	}

	// Same size, different bytes: the only case that needs the read.
	flipped := append([]byte(nil), ct...)
	flipped[len(flipped)-1] ^= 0x01
	writeRaw(t, dir, rel, flipped)
	count()
	if _, created, err := v.WriteBlob(pt); err != nil || !created {
		t.Errorf("same-size corrupt: WriteBlob created=%v err=%v, want a rewrite", created, err)
	}
	if n := count(); n != 1 {
		t.Errorf("same-size corrupt: %d reads, want exactly 1", n)
	}
	if got, err := v.ReadBlob(id); err != nil || !bytes.Equal(got, pt) {
		t.Errorf("ReadBlob after heal = %q, %v", got, err)
	}
}

// ---- blob id length ---------------------------------------------------------------

// A blob id is exactly BlobIDLen (64) lowercase hex characters, what
// hex(HMAC-SHA256) produces; shorter hex strings never name a blob, so
// BlobPath returns "", HasBlob is false and ReadBlob fails as a bad id, even
// when a file exists at the path the old rule would have probed.
func TestBlobIDLength(t *testing.T) {
	v, dir := newVault(t)
	if BlobIDLen != 64 {
		t.Fatalf("BlobIDLen = %d, want 64", BlobIDLen)
	}
	id, _, err := v.WriteBlob([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != BlobIDLen || !validBlobID(id) || BlobPath(id) == "" {
		t.Errorf("real id %q (len %d) is not accepted", id, len(id))
	}

	writeRaw(t, dir, "blobs/ab/ab.enc", []byte("planted"))
	writeRaw(t, dir, "blobs/ab/abcdef0123.enc", []byte("planted"))
	for _, bad := range []string{
		"ab", "abcdef0123", strings.Repeat("a", 63), strings.Repeat("a", 65),
		strings.Repeat("A", 64), strings.Repeat("a", 63) + "g", "",
	} {
		if validBlobID(bad) {
			t.Errorf("validBlobID(%q) = true", bad)
		}
		if got := BlobPath(bad); got != "" {
			t.Errorf("BlobPath(%q) = %q, want \"\"", bad, got)
		}
		if v.HasBlob(bad) {
			t.Errorf("HasBlob(%q) = true", bad)
		}
		_, err := v.ReadBlob(bad)
		if !errors.Is(err, ErrBlobMissing) || !errors.Is(err, ErrBadBlobID) {
			t.Errorf("ReadBlob(%q): err = %v, want ErrBlobMissing wrapping ErrBadBlobID", bad, err)
		}
	}
	if !v.HasBlob(id) {
		t.Error("HasBlob(real id) = false")
	}
}

// ---- vault.json version before shape ------------------------------------------

// The version is examined before the rest of vault.json is decoded: a newer
// format that changes the shape of kdf, wrapped_key or anything else is
// refused as ErrNewerVersion (spec §13), not reported as tampering, by both
// Open and ReadID, and without running the KDF.
func TestOpenNewerVersionBeforeShape(t *testing.T) {
	newer := map[string]func(m map[string]any){
		"kdf is a string":          func(m map[string]any) { m["kdf"] = "argon3" },
		"wrapped_key is an object": func(m map[string]any) { m["wrapped_key"] = map[string]any{"alg": "kem", "ct": "..."} },
		"id is a number":           func(m map[string]any) { m["id"] = 42 },
		"created_at not a time":    func(m map[string]any) { m["created_at"] = "yesterday" },
		"only a version left": func(m map[string]any) {
			for k := range m {
				delete(m, k)
			}
		},
		"nothing else valid": func(m map[string]any) {
			m["id"] = ""
			m["kdf"] = map[string]any{"algo": "scrypt", "memory": uint32(1) << 31}
			m["wrapped_key"] = "AAAA"
		},
	}
	for name, mutate := range newer {
		for _, bump := range []int{Version + 1, Version + 100} {
			t.Run(fmt.Sprintf("%s/version %d", name, bump), func(t *testing.T) {
				_, dir := newVault(t)
				tamper(t, dir, func(m map[string]any) {
					mutate(m)
					m["version"] = bump
				})
				start := time.Now()
				_, err := Open(dir, pass(), mA)
				if !errors.Is(err, ErrNewerVersion) || errors.Is(err, ErrInvalidVault) {
					t.Errorf("Open: err = %v, want ErrNewerVersion (not ErrInvalidVault)", err)
				}
				if elapsed := time.Since(start); elapsed > time.Second {
					t.Errorf("Open took %v; the KDF ran", elapsed)
				}
				if _, err := ReadID(dir); !errors.Is(err, ErrNewerVersion) {
					t.Errorf("ReadID: err = %v, want ErrNewerVersion", err)
				}
			})
		}
		// The same shape at the current version is tampering.
		t.Run(name+"/current version", func(t *testing.T) {
			_, dir := newVault(t)
			tamper(t, dir, mutate)
			if _, err := Open(dir, pass(), mA); !errors.Is(err, ErrInvalidVault) || errors.Is(err, ErrNewerVersion) {
				t.Errorf("Open: err = %v, want ErrInvalidVault", err)
			}
		})
	}

	// A version field of the wrong type is tampering, not a newer format.
	for name, version := range map[string]any{"string": "2", "float": 1.5, "object": map[string]any{}, "null": nil} {
		t.Run("version "+name, func(t *testing.T) {
			_, dir := newVault(t)
			tamper(t, dir, func(m map[string]any) { m["version"] = version })
			_, err := Open(dir, pass(), mA)
			if errors.Is(err, ErrNewerVersion) || err == nil {
				t.Errorf("Open: err = %v, want a refusal that is not ErrNewerVersion", err)
			}
			if version != nil && !errors.Is(err, ErrInvalidVault) {
				t.Errorf("Open: err = %v, want ErrInvalidVault", err)
			}
		})
	}
}
