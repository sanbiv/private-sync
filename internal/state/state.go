// Package state keeps the per-machine local state (spec §9.4): machine identity,
// vault pins, the sync base store and the encrypted trash.
package state

import (
	"errors"
	"time"

	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/vault"
)

// Machine is this computer's identity (never stored in config.yaml).
type Machine struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
}

// ErrVaultMismatch is returned when a vault id differs from the pinned one.
var ErrVaultMismatch = errors.New("the vault at this path has a different id than the one previously used on this machine")

// LoadMachine loads or creates <stateDir>/machine.json.
func LoadMachine(stateDir string) (Machine, error) {
	return Machine{}, errors.New("state.LoadMachine: not implemented")
}

// PinnedVault returns the vault id pinned for an absolute vault path.
func PinnedVault(stateDir, vaultPath string) (string, bool, error) { return "", false, nil }

// PinVault records the vault id for a path.
func PinVault(stateDir, vaultPath, vaultID string) error {
	return errors.New("state.PinVault: not implemented")
}

// BaseEntry is the entry this machine last converged to for a path.
type BaseEntry struct {
	Blob  string      `json:"blob,omitempty"`
	Kind  vault.Kind  `json:"kind"`
	Clock vault.Clock `json:"clock"`
}

// Store is the per-vault base store + trash (<stateDir>/vaults/<vaultID>/).
type Store struct {
	dir string
}

// Open loads (or initialises) the store for a vault.
func Open(stateDir, vaultID string) (*Store, error) {
	return nil, errors.New("state.Open: not implemented")
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) Base(project, path string) (BaseEntry, bool) { return BaseEntry{}, false }
func (s *Store) SetBase(project, path string, b BaseEntry)   {}
func (s *Store) DeleteBase(project, path string)             {}
func (s *Store) Bases(project string) map[string]BaseEntry   { return nil }
func (s *Store) DeleteProject(project string)                {}
func (s *Store) JournalSeq(machine string) uint64            { return 0 }
func (s *Store) SetJournalSeq(machine string, seq uint64)    {}
func (s *Store) Save() error                                 { return errors.New("state.Save: not implemented") }

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

// TrashPut stores content encrypted (deterministic blob format) and indexes it.
func (s *Store) TrashPut(k *crypto.Keys, project, path string, content []byte, mode uint32) (TrashEntry, error) {
	return TrashEntry{}, errors.New("state.TrashPut: not implemented")
}

// TrashList lists entries, newest first.
func (s *Store) TrashList() ([]TrashEntry, error) { return nil, nil }

// TrashRead decrypts one entry.
func (s *Store) TrashRead(k *crypto.Keys, id string) ([]byte, TrashEntry, error) {
	return nil, TrashEntry{}, errors.New("state.TrashRead: not implemented")
}

// TrashPurge removes entries older than the duration; returns the count removed.
func (s *Store) TrashPurge(olderThan time.Duration) (int, error) { return 0, nil }
