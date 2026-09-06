package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/vault"
)

func testKeys(t *testing.T) *crypto.Keys {
	t.Helper()
	vk := bytes.Repeat([]byte{0x42}, 32)
	k, err := crypto.NewKeys(vk)
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}
	return k
}

func assertPerm(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	if got := st.Mode().Perm(); got != want {
		t.Errorf("%s: perm = %o, want %o", path, got, want)
	}
}

func readIndex(t *testing.T, s *Store) []TrashEntry {
	t.Helper()
	data, err := os.ReadFile(s.trashIndexPath())
	if err != nil {
		t.Fatalf("read index: %v", err)
	}
	var entries []TrashEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		t.Fatalf("parse index: %v", err)
	}
	return entries
}

func writeIndex(t *testing.T, s *Store, entries []TrashEntry) {
	t.Helper()
	if err := s.writeTrashIndex(entries); err != nil {
		t.Fatalf("write index: %v", err)
	}
}

// mustPut is TrashPut that fails the test on error, so a failing put surfaces
// as a readable assertion instead of an index-out-of-range on e.Blob[:2].
func mustPut(t *testing.T, s *Store, k *crypto.Keys, project, path string, content []byte, mode uint32) TrashEntry {
	t.Helper()
	e, err := s.TrashPut(k, project, path, content, mode)
	if err != nil {
		t.Fatalf("TrashPut(%s/%s): %v", project, path, err)
	}
	if len(e.Blob) < 2 {
		t.Fatalf("TrashPut(%s/%s): short blob id %q", project, path, e.Blob)
	}
	return e
}

// --- machine.json -----------------------------------------------------------

func TestLoadMachineCreatesOnceAndIsStable(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "nested", "state")
	before := time.Now().Add(-2 * time.Second)

	m1, err := LoadMachine(stateDir)
	if err != nil {
		t.Fatalf("LoadMachine (first): %v", err)
	}
	if _, err := uuid.Parse(m1.ID); err != nil {
		t.Fatalf("machine id %q is not a uuid: %v", m1.ID, err)
	}
	if u := uuid.MustParse(m1.ID); u.Version() != 4 {
		t.Errorf("uuid version = %d, want 4", u.Version())
	}
	if m1.CreatedAt.Before(before) || m1.CreatedAt.After(time.Now().Add(time.Second)) {
		t.Errorf("created_at %v not close to now", m1.CreatedAt)
	}
	assertPerm(t, stateDir, 0o700)
	assertPerm(t, filepath.Join(stateDir, "machine.json"), 0o600)

	// Raw file shape.
	raw, err := os.ReadFile(filepath.Join(stateDir, "machine.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("machine.json is not JSON: %v\n%s", err, raw)
	}
	for _, key := range []string{"id", "created_at", "vaults"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("machine.json lacks %q key: %s", key, raw)
		}
	}
	if doc["id"] != m1.ID {
		t.Errorf("machine.json id = %v, want %s", doc["id"], m1.ID)
	}

	for i := 0; i < 3; i++ {
		m2, err := LoadMachine(stateDir)
		if err != nil {
			t.Fatalf("LoadMachine (call %d): %v", i+2, err)
		}
		if m2.ID != m1.ID {
			t.Fatalf("machine id changed: %s -> %s", m1.ID, m2.ID)
		}
		if !m2.CreatedAt.Equal(m1.CreatedAt) {
			t.Fatalf("created_at changed: %v -> %v", m1.CreatedAt, m2.CreatedAt)
		}
	}
	// A second state dir gets a different identity.
	other, err := LoadMachine(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == m1.ID {
		t.Errorf("two state dirs share machine id %s", m1.ID)
	}
}

func TestLoadMachineErrors(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{"corrupt json", "{not json"},
		{"empty id", `{"id":"","created_at":"2026-01-01T00:00:00Z","vaults":{}}`},
		{"wrong type", `[1,2,3]`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			stateDir := t.TempDir()
			if err := os.WriteFile(filepath.Join(stateDir, "machine.json"), []byte(tc.content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadMachine(stateDir); err == nil {
				t.Fatalf("LoadMachine on %s: want error", tc.name)
			}
			// The file must not have been clobbered with a fresh identity.
			got, _ := os.ReadFile(filepath.Join(stateDir, "machine.json"))
			if string(got) != tc.content {
				t.Errorf("machine.json overwritten: %s", got)
			}
		})
	}
	if _, err := LoadMachine(""); err == nil {
		t.Error("LoadMachine(\"\"): want error")
	}
}

func TestLoadMachinePreservesPins(t *testing.T) {
	stateDir := t.TempDir()
	if err := PinVault(stateDir, absPath(t, "vaults", "a"), "vault-a"); err != nil {
		t.Fatal(err)
	}
	m, err := LoadMachine(stateDir)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID == "" {
		t.Fatal("empty machine id after PinVault-first flow")
	}
	id, ok, err := PinnedVault(stateDir, absPath(t, "vaults", "a"))
	if err != nil || !ok || id != "vault-a" {
		t.Fatalf("pin lost after LoadMachine: id=%q ok=%v err=%v", id, ok, err)
	}
}

func TestPinUnpin(t *testing.T) {
	stateDir := t.TempDir()
	vp := absPath(t, "home", "me", "vault") // absolute on every platform (drive letter on Windows)
	other := absPath(t, "another", "vault")

	// Nothing pinned yet (no machine.json at all).
	id, ok, err := PinnedVault(stateDir, vp)
	if err != nil || ok || id != "" {
		t.Fatalf("PinnedVault on fresh dir: id=%q ok=%v err=%v", id, ok, err)
	}
	if _, err := os.Stat(filepath.Join(stateDir, "machine.json")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("PinnedVault must not create machine.json")
	}

	if err := PinVault(stateDir, vp, "v1"); err != nil {
		t.Fatalf("PinVault: %v", err)
	}
	assertPerm(t, filepath.Join(stateDir, "machine.json"), 0o600)

	// Same path, uncleaned variants resolve to the same pin.
	for _, p := range []string{vp, vp + string(filepath.Separator), filepath.Join(vp, "..", "vault"), filepath.Join(vp, ".")} {
		id, ok, err := PinnedVault(stateDir, p)
		if err != nil {
			t.Fatalf("PinnedVault(%q): %v", p, err)
		}
		if !ok || id != "v1" {
			t.Errorf("PinnedVault(%q) = %q,%v want v1,true", p, id, ok)
		}
	}
	// Other path: not pinned.
	if _, ok, _ := PinnedVault(stateDir, filepath.Join(vp, "other")); ok {
		t.Error("unrelated path reported as pinned")
	}

	// Machine id survives pinning.
	m1, _ := LoadMachine(stateDir)
	if err := PinVault(stateDir, other, "v2"); err != nil {
		t.Fatal(err)
	}
	m2, _ := LoadMachine(stateDir)
	if m1.ID != m2.ID {
		t.Errorf("machine id changed by PinVault: %s -> %s", m1.ID, m2.ID)
	}

	// Re-pinning overwrites.
	if err := PinVault(stateDir, vp, "v1b"); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := PinnedVault(stateDir, vp); id != "v1b" {
		t.Errorf("re-pin: got %q want v1b", id)
	}

	// Raw file has the vaults map keyed by cleaned path.
	raw, _ := os.ReadFile(filepath.Join(stateDir, "machine.json"))
	var doc struct {
		Vaults map[string]string `json:"vaults"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Vaults[vp] != "v1b" || doc.Vaults[other] != "v2" {
		t.Errorf("vaults map = %v", doc.Vaults)
	}

	// Unpin.
	if err := UnpinVault(stateDir, vp+string(filepath.Separator)); err != nil {
		t.Fatalf("UnpinVault: %v", err)
	}
	if _, ok, _ := PinnedVault(stateDir, vp); ok {
		t.Error("still pinned after UnpinVault")
	}
	if id, ok, _ := PinnedVault(stateDir, other); !ok || id != "v2" {
		t.Error("UnpinVault removed an unrelated pin")
	}
	if err := UnpinVault(stateDir, absPath(t, "never", "pinned")); err != nil {
		t.Errorf("UnpinVault unknown path: %v", err)
	}
	if err := UnpinVault(t.TempDir(), absPath(t, "x")); err != nil {
		t.Errorf("UnpinVault on fresh dir: %v", err)
	}

	// Argument validation.
	if err := PinVault(stateDir, "", "v"); err == nil {
		t.Error("PinVault empty path: want error")
	}
	if err := PinVault(stateDir, vp, ""); err == nil {
		t.Error("PinVault empty id: want error")
	}
	if _, _, err := PinnedVault(stateDir, ""); err == nil {
		t.Error("PinnedVault empty path: want error")
	}
	if _, _, err := PinnedVault("", vp); err == nil {
		t.Error("PinnedVault empty state dir: want error")
	}
}

func TestCheckPin(t *testing.T) {
	stateDir := t.TempDir()
	vp := absPath(t, "v", "one")
	if err := CheckPin(stateDir, vp, "id-1"); err != nil {
		t.Fatalf("CheckPin first: %v", err)
	}
	if id, ok, _ := PinnedVault(stateDir, vp); !ok || id != "id-1" {
		t.Fatalf("CheckPin did not pin: %q %v", id, ok)
	}
	if err := CheckPin(stateDir, vp, "id-1"); err != nil {
		t.Errorf("CheckPin same: %v", err)
	}
	err := CheckPin(stateDir, vp, "id-2")
	if !errors.Is(err, ErrVaultMismatch) {
		t.Errorf("CheckPin other id: %v, want ErrVaultMismatch", err)
	}
	if id, _, _ := PinnedVault(stateDir, vp); id != "id-1" {
		t.Errorf("mismatch changed the pin to %q", id)
	}
}

func TestPinnedVaultCorruptFile(t *testing.T) {
	stateDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(stateDir, "machine.json"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := PinnedVault(stateDir, absPath(t, "v")); err == nil {
		t.Error("PinnedVault on corrupt file: want error")
	}
	if err := PinVault(stateDir, absPath(t, "v"), "x"); err == nil {
		t.Error("PinVault on corrupt file: want error")
	}
}

// --- base store -------------------------------------------------------------

func TestStoreRoundTrip(t *testing.T) {
	stateDir := t.TempDir()
	s, err := Open(stateDir, "vault-1")
	if err != nil {
		t.Fatalf("Open (fresh): %v", err)
	}
	if want := filepath.Join(stateDir, "vaults", "vault-1"); s.Dir() != want {
		t.Errorf("Dir = %s want %s", s.Dir(), want)
	}
	assertPerm(t, s.Dir(), 0o700)
	if _, ok := s.Base("p1", ".env"); ok {
		t.Error("fresh store has a base")
	}
	if n := len(s.Bases("p1")); n != 0 {
		t.Errorf("fresh store Bases = %d entries", n)
	}
	if s.JournalSeq("m1") != 0 {
		t.Error("fresh store has a journal seq")
	}

	p1 := map[string]BaseEntry{
		".env":              {Blob: "aa11", Kind: vault.KindFile, Clock: vault.Clock{"m1": 3, "m2": 1}},
		"config/secret.yml": {Blob: "bb22", Kind: vault.KindFile, Clock: vault.Clock{"m1": 1}},
		"gone.txt":          {Kind: vault.KindDeleted, Clock: vault.Clock{"m2": 7}},
	}
	p2 := map[string]BaseEntry{
		".env.local": {Blob: "cc33", Kind: vault.KindUntracked, Clock: vault.Clock{"m1": 2}},
	}
	for p, b := range p1 {
		s.SetBase("p1", p, b)
	}
	for p, b := range p2 {
		s.SetBase("p2", p, b)
	}
	s.SetJournalSeq("m1", 10)
	s.SetJournalSeq("m2", 42)
	s.SetJournalSeq("m2", 43) // overwrite
	if err := s.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	assertPerm(t, filepath.Join(s.Dir(), "base.json"), 0o600)

	// Raw file shape.
	raw, err := os.ReadFile(filepath.Join(s.Dir(), "base.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Projects map[string]map[string]map[string]any `json:"projects"`
		Journals map[string]uint64                    `json:"journals"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("base.json: %v\n%s", err, raw)
	}
	if doc.Journals["m1"] != 10 || doc.Journals["m2"] != 43 {
		t.Errorf("journals = %v", doc.Journals)
	}
	if doc.Projects["p1"][".env"]["blob"] != "aa11" {
		t.Errorf("projects.p1[.env] = %v", doc.Projects["p1"][".env"])
	}
	if _, has := doc.Projects["p1"]["gone.txt"]["blob"]; has {
		t.Errorf("tombstone must omit blob: %v", doc.Projects["p1"]["gone.txt"])
	}

	// Stable output.
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	raw2, _ := os.ReadFile(filepath.Join(s.Dir(), "base.json"))
	if !bytes.Equal(raw, raw2) {
		t.Errorf("Save output not stable:\n%s\n---\n%s", raw, raw2)
	}

	// Reopen.
	s2, err := Open(stateDir, "vault-1")
	if err != nil {
		t.Fatalf("Open (reload): %v", err)
	}
	for project, want := range map[string]map[string]BaseEntry{"p1": p1, "p2": p2} {
		got := s2.Bases(project)
		if len(got) != len(want) {
			t.Fatalf("%s: Bases = %d entries want %d", project, len(got), len(want))
		}
		for p, wb := range want {
			gb, ok := s2.Base(project, p)
			if !ok {
				t.Fatalf("%s/%s: missing after reload", project, p)
			}
			if gb.Blob != wb.Blob || gb.Kind != wb.Kind || len(gb.Clock) != len(wb.Clock) {
				t.Errorf("%s/%s: got %+v want %+v", project, p, gb, wb)
			}
			for m, c := range wb.Clock {
				if gb.Clock[m] != c {
					t.Errorf("%s/%s: clock[%s] = %d want %d", project, p, m, gb.Clock[m], c)
				}
			}
			if got[p].Blob != wb.Blob {
				t.Errorf("%s/%s: Bases()[p] = %+v", project, p, got[p])
			}
		}
	}
	if s2.JournalSeq("m1") != 10 || s2.JournalSeq("m2") != 43 || s2.JournalSeq("m3") != 0 {
		t.Errorf("journal seqs after reload: m1=%d m2=%d m3=%d", s2.JournalSeq("m1"), s2.JournalSeq("m2"), s2.JournalSeq("m3"))
	}
	seqs := s2.JournalSeqs()
	if len(seqs) != 2 || seqs["m2"] != 43 {
		t.Errorf("JournalSeqs = %v", seqs)
	}
	if got := s2.Projects(); strings.Join(got, ",") != "p1,p2" {
		t.Errorf("Projects = %v", got)
	}
}

func TestStoreIsolation(t *testing.T) {
	s, err := Open(t.TempDir(), "v")
	if err != nil {
		t.Fatal(err)
	}
	clock := vault.Clock{"m1": 1}
	s.SetBase("p", "f", BaseEntry{Blob: "x", Clock: clock})
	clock["m1"] = 99 // caller mutates its map after SetBase
	got, _ := s.Base("p", "f")
	if got.Clock["m1"] != 1 {
		t.Errorf("SetBase aliased caller clock: %v", got.Clock)
	}
	got.Clock["m1"] = 5 // caller mutates returned map
	again, _ := s.Base("p", "f")
	if again.Clock["m1"] != 1 {
		t.Errorf("Base returned aliased internal clock: %v", again.Clock)
	}
	bases := s.Bases("p")
	delete(bases, "f")
	if _, ok := s.Base("p", "f"); !ok {
		t.Error("Bases returned the internal map")
	}
	// Ignored inputs never panic.
	s.SetBase("", "f", BaseEntry{})
	s.SetBase("p", "", BaseEntry{})
	s.SetJournalSeq("", 1)
	if len(s.Projects()) != 1 || len(s.JournalSeqs()) != 0 {
		t.Errorf("empty keys were recorded: %v %v", s.Projects(), s.JournalSeqs())
	}
	var nilStore *Store
	if _, ok := nilStore.Base("p", "f"); ok {
		t.Error("nil store Base")
	}
	nilStore.SetBase("p", "f", BaseEntry{})
	nilStore.DeleteBase("p", "f")
	nilStore.DeleteProject("p")
	nilStore.SetJournalSeq("m", 1)
	if nilStore.JournalSeq("m") != 0 || len(nilStore.Bases("p")) != 0 {
		t.Error("nil store returned data")
	}
	if err := nilStore.Save(); err == nil {
		t.Error("nil store Save: want error")
	}
}

func TestDeleteBaseAndProject(t *testing.T) {
	stateDir := t.TempDir()
	s, err := Open(stateDir, "v")
	if err != nil {
		t.Fatal(err)
	}
	s.SetBase("p1", "a", BaseEntry{Blob: "1"})
	s.SetBase("p1", "b", BaseEntry{Blob: "2"})
	s.SetBase("p2", "a", BaseEntry{Blob: "3"})
	s.SetJournalSeq("m", 4)

	s.DeleteBase("p1", "a")
	s.DeleteBase("p1", "missing") // no-op
	s.DeleteBase("nope", "a")     // no-op
	if _, ok := s.Base("p1", "a"); ok {
		t.Error("p1/a still present after DeleteBase")
	}
	if _, ok := s.Base("p1", "b"); !ok {
		t.Error("DeleteBase removed a sibling")
	}
	if _, ok := s.Base("p2", "a"); !ok {
		t.Error("DeleteBase touched another project")
	}
	s.DeleteBase("p1", "b")
	if got := s.Projects(); strings.Join(got, ",") != "p2" {
		t.Errorf("empty project not dropped: %v", got)
	}

	s.SetBase("p1", "a", BaseEntry{Blob: "1"})
	s.SetBase("p1", "c", BaseEntry{Blob: "5"})
	s.DeleteProject("p1")
	s.DeleteProject("unknown") // no-op
	if n := len(s.Bases("p1")); n != 0 {
		t.Errorf("DeleteProject left %d bases", n)
	}
	if _, ok := s.Base("p2", "a"); !ok {
		t.Error("DeleteProject touched another project")
	}
	if s.JournalSeq("m") != 4 {
		t.Error("DeleteProject touched journals")
	}
	if err := s.Save(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(stateDir, "v")
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Bases("p1")) != 0 || len(s2.Bases("p2")) != 1 {
		t.Errorf("after reload: p1=%v p2=%v", s2.Bases("p1"), s2.Bases("p2"))
	}
}

func TestOpenErrors(t *testing.T) {
	stateDir := t.TempDir()
	dir := filepath.Join(stateDir, "vaults", "bad")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "base.json"), []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(stateDir, "bad"); err == nil {
		t.Error("Open on corrupt base.json: want error")
	}
	for _, id := range []string{"", ".", "..", "a/b", `a\b`} {
		if _, err := Open(stateDir, id); err == nil {
			t.Errorf("Open(%q): want error", id)
		}
	}
	if _, err := Open("", "v"); err == nil {
		t.Error("Open with empty state dir: want error")
	}
	// Empty-but-valid JSON documents open as empty stores.
	for _, content := range []string{"{}", `{"projects":null,"journals":null}`, `{"projects":{"p":{}},"journals":{}}`} {
		d := filepath.Join(stateDir, "vaults", "empty")
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "base.json"), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		s, err := Open(stateDir, "empty")
		if err != nil {
			t.Errorf("Open(%s): %v", content, err)
			continue
		}
		if len(s.Projects()) != 0 {
			t.Errorf("Open(%s): projects = %v", content, s.Projects())
		}
		if err := s.Save(); err != nil {
			t.Errorf("Save after %s: %v", content, err)
		}
	}
}

// --- trash ------------------------------------------------------------------

func TestTrashPutListRead(t *testing.T) {
	k := testKeys(t)
	s, err := Open(t.TempDir(), "v")
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.TrashList()
	if err != nil {
		t.Fatalf("TrashList on empty: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("empty trash lists %d entries", len(list))
	}

	contents := [][]byte{
		[]byte("SECRET=one\n"),
		[]byte("SECRET=two\n"),
		{},
		bytes.Repeat([]byte{0, 1, 2, 255}, 1000),
	}
	var puts []TrashEntry
	for i, c := range contents {
		e, err := s.TrashPut(k, "proj", "path/"+string(rune('a'+i)), c, 0o640)
		if err != nil {
			t.Fatalf("TrashPut #%d: %v", i, err)
		}
		if len(e.ID) != 12 {
			t.Errorf("id %q: want 12 hex chars", e.ID)
		}
		if e.Blob != k.BlobID(c) {
			t.Errorf("blob = %s want %s", e.Blob, k.BlobID(c))
		}
		if e.Size != int64(len(c)) || e.Mode != 0o640 || e.Project != "proj" {
			t.Errorf("entry = %+v", e)
		}
		if e.Time.IsZero() {
			t.Error("zero time")
		}
		blobPath := filepath.Join(s.Dir(), "trash", e.Blob[:2], e.Blob+".enc")
		assertPerm(t, blobPath, 0o600)
		assertPerm(t, filepath.Dir(blobPath), 0o700)
		enc, err := os.ReadFile(blobPath)
		if err != nil {
			t.Fatalf("blob file: %v", err)
		}
		if len(c) > 0 && bytes.Contains(enc, c) {
			t.Error("plaintext found inside trash blob")
		}
		puts = append(puts, e)
		time.Sleep(2 * time.Millisecond) // distinct timestamps for ordering
	}
	assertPerm(t, filepath.Join(s.Dir(), "trash"), 0o700)
	assertPerm(t, s.trashIndexPath(), 0o600)

	// Ids are unique.
	seen := map[string]bool{}
	for _, e := range puts {
		if seen[e.ID] {
			t.Errorf("duplicate id %s", e.ID)
		}
		seen[e.ID] = true
	}

	// List newest first.
	list, err = s.TrashList()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != len(puts) {
		t.Fatalf("TrashList = %d entries want %d", len(list), len(puts))
	}
	for i := range list {
		want := puts[len(puts)-1-i]
		if list[i].ID != want.ID {
			t.Errorf("list[%d] = %s want %s (newest first)", i, list[i].ID, want.ID)
		}
		if i > 0 && list[i].Time.After(list[i-1].Time) {
			t.Errorf("list not sorted newest first at %d", i)
		}
	}

	// Read round trip.
	for i, e := range puts {
		got, ge, err := s.TrashRead(k, e.ID)
		if err != nil {
			t.Fatalf("TrashRead(%s): %v", e.ID, err)
		}
		if !bytes.Equal(got, contents[i]) {
			t.Errorf("TrashRead(%s) content mismatch", e.ID)
		}
		if ge.ID != e.ID || ge.Path != e.Path || ge.Blob != e.Blob || ge.Mode != e.Mode || ge.Size != e.Size {
			t.Errorf("TrashRead entry = %+v want %+v", ge, e)
		}
	}
	// Unknown id / wrong key.
	if _, _, err := s.TrashRead(k, "000000000000"); !errors.Is(err, ErrTrashNotFound) {
		t.Errorf("unknown id: %v want ErrTrashNotFound", err)
	}
	if _, _, err := s.TrashRead(k, ""); !errors.Is(err, ErrTrashNotFound) {
		t.Errorf("empty id: %v want ErrTrashNotFound", err)
	}
	other, _ := crypto.NewKeys(bytes.Repeat([]byte{7}, 32))
	if _, _, err := s.TrashRead(other, puts[0].ID); err == nil {
		t.Error("TrashRead with wrong key: want error")
	}
	if _, err := s.TrashPut(nil, "p", "x", []byte("a"), 0); err == nil {
		t.Error("TrashPut nil keys: want error")
	}
	if _, _, err := s.TrashRead(nil, puts[0].ID); err == nil {
		t.Error("TrashRead nil keys: want error")
	}

	// A fresh Store handle sees the same trash.
	s2, err := Open(filepath.Dir(filepath.Dir(s.Dir())), "v")
	if err != nil {
		t.Fatal(err)
	}
	if l, _ := s2.TrashList(); len(l) != len(puts) {
		t.Errorf("reopened store lists %d entries", len(l))
	}
}

func TestTrashDedupe(t *testing.T) {
	k := testKeys(t)
	s, err := Open(t.TempDir(), "v")
	if err != nil {
		t.Fatal(err)
	}
	content := []byte("same content\n")
	e1, err := s.TrashPut(k, "p1", "a", content, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	e2, err := s.TrashPut(k, "p2", "b", content, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if e1.ID == e2.ID {
		t.Error("identical content produced identical entry ids")
	}
	if e1.Blob != e2.Blob {
		t.Errorf("identical content produced different blobs: %s vs %s", e1.Blob, e2.Blob)
	}
	var files []string
	err = filepath.WalkDir(filepath.Join(s.Dir(), "trash"), func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(p, ".enc") {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Errorf("dedupe: %d blob files, want 1: %v", len(files), files)
	}
	list, _ := s.TrashList()
	if len(list) != 2 {
		t.Errorf("index has %d rows want 2", len(list))
	}
	for _, e := range []TrashEntry{e1, e2} {
		got, ge, err := s.TrashRead(k, e.ID)
		if err != nil || !bytes.Equal(got, content) {
			t.Errorf("TrashRead(%s): %v", e.ID, err)
		}
		if ge.Mode != e.Mode || ge.Project != e.Project {
			t.Errorf("entry metadata lost: %+v", ge)
		}
	}
}

func TestTrashPurge(t *testing.T) {
	k := testKeys(t)
	s, err := Open(t.TempDir(), "v")
	if err != nil {
		t.Fatal(err)
	}
	shared := []byte("shared\n")
	oldOnly := []byte("old only\n")
	fresh := []byte("fresh\n")

	eOldShared := mustPut(t, s, k, "p", "s1", shared, 0o600)
	eNewShared := mustPut(t, s, k, "p", "s2", shared, 0o600)
	eOld := mustPut(t, s, k, "p", "o", oldOnly, 0o600)
	eOld2 := mustPut(t, s, k, "p", "o2", oldOnly, 0o600) // same blob, also old
	eFresh := mustPut(t, s, k, "p", "f", fresh, 0o600)

	// Backdate.
	entries := readIndex(t, s)
	now := time.Now().UTC()
	for i := range entries {
		switch entries[i].ID {
		case eOldShared.ID, eOld.ID:
			entries[i].Time = now.Add(-40 * 24 * time.Hour)
		case eOld2.ID:
			entries[i].Time = now.Add(-31 * 24 * time.Hour)
		case eNewShared.ID:
			entries[i].Time = now.Add(-2 * 24 * time.Hour)
		}
	}
	writeIndex(t, s, entries)

	blobPath := func(e TrashEntry) string {
		return filepath.Join(s.Dir(), "trash", e.Blob[:2], e.Blob+".enc")
	}

	// Nothing older than 60 days.
	n, err := s.TrashPurge(60 * 24 * time.Hour)
	if err != nil || n != 0 {
		t.Fatalf("purge 60d: n=%d err=%v", n, err)
	}
	if l, _ := s.TrashList(); len(l) != 5 {
		t.Fatalf("purge 60d removed rows: %d left", len(l))
	}

	n, err = s.TrashPurge(30 * 24 * time.Hour)
	if err != nil {
		t.Fatalf("purge 30d: %v", err)
	}
	if n != 3 {
		t.Errorf("purge 30d removed %d want 3", n)
	}
	list, _ := s.TrashList()
	ids := map[string]bool{}
	for _, e := range list {
		ids[e.ID] = true
	}
	if len(list) != 2 || !ids[eNewShared.ID] || !ids[eFresh.ID] {
		t.Errorf("remaining after purge: %+v", list)
	}
	if _, err := os.Stat(blobPath(eOld)); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("unreferenced blob still present: %v", err)
	}
	// The shard directory is deliberately left in place (removing it could
	// race a concurrent TrashPut in another process); it must still be a
	// usable directory, empty or not.
	if st, err := os.Stat(filepath.Dir(blobPath(eOld))); err != nil || !st.IsDir() {
		t.Errorf("shard dir of purged blob: %v", err)
	}
	if _, err := os.Stat(blobPath(eNewShared)); err != nil {
		t.Errorf("shared blob deleted although still referenced: %v", err)
	}
	if _, err := os.Stat(blobPath(eFresh)); err != nil {
		t.Errorf("fresh blob deleted: %v", err)
	}
	// Remaining entries still readable.
	for _, e := range []TrashEntry{eNewShared, eFresh} {
		if _, _, err := s.TrashRead(k, e.ID); err != nil {
			t.Errorf("TrashRead(%s) after purge: %v", e.ID, err)
		}
	}
	if _, _, err := s.TrashRead(k, eOldShared.ID); !errors.Is(err, ErrTrashNotFound) {
		t.Errorf("purged entry still readable: %v", err)
	}

	// Purge everything.
	n, err = s.TrashPurge(0)
	if err != nil || n != 2 {
		t.Fatalf("purge all: n=%d err=%v", n, err)
	}
	if l, _ := s.TrashList(); len(l) != 0 {
		t.Errorf("entries left after purge all: %d", len(l))
	}
	for _, e := range []TrashEntry{eNewShared, eFresh} {
		if _, err := os.Stat(blobPath(e)); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("blob %s not deleted: %v", e.Blob, err)
		}
	}
	// Idempotent on empty.
	if n, err := s.TrashPurge(0); err != nil || n != 0 {
		t.Errorf("purge empty: n=%d err=%v", n, err)
	}
}

func TestTrashPurgeMissingBlobFile(t *testing.T) {
	k := testKeys(t)
	s, err := Open(t.TempDir(), "v")
	if err != nil {
		t.Fatal(err)
	}
	e, err := s.TrashPut(k, "p", "x", []byte("x"), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.Dir(), "trash", e.Blob[:2], e.Blob+".enc")); err != nil {
		t.Fatal(err)
	}
	entries := readIndex(t, s)
	entries[0].Time = time.Now().Add(-time.Hour)
	writeIndex(t, s, entries)
	if n, err := s.TrashPurge(time.Minute); err != nil || n != 1 {
		t.Errorf("purge with missing blob: n=%d err=%v", n, err)
	}
	// Reading an entry whose blob is missing is an error, not a panic.
	e2 := mustPut(t, s, k, "p", "y", []byte("y"), 0o600)
	_ = os.Remove(filepath.Join(s.Dir(), "trash", e2.Blob[:2], e2.Blob+".enc"))
	if _, _, err := s.TrashRead(k, e2.ID); err == nil {
		t.Error("TrashRead with missing blob file: want error")
	}
}

func TestTrashCorruptIndex(t *testing.T) {
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
	if _, err := s.TrashList(); err == nil {
		t.Error("TrashList corrupt index: want error")
	}
	if _, err := s.TrashPut(k, "p", "x", []byte("x"), 0); err == nil {
		t.Error("TrashPut corrupt index: want error")
	}
	if _, _, err := s.TrashRead(k, "abc"); err == nil {
		t.Error("TrashRead corrupt index: want error")
	}
	if _, err := s.TrashPurge(0); err == nil {
		t.Error("TrashPurge corrupt index: want error")
	}
	// Entries with a bogus blob id in the index are rejected safely.
	writeIndex(t, s, []TrashEntry{{ID: "abc", Blob: "../../etc/passwd", Time: time.Now()}})
	if _, _, err := s.TrashRead(k, "abc"); err == nil {
		t.Error("TrashRead bogus blob id: want error")
	}
	if n, err := s.TrashPurge(0); err != nil || n != 1 {
		t.Errorf("TrashPurge bogus blob id: n=%d err=%v", n, err)
	}
	// Empty index file is an empty trash.
	if err := os.WriteFile(s.trashIndexPath(), []byte(" \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if l, err := s.TrashList(); err != nil || len(l) != 0 {
		t.Errorf("blank index: %v %v", l, err)
	}
}

func TestNilStoreTrash(t *testing.T) {
	var s *Store
	k := testKeys(t)
	if _, err := s.TrashPut(k, "p", "x", nil, 0); err == nil {
		t.Error("nil TrashPut")
	}
	if _, err := s.TrashList(); err == nil {
		t.Error("nil TrashList")
	}
	if _, _, err := s.TrashRead(k, "x"); err == nil {
		t.Error("nil TrashRead")
	}
	if _, err := s.TrashPurge(0); err == nil {
		t.Error("nil TrashPurge")
	}
}
