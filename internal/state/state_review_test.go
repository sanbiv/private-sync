package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Regression tests for the second review round.

// --- pins are keyed by absolute paths on every platform ---------------------

func TestPinKeyIsAbsoluteOnThisPlatform(t *testing.T) {
	stateDir := t.TempDir()
	vp := absPath(t, "home", "me", "vault")
	if !filepath.IsAbs(vp) {
		t.Fatalf("absPath produced a non-absolute path %q on %s", vp, runtime.GOOS)
	}
	if err := PinVault(stateDir, vp, "v1"); err != nil {
		t.Fatalf("PinVault(%q): %v", vp, err)
	}
	// The bare rooted form without a volume is rejected on Windows only; on
	// unix it is the same path.
	rooted := filepath.Join(string(filepath.Separator), "home", "me", "vault")
	_, _, err := PinnedVault(stateDir, rooted)
	if runtime.GOOS == "windows" {
		if err == nil {
			t.Errorf("PinnedVault(%q) on windows: want error (no volume)", rooted)
		}
	} else if err != nil {
		t.Errorf("PinnedVault(%q): %v", rooted, err)
	}
}

// --- ErrVaultMismatch carries the spec §9.4 remedy --------------------------

func TestErrVaultMismatchCarriesRemedy(t *testing.T) {
	msg := ErrVaultMismatch.Error()
	for _, want := range []string{
		"remote already contains a different vault",
		"run init and choose open",
		"delete the local copy",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrVaultMismatch %q lacks %q", msg, want)
		}
	}
	// Wrapping via CheckPin keeps the remedy visible to the user.
	stateDir := t.TempDir()
	vp := absPath(t, "v")
	if err := CheckPin(stateDir, vp, "one"); err != nil {
		t.Fatal(err)
	}
	err := CheckPin(stateDir, vp, "two")
	if !errors.Is(err, ErrVaultMismatch) || !strings.Contains(err.Error(), "delete the local copy") {
		t.Errorf("CheckPin mismatch = %v", err)
	}
}

// --- trash index row uses the "ts" key (spec §9.4) --------------------------

func TestTrashIndexRowSchema(t *testing.T) {
	k := testKeys(t)
	s, err := Open(t.TempDir(), "v")
	if err != nil {
		t.Fatal(err)
	}
	e := mustPut(t, s, k, "proj", "a/b", []byte("x"), 0o640)
	raw, err := os.ReadFile(s.trashIndexPath())
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("index: %v\n%s", err, raw)
	}
	if len(rows) != 1 {
		t.Fatalf("%d rows", len(rows))
	}
	row := rows[0]
	for _, key := range []string{"id", "ts", "project", "path", "blob", "mode", "size"} {
		if _, ok := row[key]; !ok {
			t.Errorf("index row lacks %q: %v", key, row)
		}
	}
	if _, has := row["time"]; has {
		t.Errorf("index row still uses the legacy \"time\" key: %v", row)
	}
	if len(row) != 7 {
		t.Errorf("index row has %d keys, want 7: %v", len(row), row)
	}
	ts, _ := time.Parse(time.RFC3339Nano, fmt.Sprint(row["ts"]))
	if !ts.Equal(e.Time) {
		t.Errorf("ts = %v want %v", row["ts"], e.Time)
	}
	// And it round-trips through TrashList.
	list, err := s.TrashList()
	if err != nil || len(list) != 1 || !list[0].Time.Equal(e.Time) {
		t.Errorf("TrashList = %+v, %v", list, err)
	}
}

// --- journal sequence semantics ---------------------------------------------

func TestJournalSeqSemantics(t *testing.T) {
	s, err := Open(t.TempDir(), "v")
	if err != nil {
		t.Fatal(err)
	}
	// SetJournalSeq overwrites: the engine relies on it to reset after an
	// accepted rollback.
	s.SetJournalSeq("m", 10)
	s.SetJournalSeq("m", 3)
	if got := s.JournalSeq("m"); got != 3 {
		t.Errorf("SetJournalSeq lower: seq = %d want 3 (overwrite semantics)", got)
	}
	// AdvanceJournalSeq never regresses.
	if !s.AdvanceJournalSeq("m", 7) || s.JournalSeq("m") != 7 {
		t.Errorf("AdvanceJournalSeq(7) after 3: seq = %d", s.JournalSeq("m"))
	}
	if s.AdvanceJournalSeq("m", 7) {
		t.Error("AdvanceJournalSeq(equal) reported a change")
	}
	if s.AdvanceJournalSeq("m", 2) || s.JournalSeq("m") != 7 {
		t.Errorf("AdvanceJournalSeq(2) regressed the watermark to %d", s.JournalSeq("m"))
	}
	if !s.AdvanceJournalSeq("new", 1) || s.JournalSeq("new") != 1 {
		t.Error("AdvanceJournalSeq on unknown machine")
	}
	if s.AdvanceJournalSeq("", 5) || s.AdvanceJournalSeq("zero", 0) {
		t.Error("AdvanceJournalSeq recorded an empty machine or a zero seq")
	}
	if _, ok := s.JournalSeqs()["zero"]; ok {
		t.Error("zero seq recorded")
	}
	var nilStore *Store
	if nilStore.AdvanceJournalSeq("m", 1) {
		t.Error("nil store AdvanceJournalSeq")
	}
}

// --- purge keeps shard dirs; puts keep working afterwards -------------------

func TestTrashPurgeKeepsShardDirUsable(t *testing.T) {
	k := testKeys(t)
	s, err := Open(t.TempDir(), "v")
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("only once\n")
	e := mustPut(t, s, k, "p", "a", content, 0o600)
	entries := readIndex(t, s)
	entries[0].Time = time.Now().Add(-48 * time.Hour)
	writeIndex(t, s, entries)
	if n, err := s.TrashPurge(24 * time.Hour); err != nil || n != 1 {
		t.Fatalf("purge: n=%d err=%v", n, err)
	}
	shard := filepath.Join(s.Dir(), "trash", e.Blob[:2])
	if st, err := os.Stat(shard); err != nil || !st.IsDir() {
		t.Fatalf("shard dir removed by purge: %v", err)
	}
	if _, err := os.Stat(filepath.Join(shard, e.Blob+".enc")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("blob not removed: %v", err)
	}
	// Re-putting the same content lands in the same shard and is readable.
	e2 := mustPut(t, s, k, "p", "a", content, 0o600)
	if e2.Blob != e.Blob {
		t.Errorf("blob changed: %s -> %s", e.Blob, e2.Blob)
	}
	got, _, err := s.TrashRead(k, e2.ID)
	if err != nil || !bytes.Equal(got, content) {
		t.Errorf("TrashRead after purge+put: %v", err)
	}
}

// --- advisory lock ------------------------------------------------------------

func TestDirLockSerialises(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sub")
	l1, err := lockDir(dir)
	if err != nil {
		t.Fatalf("lockDir: %v", err)
	}
	assertPerm(t, dir, 0o700)
	assertPerm(t, filepath.Join(dir, lockFile), 0o600)

	acquired := make(chan time.Time, 1)
	go func() {
		l2, err := lockDir(dir)
		if err != nil {
			t.Errorf("second lockDir: %v", err)
			acquired <- time.Time{}
			return
		}
		acquired <- time.Now()
		l2.Unlock()
	}()
	select {
	case <-acquired:
		t.Fatal("second lock acquired while the first was held")
	case <-time.After(150 * time.Millisecond):
	}
	released := time.Now()
	l1.Unlock()
	select {
	case at := <-acquired:
		if at.Before(released) {
			t.Error("second lock acquired before the first was released")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("second lock never acquired after Unlock")
	}
	// Unlock is idempotent and nil-safe.
	l1.Unlock()
	var nilLock *dirLock
	nilLock.Unlock()
	if _, err := lockDir(""); err == nil {
		t.Error("lockDir(\"\"): want error")
	}
}

// Two Store handles do not share s.mu; only the lock file keeps their
// read-modify-write cycles on the trash index from losing rows.
func TestTrashPutConcurrentHandlesLoseNoRows(t *testing.T) {
	k := testKeys(t)
	stateDir := t.TempDir()
	const handles, puts = 6, 20
	var wg sync.WaitGroup
	errs := make(chan error, handles*puts)
	for h := 0; h < handles; h++ {
		s, err := Open(stateDir, "v")
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(h int, s *Store) {
			defer wg.Done()
			for i := 0; i < puts; i++ {
				content := []byte(fmt.Sprintf("h%d-%d", h, i%3)) // some dedupe across handles
				if _, err := s.TrashPut(k, "p", fmt.Sprintf("h%d/f%d", h, i), content, 0o600); err != nil {
					errs <- err
					return
				}
				if i%5 == 4 {
					if _, err := s.TrashPurge(time.Hour); err != nil { // never removes anything, still RMWs
						errs <- err
						return
					}
				}
			}
		}(h, s)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	s, err := Open(stateDir, "v")
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.TrashList()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != handles*puts {
		t.Fatalf("index has %d rows, want %d (rows lost to a concurrent rename)", len(list), handles*puts)
	}
	ids := map[string]bool{}
	for _, e := range list {
		if ids[e.ID] {
			t.Errorf("duplicate id %s", e.ID)
		}
		ids[e.ID] = true
		got, _, err := s.TrashRead(k, e.ID)
		if err != nil {
			t.Errorf("TrashRead(%s): %v", e.ID, err)
			continue
		}
		parts := strings.SplitN(e.Path, "/", 2)
		if !bytes.HasPrefix(got, []byte(parts[0]+"-")) {
			t.Errorf("entry %s (%s) decrypted to %q", e.ID, e.Path, got)
		}
	}
}

// --- real cross-process race ------------------------------------------------

const helperEnv = "PRIVATE_SYNC_STATE_TEST_HELPER"

// TestHelperProcess is the body of the child processes spawned by
// TestConcurrentProcessesLoseNoRows: it performs puts and pins against the
// state dir named in the environment. It does nothing when run normally.
func TestHelperProcess(t *testing.T) {
	spec := os.Getenv(helperEnv)
	if spec == "" {
		t.Skip("helper process only")
	}
	// spec: <stateDir>|<worker>|<puts>
	parts := strings.Split(spec, "|")
	if len(parts) != 3 {
		fmt.Fprintln(os.Stderr, "bad helper spec")
		os.Exit(2)
	}
	stateDir, worker := parts[0], parts[1]
	puts, _ := strconv.Atoi(parts[2])
	k := testKeys(t)
	s, err := Open(stateDir, "v")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(3)
	}
	for i := 0; i < puts; i++ {
		content := []byte(fmt.Sprintf("w%s-%d", worker, i%2))
		if _, err := s.TrashPut(k, "p", fmt.Sprintf("w%s/f%d", worker, i), content, 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(4)
		}
		if _, err := s.TrashPurge(time.Hour); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(5)
		}
		if err := PinVault(stateDir, filepath.Join(stateDir, "vaults-of-"+worker, strconv.Itoa(i)), "id-"+worker); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(6)
		}
		s.SetJournalSeq("m"+worker, uint64(i+1))
		if err := s.Save(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(7)
		}
	}
	os.Exit(0)
}

func TestConcurrentProcessesLoseNoRows(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns child processes")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Skipf("os.Executable: %v", err)
	}
	stateDir := t.TempDir()
	const workers, puts = 4, 15
	var wg sync.WaitGroup
	outs := make([]string, workers)
	errs := make([]error, workers)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			cmd := exec.Command(exe, "-test.run=^TestHelperProcess$", "-test.v=false")
			cmd.Env = append(os.Environ(), helperEnv+"="+strings.Join([]string{stateDir, strconv.Itoa(w), strconv.Itoa(puts)}, "|"))
			out, err := cmd.CombinedOutput()
			outs[w], errs[w] = string(out), err
		}(w)
	}
	wg.Wait()
	for w := range errs {
		if errs[w] != nil {
			t.Fatalf("worker %d: %v\n%s", w, errs[w], outs[w])
		}
	}
	s, err := Open(stateDir, "v")
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.TrashList()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != workers*puts {
		t.Errorf("index has %d rows, want %d (rows lost to a concurrent process)", len(list), workers*puts)
	}
	k := testKeys(t)
	for _, e := range list {
		if _, _, err := s.TrashRead(k, e.ID); err != nil {
			t.Errorf("TrashRead(%s): %v", e.ID, err)
		}
	}
	// Every pin of every worker survived the concurrent machine.json rewrites.
	for w := 0; w < workers; w++ {
		for i := 0; i < puts; i++ {
			p := filepath.Join(stateDir, "vaults-of-"+strconv.Itoa(w), strconv.Itoa(i))
			id, ok, err := PinnedVault(stateDir, p)
			if err != nil || !ok || id != "id-"+strconv.Itoa(w) {
				t.Errorf("pin %s: id=%q ok=%v err=%v", p, id, ok, err)
			}
		}
	}
	m, err := LoadMachine(stateDir)
	if err != nil || m.ID == "" {
		t.Fatalf("LoadMachine after workers: %+v %v", m, err)
	}
	// base.json is intact (each worker's last Save is a complete document).
	if len(s.JournalSeqs()) == 0 {
		t.Error("no journal seq survived")
	}
	// No temp files or stray files besides the expected ones.
	entries, _ := os.ReadDir(s.Dir())
	for _, e := range entries {
		if strings.Contains(e.Name(), ".psv-tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
	assertPerm(t, filepath.Join(stateDir, lockFile), 0o600)
	assertPerm(t, filepath.Join(s.Dir(), lockFile), 0o600)
}
