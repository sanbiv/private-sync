// Package state keeps the per-machine local state (spec §9.4): machine identity,
// vault pins, the sync base store and the encrypted trash.
//
// Layout below the state directory (dirs 0700, files 0600):
//
//	machine.json                         {id, created_at, vaults: {<abs vault path>: <vaultID>}}
//	vaults/<vaultID>/base.json           {projects: {projectID: {relpath: BaseEntry}}, journals: {machineID: maxSeq}}
//	vaults/<vaultID>/trash/index.json    [TrashEntry, ...]
//	vaults/<vaultID>/trash/<aa>/<blob>.enc   deterministic blob ciphertext (crypto.Keys.SealBlob)
//
// Every write goes through fsutil.WriteFileAtomic so a crash never leaves a
// half-written file behind. Plaintext never touches the state directory: trash
// pre-images are stored as ciphertext only.
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/fsutil"
	"github.com/sanbiv/private-sync/internal/vault"
)

const (
	// machineFile is the name of the machine identity file inside the state dir.
	machineFile = "machine.json"
	// vaultsDir holds one sub directory per vault id.
	vaultsDir = "vaults"
	// baseFile is the per-vault base store.
	baseFile = "base.json"
	// trashDir holds the encrypted trash of a vault.
	trashDir = "trash"
	// trashIndexFile is the trash index inside trashDir.
	trashIndexFile = "index.json"
	// trashBlobExt is the extension of encrypted trash blobs.
	trashBlobExt = ".enc"
	// writeSuffix identifies this package's temp files (fsutil suffix).
	writeSuffix = "state"
	// trashIDBytes is the number of random bytes in a trash entry id (12 hex chars).
	trashIDBytes = 6

	dirMode  fs.FileMode = 0o700
	fileMode fs.FileMode = 0o600
)

// Machine is this computer's identity (never stored in config.yaml).
type Machine struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrVaultMismatch is returned when a vault id differs from the pinned one.
var ErrVaultMismatch = errors.New("the vault at this path has a different id than the one previously used on this machine")

// machineDoc is the on-disk shape of machine.json.
type machineDoc struct {
	ID        string            `json:"id"`
	CreatedAt time.Time         `json:"created_at"`
	Vaults    map[string]string `json:"vaults"`
}

// machineMu serialises read-modify-write cycles on machine.json within this
// process (LoadMachine on first use, PinVault).
var machineMu sync.Mutex

// machinePath returns <stateDir>/machine.json.
func machinePath(stateDir string) string { return filepath.Join(stateDir, machineFile) }

// readMachineDoc reads machine.json. A missing file yields (doc, false, nil);
// a corrupt or invalid file yields an error.
func readMachineDoc(stateDir string) (machineDoc, bool, error) {
	var doc machineDoc
	data, err := os.ReadFile(machinePath(stateDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return doc, false, nil
		}
		return doc, false, fmt.Errorf("state: read %s: %w", machineFile, err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		return doc, false, fmt.Errorf("state: parse %s: %w", machinePath(stateDir), err)
	}
	if strings.TrimSpace(doc.ID) == "" {
		return doc, false, fmt.Errorf("state: %s has an empty machine id", machinePath(stateDir))
	}
	if doc.Vaults == nil {
		doc.Vaults = map[string]string{}
	}
	return doc, true, nil
}

// writeMachineDoc atomically writes machine.json (dir 0700, file 0600).
func writeMachineDoc(stateDir string, doc machineDoc) error {
	if doc.Vaults == nil {
		doc.Vaults = map[string]string{}
	}
	if err := ensureDir(stateDir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("state: encode %s: %w", machineFile, err)
	}
	data = append(data, '\n')
	if err := fsutil.WriteFileAtomic(machinePath(stateDir), data, fileMode, writeSuffix); err != nil {
		return fmt.Errorf("state: write %s: %w", machineFile, err)
	}
	return nil
}

// loadOrCreateMachineDoc returns the machine document, generating a fresh
// identity on first use. Callers must hold machineMu.
func loadOrCreateMachineDoc(stateDir string) (machineDoc, error) {
	doc, ok, err := readMachineDoc(stateDir)
	if err != nil {
		return machineDoc{}, err
	}
	if ok {
		return doc, nil
	}
	id, err := uuid.NewRandom()
	if err != nil {
		return machineDoc{}, fmt.Errorf("state: generate machine id: %w", err)
	}
	doc = machineDoc{
		ID:        id.String(),
		CreatedAt: time.Now().UTC().Truncate(time.Second),
		Vaults:    map[string]string{},
	}
	if err := writeMachineDoc(stateDir, doc); err != nil {
		return machineDoc{}, err
	}
	return doc, nil
}

// LoadMachine loads or creates <stateDir>/machine.json.
func LoadMachine(stateDir string) (Machine, error) {
	if stateDir == "" {
		return Machine{}, errors.New("state.LoadMachine: empty state dir")
	}
	machineMu.Lock()
	defer machineMu.Unlock()
	doc, err := loadOrCreateMachineDoc(stateDir)
	if err != nil {
		return Machine{}, err
	}
	return Machine{ID: doc.ID, CreatedAt: doc.CreatedAt}, nil
}

// cleanVaultPath normalises a vault path for use as a pin key.
func cleanVaultPath(vaultPath string) string {
	if vaultPath == "" {
		return ""
	}
	return filepath.Clean(vaultPath)
}

// PinnedVault returns the vault id pinned for an absolute vault path.
func PinnedVault(stateDir, vaultPath string) (string, bool, error) {
	if stateDir == "" {
		return "", false, errors.New("state.PinnedVault: empty state dir")
	}
	key := cleanVaultPath(vaultPath)
	if key == "" {
		return "", false, errors.New("state.PinnedVault: empty vault path")
	}
	machineMu.Lock()
	defer machineMu.Unlock()
	doc, ok, err := readMachineDoc(stateDir)
	if err != nil {
		return "", false, err
	}
	if !ok {
		return "", false, nil
	}
	id, found := doc.Vaults[key]
	if !found || id == "" {
		return "", false, nil
	}
	return id, true, nil
}

// PinVault records the vault id for a path.
func PinVault(stateDir, vaultPath, vaultID string) error {
	if stateDir == "" {
		return errors.New("state.PinVault: empty state dir")
	}
	key := cleanVaultPath(vaultPath)
	if key == "" {
		return errors.New("state.PinVault: empty vault path")
	}
	if vaultID == "" {
		return errors.New("state.PinVault: empty vault id")
	}
	machineMu.Lock()
	defer machineMu.Unlock()
	doc, err := loadOrCreateMachineDoc(stateDir)
	if err != nil {
		return err
	}
	if cur, ok := doc.Vaults[key]; ok && cur == vaultID {
		return nil
	}
	doc.Vaults[key] = vaultID
	return writeMachineDoc(stateDir, doc)
}

// UnpinVault forgets the pin for a path. Unknown paths are not an error.
func UnpinVault(stateDir, vaultPath string) error {
	if stateDir == "" {
		return errors.New("state.UnpinVault: empty state dir")
	}
	key := cleanVaultPath(vaultPath)
	if key == "" {
		return errors.New("state.UnpinVault: empty vault path")
	}
	machineMu.Lock()
	defer machineMu.Unlock()
	doc, ok, err := readMachineDoc(stateDir)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if _, found := doc.Vaults[key]; !found {
		return nil
	}
	delete(doc.Vaults, key)
	return writeMachineDoc(stateDir, doc)
}

// CheckPin verifies vaultID against the pin for vaultPath: it returns
// ErrVaultMismatch when a different id is pinned and pins the id when no pin
// exists yet.
func CheckPin(stateDir, vaultPath, vaultID string) error {
	pinned, ok, err := PinnedVault(stateDir, vaultPath)
	if err != nil {
		return err
	}
	if ok {
		if pinned != vaultID {
			return fmt.Errorf("%w (pinned %s, found %s)", ErrVaultMismatch, pinned, vaultID)
		}
		return nil
	}
	return PinVault(stateDir, vaultPath, vaultID)
}

// BaseEntry is the entry this machine last converged to for a path.
type BaseEntry struct {
	Blob  string      `json:"blob,omitempty"`
	Kind  vault.Kind  `json:"kind"`
	Clock vault.Clock `json:"clock"`
}

// clone returns a deep copy (the clock map is copied).
func (b BaseEntry) clone() BaseEntry {
	if b.Clock != nil {
		c := make(vault.Clock, len(b.Clock))
		for k, v := range b.Clock {
			c[k] = v
		}
		b.Clock = c
	}
	return b
}

// baseDoc is the on-disk shape of base.json.
type baseDoc struct {
	Projects map[string]map[string]BaseEntry `json:"projects"`
	Journals map[string]uint64               `json:"journals"`
}

// Store is the per-vault base store + trash (<stateDir>/vaults/<vaultID>/).
type Store struct {
	dir string

	mu       sync.Mutex
	projects map[string]map[string]BaseEntry
	journals map[string]uint64
}

// validVaultID rejects ids that could escape the vaults directory.
func validVaultID(id string) error {
	switch {
	case id == "":
		return errors.New("empty vault id")
	case id == "." || id == "..":
		return fmt.Errorf("invalid vault id %q", id)
	case strings.ContainsAny(id, `/\`):
		return fmt.Errorf("invalid vault id %q: must not contain path separators", id)
	case strings.ContainsRune(id, 0):
		return fmt.Errorf("invalid vault id: contains NUL")
	}
	return nil
}

// Open loads (or initialises) the store for a vault.
func Open(stateDir, vaultID string) (*Store, error) {
	if stateDir == "" {
		return nil, errors.New("state.Open: empty state dir")
	}
	if err := validVaultID(vaultID); err != nil {
		return nil, fmt.Errorf("state.Open: %w", err)
	}
	dir := filepath.Join(stateDir, vaultsDir, vaultID)
	if err := ensureDir(dir); err != nil {
		return nil, fmt.Errorf("state.Open: %w", err)
	}
	s := &Store{
		dir:      dir,
		projects: map[string]map[string]BaseEntry{},
		journals: map[string]uint64{},
	}
	data, err := os.ReadFile(s.basePath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return s, nil
		}
		return nil, fmt.Errorf("state.Open: read %s: %w", baseFile, err)
	}
	var doc baseDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("state.Open: parse %s: %w", s.basePath(), err)
	}
	for project, bases := range doc.Projects {
		if project == "" || len(bases) == 0 {
			continue
		}
		m := make(map[string]BaseEntry, len(bases))
		for p, b := range bases {
			m[p] = b
		}
		s.projects[project] = m
	}
	for machine, seq := range doc.Journals {
		if machine == "" {
			continue
		}
		s.journals[machine] = seq
	}
	return s, nil
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) basePath() string       { return filepath.Join(s.dir, baseFile) }
func (s *Store) trashPath() string      { return filepath.Join(s.dir, trashDir) }
func (s *Store) trashIndexPath() string { return filepath.Join(s.trashPath(), trashIndexFile) }

// Base returns the base entry for a path of a project.
func (s *Store) Base(project, path string) (BaseEntry, bool) {
	if s == nil {
		return BaseEntry{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.projects[project][path]
	if !ok {
		return BaseEntry{}, false
	}
	return b.clone(), true
}

// SetBase records the base entry for a path of a project.
func (s *Store) SetBase(project, path string, b BaseEntry) {
	if s == nil || project == "" || path == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.projects[project]
	if m == nil {
		m = map[string]BaseEntry{}
		s.projects[project] = m
	}
	m[path] = b.clone()
}

// DeleteBase forgets the base entry for a path of a project.
func (s *Store) DeleteBase(project, path string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.projects[project]
	if !ok {
		return
	}
	delete(m, path)
	if len(m) == 0 {
		delete(s.projects, project)
	}
}

// Bases returns a copy of all base entries of a project (empty map when none).
func (s *Store) Bases(project string) map[string]BaseEntry {
	out := map[string]BaseEntry{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for p, b := range s.projects[project] {
		out[p] = b.clone()
	}
	return out
}

// Projects returns the ids of all projects with at least one base, sorted.
func (s *Store) Projects() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.projects))
	for p := range s.projects {
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// DeleteProject forgets every base entry of a project.
func (s *Store) DeleteProject(project string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.projects, project)
}

// JournalSeq returns the highest journal sequence applied for a machine (0 when unknown).
func (s *Store) JournalSeq(machine string) uint64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.journals[machine]
}

// SetJournalSeq records the highest journal sequence applied for a machine.
func (s *Store) SetJournalSeq(machine string, seq uint64) {
	if s == nil || machine == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.journals[machine] = seq
}

// JournalSeqs returns a copy of every recorded journal sequence.
func (s *Store) JournalSeqs() map[string]uint64 {
	out := map[string]uint64{}
	if s == nil {
		return out
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for m, seq := range s.journals {
		out[m] = seq
	}
	return out
}

// Save writes base.json atomically (0600). Map keys are emitted sorted by
// encoding/json, so the output is stable for identical state.
func (s *Store) Save() error {
	if s == nil {
		return errors.New("state.Save: nil store")
	}
	s.mu.Lock()
	doc := baseDoc{
		Projects: make(map[string]map[string]BaseEntry, len(s.projects)),
		Journals: make(map[string]uint64, len(s.journals)),
	}
	for project, bases := range s.projects {
		if len(bases) == 0 {
			continue
		}
		m := make(map[string]BaseEntry, len(bases))
		for p, b := range bases {
			m[p] = b.clone()
		}
		doc.Projects[project] = m
	}
	for m, seq := range s.journals {
		doc.Journals[m] = seq
	}
	s.mu.Unlock()

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("state.Save: encode: %w", err)
	}
	data = append(data, '\n')
	if err := ensureDir(s.dir); err != nil {
		return fmt.Errorf("state.Save: %w", err)
	}
	if err := fsutil.WriteFileAtomic(s.basePath(), data, fileMode, writeSuffix); err != nil {
		return fmt.Errorf("state.Save: write %s: %w", baseFile, err)
	}
	return nil
}

// TrashEntry indexes one encrypted pre-image.
type TrashEntry struct {
	ID      string    `json:"id"`
	Time    time.Time `json:"time"`
	Project string    `json:"project"`
	Path    string    `json:"path"`
	Blob    string    `json:"blob"`
	Mode    uint32    `json:"mode"`
	Size    int64     `json:"size"`
}

// trashBlobPath returns trash/<blob[:2]>/<blob>.enc, or an error for ids that
// are too short or could escape the trash directory.
func (s *Store) trashBlobPath(blob string) (string, error) {
	if len(blob) < 2 {
		return "", fmt.Errorf("invalid trash blob id %q", blob)
	}
	for _, r := range blob {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return "", fmt.Errorf("invalid trash blob id %q: not hex", blob)
		}
	}
	return filepath.Join(s.trashPath(), blob[:2], blob+trashBlobExt), nil
}

// readTrashIndex loads the trash index; a missing index is empty.
func (s *Store) readTrashIndex() ([]TrashEntry, error) {
	data, err := os.ReadFile(s.trashIndexPath())
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return []TrashEntry{}, nil
		}
		return nil, fmt.Errorf("read trash index: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return []TrashEntry{}, nil
	}
	var entries []TrashEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("parse %s: %w", s.trashIndexPath(), err)
	}
	if entries == nil {
		entries = []TrashEntry{}
	}
	return entries, nil
}

// writeTrashIndex atomically writes the trash index (0600).
func (s *Store) writeTrashIndex(entries []TrashEntry) error {
	if entries == nil {
		entries = []TrashEntry{}
	}
	data, err := json.MarshalIndent(entries, "", "  ")
	if err != nil {
		return fmt.Errorf("encode trash index: %w", err)
	}
	data = append(data, '\n')
	if err := ensureDir(s.trashPath()); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(s.trashIndexPath(), data, fileMode, writeSuffix); err != nil {
		return fmt.Errorf("write trash index: %w", err)
	}
	return nil
}

// sortTrash orders entries newest first (ties broken by id for stability).
func sortTrash(entries []TrashEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		if !entries[i].Time.Equal(entries[j].Time) {
			return entries[i].Time.After(entries[j].Time)
		}
		return entries[i].ID > entries[j].ID
	})
}

// TrashPut stores content encrypted (deterministic blob format) and indexes it.
func (s *Store) TrashPut(k *crypto.Keys, project, path string, content []byte, mode uint32) (TrashEntry, error) {
	if s == nil {
		return TrashEntry{}, errors.New("state.TrashPut: nil store")
	}
	if k == nil {
		return TrashEntry{}, errors.New("state.TrashPut: nil keys")
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	blob, ciphertext, err := k.SealBlob(content)
	if err != nil {
		return TrashEntry{}, fmt.Errorf("state.TrashPut: %w", err)
	}
	blobPath, err := s.trashBlobPath(blob)
	if err != nil {
		return TrashEntry{}, fmt.Errorf("state.TrashPut: %w", err)
	}
	if err := ensureDir(filepath.Dir(blobPath)); err != nil {
		return TrashEntry{}, fmt.Errorf("state.TrashPut: %w", err)
	}
	// Identical pre-images share one file: the format is deterministic.
	if !fsutil.Exists(blobPath) {
		if err := fsutil.WriteFileAtomic(blobPath, ciphertext, fileMode, writeSuffix); err != nil {
			return TrashEntry{}, fmt.Errorf("state.TrashPut: write blob: %w", err)
		}
	}

	entries, err := s.readTrashIndex()
	if err != nil {
		return TrashEntry{}, fmt.Errorf("state.TrashPut: %w", err)
	}
	used := make(map[string]bool, len(entries))
	for _, e := range entries {
		used[e.ID] = true
	}
	var id string
	for attempt := 0; attempt < 16; attempt++ {
		id, err = crypto.RandomHex(trashIDBytes)
		if err != nil {
			return TrashEntry{}, fmt.Errorf("state.TrashPut: %w", err)
		}
		if !used[id] {
			break
		}
		id = ""
	}
	if id == "" {
		return TrashEntry{}, errors.New("state.TrashPut: could not allocate a unique trash id")
	}
	entry := TrashEntry{
		ID:      id,
		Time:    time.Now().UTC(),
		Project: project,
		Path:    path,
		Blob:    blob,
		Mode:    mode,
		Size:    int64(len(content)),
	}
	entries = append(entries, entry)
	if err := s.writeTrashIndex(entries); err != nil {
		return TrashEntry{}, fmt.Errorf("state.TrashPut: %w", err)
	}
	return entry, nil
}

// TrashList lists entries, newest first.
func (s *Store) TrashList() ([]TrashEntry, error) {
	if s == nil {
		return nil, errors.New("state.TrashList: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readTrashIndex()
	if err != nil {
		return nil, fmt.Errorf("state.TrashList: %w", err)
	}
	sortTrash(entries)
	return entries, nil
}

// ErrTrashNotFound is returned by TrashRead for an unknown entry id.
var ErrTrashNotFound = errors.New("trash entry not found")

// TrashRead decrypts one entry.
func (s *Store) TrashRead(k *crypto.Keys, id string) ([]byte, TrashEntry, error) {
	if s == nil {
		return nil, TrashEntry{}, errors.New("state.TrashRead: nil store")
	}
	if k == nil {
		return nil, TrashEntry{}, errors.New("state.TrashRead: nil keys")
	}
	if id == "" {
		return nil, TrashEntry{}, fmt.Errorf("state.TrashRead: %w: empty id", ErrTrashNotFound)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readTrashIndex()
	if err != nil {
		return nil, TrashEntry{}, fmt.Errorf("state.TrashRead: %w", err)
	}
	var entry TrashEntry
	found := false
	for _, e := range entries {
		if e.ID == id {
			entry, found = e, true
			break
		}
	}
	if !found {
		return nil, TrashEntry{}, fmt.Errorf("state.TrashRead: %w: %s", ErrTrashNotFound, id)
	}
	blobPath, err := s.trashBlobPath(entry.Blob)
	if err != nil {
		return nil, TrashEntry{}, fmt.Errorf("state.TrashRead: %w", err)
	}
	ciphertext, err := os.ReadFile(blobPath)
	if err != nil {
		return nil, TrashEntry{}, fmt.Errorf("state.TrashRead: read blob %s: %w", entry.Blob, err)
	}
	content, err := k.OpenBlob(entry.Blob, ciphertext)
	if err != nil {
		return nil, TrashEntry{}, fmt.Errorf("state.TrashRead: %w", err)
	}
	return content, entry, nil
}

// TrashPurge removes entries older than the duration; returns the count removed.
// Blob files no longer referenced by any remaining entry are deleted too.
func (s *Store) TrashPurge(olderThan time.Duration) (int, error) {
	if s == nil {
		return 0, errors.New("state.TrashPurge: nil store")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := s.readTrashIndex()
	if err != nil {
		return 0, fmt.Errorf("state.TrashPurge: %w", err)
	}
	if len(entries) == 0 {
		return 0, nil
	}
	cutoff := time.Now().Add(-olderThan)
	keep := make([]TrashEntry, 0, len(entries))
	var removed []TrashEntry
	for _, e := range entries {
		if e.Time.Before(cutoff) {
			removed = append(removed, e)
		} else {
			keep = append(keep, e)
		}
	}
	if len(removed) == 0 {
		return 0, nil
	}
	// Write the index first: a blob left behind is harmless, a dangling index row is not.
	if err := s.writeTrashIndex(keep); err != nil {
		return 0, fmt.Errorf("state.TrashPurge: %w", err)
	}
	referenced := make(map[string]bool, len(keep))
	for _, e := range keep {
		referenced[e.Blob] = true
	}
	var firstErr error
	deleted := map[string]bool{}
	for _, e := range removed {
		if referenced[e.Blob] || deleted[e.Blob] {
			continue
		}
		deleted[e.Blob] = true
		blobPath, err := s.trashBlobPath(e.Blob)
		if err != nil {
			continue // never indexed a file for this id
		}
		if err := os.Remove(blobPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if firstErr == nil {
				firstErr = fmt.Errorf("state.TrashPurge: remove blob %s: %w", e.Blob, err)
			}
			continue
		}
		// Drop the shard directory once empty (ignore failures: it may hold other blobs).
		_ = os.Remove(filepath.Dir(blobPath))
	}
	return len(removed), firstErr
}

// ensureDir creates dir with mode 0700 and tightens the mode of an existing one.
func ensureDir(dir string) error {
	if err := fsutil.EnsureDir(dir); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if st, err := os.Stat(dir); err == nil && st.Mode().Perm() != dirMode {
		_ = os.Chmod(dir, dirMode)
	}
	return nil
}
