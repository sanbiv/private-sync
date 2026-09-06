package vault

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/fsutil"
)

// Regression tests for the review findings on internal/vault: document writes
// were not serialised, readJournal hid the underlying cause with %v, unknown
// entry kinds were accepted, and Head.Candidates collapsed Equal clocks.

// Concurrent document writes through one handle share a temp file name
// (<target>.psv-tmp-<writer suffix>, per handle rather than per call), exactly
// like blobs before blobLocks: without serialisation two WriteJournal calls
// for the same project open the same temp file with O_TRUNC and either
// interleave their bytes (an undecryptable journal makes the whole project
// unreadable) or lose the rename race with ENOENT. Every write must land
// intact, whichever one wins.
func TestWriteJournalConcurrent(t *testing.T) {
	v, dir := newVault(t)
	const (
		workers = 24
		perJob  = 200
	)
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
	)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// Every journal is large enough that a torn write is not a
			// benign prefix, and every worker's entries are distinct so a
			// mixed result would be detectable even if it decrypted.
			j := &Journal{Machine: mA, Entries: make(map[string]Entry, perJob)}
			blob := strings.Repeat(fmt.Sprintf("%02x", i), 32)
			for n := 0; n < perJob; n++ {
				p := fmt.Sprintf("worker%02d/file%03d.txt", i, n)
				j.Entries[p] = entry(p, blob, Clock{mA: uint64(n + 1)})
			}
			err := v.WriteJournal(projA, j)
			if err == nil {
				err = v.WriteProjectMeta(projA, mA, ProjectMeta{Name: fmt.Sprintf("meta %02d", i), CreatedAt: time.Unix(int64(i), 0).UTC()})
			}
			if err == nil {
				err = v.WriteMachine(MachineInfo{ID: mA, Name: fmt.Sprintf("host %02d", i)})
			}
			mu.Lock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("concurrent document writes: %v", firstErr)
	}

	j, err := v.ReadJournal(projA, mA)
	if err != nil || j == nil {
		t.Fatalf("ReadJournal after concurrent writes: %+v, %v", j, err)
	}
	if j.Seq != 1 || len(j.Entries) != perJob {
		t.Errorf("journal seq=%d entries=%d, want one intact journal (seq 1, %d entries)", j.Seq, len(j.Entries), perJob)
	}
	prefixes := map[string]struct{}{}
	for p := range j.Entries {
		prefixes[strings.SplitN(p, "/", 2)[0]] = struct{}{}
	}
	if len(prefixes) != 1 {
		t.Errorf("journal mixes entries of %d workers: %v", len(prefixes), prefixes)
	}
	if _, _, err := v.ReadJournals(projA); err != nil {
		t.Errorf("ReadJournals: %v", err)
	}
	if m, err := v.ReadProjectMeta(projA, mA); err != nil || m == nil || !strings.HasPrefix(m.Name, "meta ") {
		t.Errorf("ReadProjectMeta = %+v, %v", m, err)
	}
	ms, warnings, err := v.ListMachines()
	if err != nil || len(ms) != 1 || !strings.HasPrefix(ms[0].Name, "host ") || len(warnings) != 0 {
		t.Errorf("ListMachines = %+v, %v, %v", ms, warnings, err)
	}

	// Nothing is left behind: a lost rename race leaves no temp file either
	// way, but a write that failed mid-way would.
	for _, rel := range []string{path.Join(projectDir(projA), StateDir), path.Join(projectDir(projA), MetaDir), MachinesDir} {
		entries, err := os.ReadDir(filepath.Join(dir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("ReadDir %s: %v", rel, err)
		}
		for _, e := range entries {
			if fsutil.IsTemp(e.Name()) {
				t.Errorf("%s: temp file %q left behind", rel, e.Name())
			}
		}
		if len(entries) != 1 {
			t.Errorf("%s holds %d files, want exactly one", rel, len(entries))
		}
	}

	// Written lists each document once, however many times it was written.
	written := v.Written()
	counts := map[string]int{}
	for _, w := range written {
		counts[w]++
	}
	for _, rel := range []string{statePath(projA, mA), metaPath(projA, mA), machinePath(mA)} {
		if counts[rel] != 1 {
			t.Errorf("Written() lists %s %d times, want once", rel, counts[rel])
		}
	}
	if len(written) != 4 {
		t.Errorf("Written() = %v, want vault.json plus the three documents", written)
	}
}

// readJournal keeps the underlying cause in the error chain (double %w, as
// ReadBlob does), so callers can tell a tampered journal (crypto.ErrAuth) from
// a file they may not read (fs.ErrPermission) while still matching
// ErrUnreadableJournal and seeing the file name.
func TestReadJournalErrorChain(t *testing.T) {
	v, dir := newVault(t)
	if err := v.WriteJournal(projA, &Journal{Machine: mA, Entries: map[string]Entry{"f": entry("f", "ab", Clock{mA: 1})}}); err != nil {
		t.Fatal(err)
	}
	rel := statePath(projA, mA)
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	ct := mustRead(t, abs)

	tampered := append([]byte(nil), ct...)
	tampered[len(tampered)-1] ^= 0x01 // inside the Poly1305 tag
	writeRaw(t, dir, rel, tampered)
	_, err := v.ReadJournal(projA, mA)
	if !errors.Is(err, ErrUnreadableJournal) || !errors.Is(err, crypto.ErrAuth) || !strings.Contains(err.Error(), rel) {
		t.Errorf("tampered journal: err = %v, want ErrUnreadableJournal wrapping crypto.ErrAuth and naming %s", err, rel)
	}
	if _, _, err := v.ReadJournals(projA); !errors.Is(err, ErrUnreadableJournal) || !errors.Is(err, crypto.ErrAuth) {
		t.Errorf("ReadJournals tampered: err = %v, want ErrUnreadableJournal wrapping crypto.ErrAuth", err)
	}

	// Valid ciphertext, invalid JSON: the decode error is in the chain too.
	notJSON, err := v.SealDoc(rel, []byte("{not json"))
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, rel, notJSON)
	var syntax interface{ Error() string }
	if _, err := v.ReadJournal(projA, mA); !errors.Is(err, ErrUnreadableJournal) || errors.Is(err, crypto.ErrAuth) || !errors.As(err, &syntax) {
		t.Errorf("non-json journal: err = %v, want ErrUnreadableJournal without crypto.ErrAuth", err)
	}

	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("file permissions are not enforced here")
	}
	writeRaw(t, dir, rel, ct)
	if err := os.Chmod(abs, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(abs, 0o600) })
	_, err = v.ReadJournal(projA, mA)
	if !errors.Is(err, ErrUnreadableJournal) || !errors.Is(err, fs.ErrPermission) || !strings.Contains(err.Error(), rel) {
		t.Errorf("unreadable journal file: err = %v, want ErrUnreadableJournal wrapping fs.ErrPermission and naming %s", err, rel)
	}
	if _, _, err := v.ReadJournals(projA); !errors.Is(err, fs.ErrPermission) {
		t.Errorf("ReadJournals unreadable file: err = %v, want fs.ErrPermission in the chain", err)
	}
}

func TestKindValid(t *testing.T) {
	for _, tc := range []struct {
		kind Kind
		want bool
	}{
		{KindFile, true}, {KindDeleted, true}, {KindUntracked, true},
		{Kind(-1), false}, {Kind(3), false}, {Kind(7), false},
	} {
		if got := tc.kind.Valid(); got != tc.want {
			t.Errorf("Kind(%d).Valid() = %v, want %v", int(tc.kind), got, tc.want)
		}
	}
}

// An entry with an unknown kind (a tampered or newer journal) must not be
// accepted: ResolveHeads would treat it as a live file (neither deleted nor
// untracked) that can win a head. readJournal refuses the journal as
// unreadable, naming the file and the entry, and WriteJournal refuses to
// produce one in the first place.
func TestJournalUnknownKind(t *testing.T) {
	v, dir := newVault(t)
	rel := statePath(projA, mA)

	// WriteJournal: refused, nothing written, the journal untouched.
	j := &Journal{Machine: mA, Seq: 4, Entries: map[string]Entry{
		"ok":  entry("ok", "ab", Clock{mA: 1}),
		"bad": {Path: "bad", Kind: Kind(9), Blob: "cd", Clock: Clock{mA: 1}, Machine: mA},
	}}
	err := v.WriteJournal(projA, j)
	if err == nil || !strings.Contains(err.Error(), `"bad"`) || !strings.Contains(err.Error(), "kind 9") {
		t.Errorf("WriteJournal with unknown kind: err = %v, want an error naming the entry and kind", err)
	}
	if j.Seq != 4 || !j.UpdatedAt.IsZero() {
		t.Errorf("failed WriteJournal modified the journal: seq=%d updated=%v", j.Seq, j.UpdatedAt)
	}
	if hasWarning(v.Written(), rel) {
		t.Errorf("Written() = %v lists the refused journal", v.Written())
	}
	if got, err := v.ReadJournal(projA, mA); err != nil || got != nil {
		t.Errorf("ReadJournal after refused write = %+v, %v; want nil, nil", got, err)
	}

	// readJournal: every unknown value, negative or past the last kind.
	for _, kind := range []int{-1, 3, 7} {
		doc := fmt.Sprintf(`{"machine":%q,"seq":1,"entries":{"f":{"path":"f","kind":%d,"blob":"ab","clock":{%q:1},"machine":%q}}}`, mA, kind, mA, mA)
		ct, err := v.SealDoc(rel, []byte(doc))
		if err != nil {
			t.Fatal(err)
		}
		writeRaw(t, dir, rel, ct)
		_, err = v.ReadJournal(projA, mA)
		if !errors.Is(err, ErrUnreadableJournal) || !strings.Contains(err.Error(), rel) || !strings.Contains(err.Error(), `"f"`) || !strings.Contains(err.Error(), fmt.Sprintf("kind %d", kind)) {
			t.Errorf("kind %d: ReadJournal err = %v, want ErrUnreadableJournal naming %s, the entry and the kind", kind, err, rel)
		}
		if js, _, err := v.ReadJournals(projA); !errors.Is(err, ErrUnreadableJournal) || js != nil {
			t.Errorf("kind %d: ReadJournals = %v, %v; want ErrUnreadableJournal (whole project unreadable)", kind, js, err)
		}
	}

	// Every known kind is still accepted, including tombstones without a blob.
	doc := fmt.Sprintf(`{"machine":%q,"seq":1,"entries":{"a":{"kind":0,"blob":"ab","clock":{%q:1}},"b":{"kind":1,"clock":{%q:1}},"c":{"kind":2,"clock":{%q:1}}}}`, mA, mA, mA, mA)
	ct, err := v.SealDoc(rel, []byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, rel, ct)
	got, err := v.ReadJournal(projA, mA)
	if err != nil || got == nil || len(got.Entries) != 3 {
		t.Fatalf("ReadJournal with every known kind = %+v, %v", got, err)
	}
	if got.Entries["b"].Kind != KindDeleted || got.Entries["c"].Kind != KindUntracked || got.Entries["c"].Path != "c" {
		t.Errorf("entries = %+v", got.Entries)
	}
}

// Head.Candidates holds every survivor of dominance filtering, one per
// machine and in machine-id order, exactly as spec §5 documents: entries with
// Equal clocks are not collapsed, and each keeps its own clock while Entry
// carries the merge.
func TestResolveHeadsCandidatesPerMachine(t *testing.T) {
	t1 := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	same := Clock{mA: 1, mB: 1}
	tests := []struct {
		name      string
		entries   []he
		wantEntry bool
		wantKind  Kind
		wantCands []string
	}{
		{
			name: "equal clocks same content on three machines",
			entries: []he{
				{mC, KindFile, "same", same, nil, t1},
				{mA, KindFile, "same", same, nil, t1},
				{mB, KindFile, "same", same, nil, t1},
			},
			wantEntry: true, wantKind: KindFile,
			wantCands: []string{mA, mB, mC},
		},
		{
			name: "equal clocks same content, a dominated third",
			entries: []he{
				{mA, KindFile, "same", same, nil, t1},
				{mB, KindFile, "same", same, nil, t1},
				{mC, KindFile, "old", Clock{mA: 1}, nil, t1},
			},
			wantEntry: true, wantKind: KindFile,
			wantCands: []string{mA, mB},
		},
		{
			name: "equal clocks deleted on both",
			entries: []he{
				{mA, KindDeleted, "", same, nil, t1},
				{mB, KindDeleted, "", same, nil, t1},
			},
			wantEntry: true, wantKind: KindDeleted,
			wantCands: []string{mA, mB},
		},
		{
			name: "equal clocks untracked vs file",
			entries: []he{
				{mA, KindFile, "x", same, nil, t1},
				{mB, KindUntracked, "", same, nil, t1},
			},
			wantEntry: true, wantKind: KindUntracked,
			wantCands: []string{mA, mB},
		},
		{
			name: "equal clocks different content",
			entries: []he{
				{mA, KindFile, "x", same, nil, t1},
				{mB, KindFile, "y", same, nil, t1},
			},
			wantEntry: false,
			wantCands: []string{mA, mB},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			js := journalsOf("f", tc.entries...)
			h := ResolveHeads(js)["f"]
			if got := candidateMachines(h); !reflect.DeepEqual(got, tc.wantCands) {
				t.Errorf("Candidates = %v, want one per surviving machine %v", got, tc.wantCands)
			}
			if (h.Entry != nil) != tc.wantEntry {
				t.Fatalf("Entry = %+v, want present=%v", h.Entry, tc.wantEntry)
			}
			if h.Concurrent() == tc.wantEntry {
				t.Errorf("Concurrent() = %v with Entry %+v", h.Concurrent(), h.Entry)
			}
			if tc.wantEntry && h.Entry.Kind != tc.wantKind {
				t.Errorf("Entry.Kind = %v, want %v", h.Entry.Kind, tc.wantKind)
			}
			for _, c := range h.Candidates {
				if want := js[c.Machine].Entries["f"].Clock; !reflect.DeepEqual(c.Clock, want) {
					t.Errorf("candidate %s clock = %v, want its own %v", c.Machine, c.Clock, want)
				}
			}
			if tc.wantEntry && len(h.Candidates) > 1 && !reflect.DeepEqual(h.Entry.Clock, same) {
				t.Errorf("Entry.Clock = %v, want the merge %v", h.Entry.Clock, same)
			}
		})
	}
}
