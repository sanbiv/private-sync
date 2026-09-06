package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanbiv/private-sync/internal/fsutil"
)

// absPath builds an absolute, platform-native path from elems ("/a/b" on
// Unix, "C:\a\b" on Windows) so pin keys are absolute everywhere.
func absPath(t *testing.T, elems ...string) string {
	t.Helper()
	p, err := filepath.Abs(filepath.Join(append([]string{string(filepath.Separator)}, elems...)...))
	if err != nil {
		t.Fatalf("abs path: %v", err)
	}
	return p
}

// --- Save holds the lock across the write (review: medium) ------------------

func TestSaveConcurrentNeverTearsFile(t *testing.T) {
	stateDir := t.TempDir()
	s, err := Open(stateDir, "v")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	base := filepath.Join(s.Dir(), "base.json")

	const writers, rounds = 8, 25
	errs := make(chan error, writers*rounds+1)
	var writersWG sync.WaitGroup
	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			for r := 0; r < rounds; r++ {
				s.SetBase(fmt.Sprintf("p%d", w), fmt.Sprintf("f%d", r), BaseEntry{Blob: strconv.Itoa(r)})
				s.SetJournalSeq(fmt.Sprintf("m%d", w), uint64(r))
				if err := s.Save(); err != nil {
					errs <- err
					return
				}
			}
		}(w)
	}
	// A concurrent reader must always find a parseable document: the rename is
	// atomic and no two writers share the temp file.
	stop := make(chan struct{})
	var readerWG sync.WaitGroup
	readerWG.Add(1)
	go func() {
		defer readerWG.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			raw, err := os.ReadFile(base)
			if err != nil {
				errs <- fmt.Errorf("read during saves: %w", err)
				return
			}
			var doc baseDoc
			if err := json.Unmarshal(raw, &doc); err != nil {
				errs <- fmt.Errorf("torn base.json during saves: %w", err)
				return
			}
		}
	}()
	writersWG.Wait()
	close(stop)
	readerWG.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// The last Save reflects the complete in-memory state.
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(stateDir, "v")
	if err != nil {
		t.Fatalf("reopen after concurrent saves: %v", err)
	}
	for w := 0; w < writers; w++ {
		p := fmt.Sprintf("p%d", w)
		if n := len(s2.Bases(p)); n != rounds {
			t.Errorf("%s: %d bases after reload, want %d", p, n, rounds)
		}
		if got := s2.JournalSeq(fmt.Sprintf("m%d", w)); got != rounds-1 {
			t.Errorf("m%d: seq %d want %d", w, got, rounds-1)
		}
	}
	// No temp file left behind.
	entries, _ := os.ReadDir(s.Dir())
	for _, e := range entries {
		if fsutil.IsTemp(e.Name()) {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// --- writer-unique temp suffix (review: low) --------------------------------

func TestTempSuffixIsWriterUnique(t *testing.T) {
	pid := strconv.Itoa(os.Getpid())
	if processSuffix != "state-"+pid {
		t.Errorf("processSuffix = %q want state-%s", processSuffix, pid)
	}
	stateDir := t.TempDir()
	a, err := Open(stateDir, "v")
	if err != nil {
		t.Fatal(err)
	}
	b, err := Open(stateDir, "v")
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range []*Store{a, b} {
		if !strings.HasPrefix(s.TempSuffix(), "state-"+pid+"-") {
			t.Errorf("handle suffix %q lacks the state-<pid>- prefix", s.TempSuffix())
		}
		if len(s.TempSuffix()) != len("state-"+pid+"-")+2*handleSuffixBytes {
			t.Errorf("handle suffix %q has unexpected length", s.TempSuffix())
		}
		if !isOwnProcessTemp("base.json" + fsutil.TempPrefix + s.TempSuffix()) {
			t.Errorf("own temp name not recognised for suffix %q", s.TempSuffix())
		}
	}
	if a.TempSuffix() == b.TempSuffix() {
		t.Errorf("two handles share the temp suffix %q", a.TempSuffix())
	}
	if !isOwnProcessTemp("machine.json" + fsutil.TempPrefix + processSuffix) {
		t.Error("machine.json temp name not recognised as own")
	}
	for _, name := range []string{
		"base.json" + fsutil.TempPrefix + "state-999999",
		"base.json" + fsutil.TempPrefix + "state-" + pid + "9", // different pid sharing a prefix
		"base.json" + fsutil.TempPrefix + "w",
		"base.json",
	} {
		if isOwnProcessTemp(name) {
			t.Errorf("%q wrongly recognised as own temp file", name)
		}
	}
	var nilStore *Store
	if nilStore.TempSuffix() != "" || nilStore.Dir() != "" {
		t.Error("nil store TempSuffix/Dir must be empty")
	}
}

func TestOpenRemovesStaleTempFiles(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "vaults", "v")
	shard := filepath.Join(dir, "trash", "ab")
	if err := os.MkdirAll(shard, 0o700); err != nil {
		t.Fatal(err)
	}
	pid := strconv.Itoa(os.Getpid())
	old := time.Now().Add(-2 * staleTempAge)
	mk := func(name string, mtime time.Time) string {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
		return p
	}
	staleBase := mk("base.json"+fsutil.TempPrefix+"state-999999", old)
	staleIndex := mk(filepath.Join("trash", "index.json"+fsutil.TempPrefix+"state-4242"), old)
	staleBlob := mk(filepath.Join("trash", "ab", "abcd.enc"+fsutil.TempPrefix+"state-4242-deadbeef"), old)
	youngOther := mk("base.json"+fsutil.TempPrefix+"state-999998", time.Now())
	ownOld := mk("base.json"+fsutil.TempPrefix+"state-"+pid+"-cafe0000", old)
	foreign := mk("base.json"+fsutil.TempPrefix+"w", old)
	// The real base.json is produced by a Save (correct on-disk encoding) and
	// then backdated like the stale files: age alone must not doom it.
	seed, err := Open(stateDir, "v")
	if err != nil {
		t.Fatal(err)
	}
	seed.SetBase("p", "f", BaseEntry{Blob: "1"})
	if err := seed.Save(); err != nil {
		t.Fatal(err)
	}
	realBase := filepath.Join(dir, "base.json")
	if err := os.Chtimes(realBase, old, old); err != nil {
		t.Fatal(err)
	}

	s, err := Open(stateDir, "v")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, ok := s.Base("p", "f"); !ok {
		t.Error("real base.json not loaded")
	}
	for _, p := range []string{staleBase, staleIndex, staleBlob} {
		if _, err := os.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("stale temp file not removed: %s (%v)", p, err)
		}
	}
	for _, p := range []string{youngOther, ownOld, foreign, realBase} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("file wrongly removed: %s (%v)", p, err)
		}
	}
	// Open never fails because of cleanup problems: a missing dir is fine.
	removeStaleTemp(filepath.Join(stateDir, "nope"), true)
}

func TestLoadMachineRemovesStaleTempTopLevelOnly(t *testing.T) {
	stateDir := t.TempDir()
	nested := filepath.Join(stateDir, "vaults", "v")
	if err := os.MkdirAll(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * staleTempAge)
	top := filepath.Join(stateDir, "machine.json"+fsutil.TempPrefix+"state-777")
	deep := filepath.Join(nested, "base.json"+fsutil.TempPrefix+"state-777")
	for _, p := range []string{top, deep} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := LoadMachine(stateDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(top); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("stale machine.json temp not removed: %v", err)
	}
	if _, err := os.Stat(deep); err != nil {
		t.Errorf("LoadMachine must not walk into vault dirs: %v", err)
	}
	// The stale file must not have been mistaken for machine.json.
	m, err := LoadMachine(stateDir)
	if err != nil || m.ID == "" {
		t.Fatalf("LoadMachine after cleanup: %+v %v", m, err)
	}
}

// --- pins must be keyed by absolute path (review: low) ----------------------

func TestPinRelativePathRejected(t *testing.T) {
	stateDir := t.TempDir()
	for _, p := range []string{"relative/vault", ".", "..", "vault", "  ", "./x"} {
		if err := PinVault(stateDir, p, "id"); err == nil {
			t.Errorf("PinVault(%q): want error", p)
		}
		if _, _, err := PinnedVault(stateDir, p); err == nil {
			t.Errorf("PinnedVault(%q): want error", p)
		}
		if err := UnpinVault(stateDir, p); err == nil {
			t.Errorf("UnpinVault(%q): want error", p)
		}
		if err := CheckPin(stateDir, p, "id"); err == nil {
			t.Errorf("CheckPin(%q): want error", p)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, "machine.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("rejected pins must not create machine.json: %v", err)
	}
	// Absolute paths with redundant elements are cleaned, not rejected.
	vp := absPath(t, "a", "b")
	if err := PinVault(stateDir, filepath.Join(vp, "..", "b", "."), "id"); err != nil {
		t.Fatalf("PinVault cleaned absolute: %v", err)
	}
	if id, ok, _ := PinnedVault(stateDir, vp); !ok || id != "id" {
		t.Errorf("cleaned pin not found: %q %v", id, ok)
	}
	raw, _ := os.ReadFile(filepath.Join(stateDir, "machine.json"))
	var doc struct {
		Vaults map[string]string `json:"vaults"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := doc.Vaults[vp]; !ok || len(doc.Vaults) != 1 {
		t.Errorf("vaults map keyed by %v, want only %q", doc.Vaults, vp)
	}
}

// --- TrashPut on a corrupt index leaves no orphan blob ----------------------

func TestTrashPutCorruptIndexWritesNoBlob(t *testing.T) {
	k := testKeys(t)
	s, err := Open(t.TempDir(), "v")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.trashPath(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.trashIndexPath(), []byte("{{{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.TrashPut(k, "p", "x", []byte("secret"), 0o600); err == nil {
		t.Fatal("TrashPut on corrupt index: want error")
	}
	var blobs []string
	_ = filepath.WalkDir(s.trashPath(), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() && strings.HasSuffix(p, ".enc") {
			blobs = append(blobs, p)
		}
		return nil
	})
	if len(blobs) != 0 {
		t.Errorf("orphan blob written despite index failure: %v", blobs)
	}
}
