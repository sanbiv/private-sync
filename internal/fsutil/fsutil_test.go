package fsutil

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestWriteFileAtomicTempNameNotFixed pins the regression: the temp file name
// used to be exactly "<target>.psv-tmp-<suffix>", i.e. constant for a given
// target and machine, so two writers on one machine opened, truncated and
// renamed the very same file. Occupying the old fixed name proves the writer no
// longer depends on it.
func TestWriteFileAtomicTempNameNotFixed(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "journal.json.enc")
	const suffix = "1d4aa9b9"

	// Something else already owns the legacy fixed temp path.
	if err := os.Mkdir(target+TempPrefix+suffix, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(target, []byte("payload"), 0o600, suffix); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "payload" {
		t.Fatalf("target content = %q, want %q", got, "payload")
	}
	st, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("target mode = %v, want 0600", perm)
	}
	if leftovers := tempFiles(t, dir); len(leftovers) != 1 {
		// Only the directory we planted may remain.
		t.Fatalf("unexpected temp leftovers: %v", leftovers)
	}
}

// TestCreateTempNamesAreUnique checks the naming itself: two temp files for the
// same target and the same writer suffix coexist under distinct names, while
// still carrying the marker the other packages match on. Before the fix both
// calls named the same path, so the second silently truncated the first.
func TestCreateTempNamesAreUnique(t *testing.T) {
	dir := t.TempDir()
	const suffix = "1d4aa9b9"

	names := map[string]bool{}
	for i := 0; i < 8; i++ {
		f, err := createTemp(dir, "journal.json.enc", suffix)
		if err != nil {
			t.Fatalf("createTemp: %v", err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		n := filepath.Base(f.Name())
		if names[n] {
			t.Fatalf("createTemp reused the name %q", n)
		}
		names[n] = true
		if !IsTemp(n) {
			t.Fatalf("temp name %q not recognised by IsTemp", n)
		}
		if !strings.HasPrefix(n, "journal.json.enc"+TempPrefix+suffix+"-") {
			t.Fatalf("temp name %q does not carry the target name and writer suffix", n)
		}
	}
	// All eight still exist side by side: none overwrote another.
	if got := len(tempFiles(t, dir)); got != len(names) {
		t.Fatalf("%d temp files on disk, want %d", got, len(names))
	}
}

// TestWriteFileAtomicConcurrentSameSuffix is the product-level regression: two
// private-sync processes on one machine (same machine-id suffix) writing the
// same target used to collide — chmod/rename of a temp file another writer had
// already renamed away, and target bytes interleaved from both payloads.
func TestWriteFileAtomicConcurrentSameSuffix(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state", "machine.json.enc")
	const (
		suffix  = "1d4aa9b9"
		size    = 16 << 10
		writers = 6
		rounds  = 12
	)

	payloads := make([][]byte, writers)
	for i := range payloads {
		payloads[i] = bytes.Repeat([]byte{byte('A' + i)}, size)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, writers*rounds)
	stop := make(chan struct{})
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(data []byte) {
			defer wg.Done()
			for r := 0; r < rounds; r++ {
				if err := WriteFileAtomic(target, data, 0o600, suffix); err != nil {
					errCh <- err
					return
				}
			}
		}(payloads[i])
	}

	// A reader that must never observe a half-published (torn) file.
	torn := make(chan []byte, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			select {
			case <-stop:
				return
			default:
			}
			b, err := os.ReadFile(target)
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) {
					continue
				}
				select {
				case torn <- []byte(err.Error()):
				default:
				}
				return
			}
			if len(b) != size || bytes.Count(b, b[:1]) != len(b) {
				select {
				case torn <- b:
				default:
				}
				return
			}
		}
	}()

	wg.Wait()
	close(stop)
	<-readerDone
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent WriteFileAtomic: %v", err)
	}
	select {
	case b := <-torn:
		t.Fatalf("reader observed a torn published file (%d bytes): %.64q", len(b), b)
	default:
	}

	final, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(final) != size || bytes.Count(final, final[:1]) != len(final) {
		t.Fatalf("final file is not one writer's payload (%d bytes): %.64q", len(final), final)
	}
	if names := tempFiles(t, dir); len(names) != 0 {
		t.Fatalf("temp files left behind: %v", names)
	}
}

// TestCleanupTempMatchesUniqueNames guards the cleanup side of the fix: the
// unique tail must not hide a writer's own leftovers, and must not make another
// writer's temp files look like ours.
func TestCleanupTempMatchesUniqueNames(t *testing.T) {
	dir := t.TempDir()
	const suffix = "1d4aa9b9"

	mk := func(rel string) string {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}

	gone := []string{
		mk("base.json" + TempPrefix + suffix),                // legacy fixed name
		mk("base.json" + TempPrefix + suffix + "-123456789"), // new unique name
		mk("blobs/ab/abcd.enc" + TempPrefix + suffix + "-42"),
	}
	kept := []string{
		mk("base.json" + TempPrefix + "deadbeef"),      // another machine
		mk("base.json" + TempPrefix + suffix + "0-77"), // longer suffix sharing our prefix
		mk("base.json"),
	}

	if err := CleanupTemp(dir, suffix); err != nil {
		t.Fatalf("CleanupTemp: %v", err)
	}
	for _, p := range gone {
		if Exists(p) {
			t.Errorf("CleanupTemp left %s behind", filepath.Base(p))
		}
	}
	for _, p := range kept {
		if !Exists(p) {
			t.Errorf("CleanupTemp removed %s, which is not ours", filepath.Base(p))
		}
	}
}

// tempFiles lists the names of temp entries below root.
func tempFiles(t *testing.T, root string) []string {
	t.Helper()
	var names []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if IsTemp(d.Name()) {
			names = append(names, d.Name())
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return names
}
