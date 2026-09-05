// Package vault implements the on-disk encrypted vault (spec §5):
//
//	vault.json                        plaintext metadata + wrapped vault key
//	machines/<machineID>.json.enc     MachineInfo
//	projects/<id>/meta/<machineID>.json.enc    ProjectMeta
//	projects/<id>/state/<machineID>.json.enc   Journal
//	blobs/<aa>/<blobID>.enc           deterministic content-addressed blobs
//
// A machine only ever writes its own <machineID> files plus blobs.
package vault

import (
	"errors"
	"time"

	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/identity"
)

// Version of the vault format written by this build.
const Version = 1

var (
	ErrWrongPassphrase   = errors.New("wrong passphrase")
	ErrInvalidVault      = errors.New("vault.json is invalid or has been tampered with")
	ErrNewerVersion      = errors.New("vault was created by a newer version of private-sync")
	ErrNoVault           = errors.New("no vault at this path")
	ErrVaultExists       = errors.New("a vault already exists at this path")
	ErrBlobMissing       = errors.New("blob not available")
	ErrNoProject         = errors.New("project not found in vault")
	ErrUnreadableJournal = errors.New("unreadable journal")
)

// Kind of a journal entry.
type Kind int

const (
	KindFile      Kind = iota // regular tracked file
	KindDeleted               // tombstone: delete everywhere
	KindUntracked             // tombstone: stop syncing, keep local copies
)

// Clock is a per-path version vector: machineID -> counter.
type Clock map[string]uint64

// Ordering is the result of Clock.Compare.
type Ordering int

const (
	Equal Ordering = iota
	Before
	After
	Concurrent
)

// Compare returns the ordering of c relative to o.
func (c Clock) Compare(o Clock) Ordering { return Concurrent }

// Merge returns the componentwise max of c and o (new map).
func (c Clock) Merge(o Clock) Clock { return nil }

// Tick returns a copy of c with machine's component incremented.
func (c Clock) Tick(machine string) Clock { return nil }

// Entry is the latest state of a path as seen by one machine.
type Entry struct {
	Path      string    `json:"path"`
	Kind      Kind      `json:"kind"`
	Blob      string    `json:"blob,omitempty"` // "" for tombstones
	Clock     Clock     `json:"clock"`
	Parents   []string  `json:"parents,omitempty"` // non-empty blob ids only
	Mode      uint32    `json:"mode,omitempty"`
	Size      int64     `json:"size,omitempty"`
	ModTime   time.Time `json:"mtime,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
	Machine   string    `json:"machine"`
}

// Journal is one machine's view of a project.
type Journal struct {
	Machine   string           `json:"machine"`
	Seq       uint64           `json:"seq"`
	UpdatedAt time.Time        `json:"updated_at"`
	Entries   map[string]Entry `json:"entries"`
}

// ProjectMeta is one machine's description of a project.
type ProjectMeta struct {
	Name         string                 `json:"name"`
	Fingerprints []identity.Fingerprint `json:"fingerprints"`
	CreatedAt    time.Time              `json:"created_at"`
}

// Project is the merged (across machines) view of a project.
type Project struct {
	ID           string
	Name         string // from the oldest meta
	Fingerprints []identity.Fingerprint
	Machines     []string
	CreatedAt    time.Time
}

// MachineInfo describes a machine that uses the vault.
type MachineInfo struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Hostname string    `json:"hostname"`
	LastSeen time.Time `json:"last_seen"`
}

// Head is the resolved state of a path across machines.
type Head struct {
	Path       string
	Candidates []Entry // survivors of dominance filtering, >= 1
	Entry      *Entry  // non-nil when the candidates agree (single head)
	Base       string  // blob shared by all candidates' Parents; "" if none
}

// Concurrent reports whether the head is unresolved.
func (h Head) Concurrent() bool { return h.Entry == nil }

// ResolveHeads applies the head resolution rules of spec §5 to a set of journals.
func ResolveHeads(journals map[string]*Journal) map[string]Head { return nil }

// Vault is an opened vault.
type Vault struct {
	dir      string
	id       string
	keys     *crypto.Keys
	writerID string
	written  []string
}

// Exists reports whether dir contains a vault.json.
func Exists(dir string) bool { return false }

// Create initialises a new vault: writes vault.json (0600) and nothing else.
// writerID is the machine id (used for temp file suffixes).
func Create(dir string, passphrase []byte, params crypto.KDFParams, writerID string) (*Vault, error) {
	return nil, errors.New("vault.Create: not implemented")
}

// Open validates vault.json, derives the KEK and unwraps the vault key.
func Open(dir string, passphrase []byte, writerID string) (*Vault, error) {
	return nil, errors.New("vault.Open: not implemented")
}

// ReadID returns the vault id from vault.json without a passphrase.
func ReadID(dir string) (string, error) { return "", errors.New("vault.ReadID: not implemented") }

func (v *Vault) ID() string             { return v.id }
func (v *Vault) Dir() string            { return v.dir }
func (v *Vault) Keys() *crypto.Keys     { return v.keys }
func (v *Vault) Close()                 {}
func (v *Vault) Written() []string      { return v.written }
func (v *Vault) BlobID(p []byte) string { return v.keys.BlobID(p) }

// Rekey rewraps the vault key with a new passphrase (vault.json only).
func (v *Vault) Rekey(newPassphrase []byte, params crypto.KDFParams) error {
	return errors.New("vault.Rekey: not implemented")
}

// WriteBlob stores plaintext as a deterministic blob; created is false when it existed.
func (v *Vault) WriteBlob(plaintext []byte) (id string, created bool, err error) {
	return "", false, errors.New("vault.WriteBlob: not implemented")
}

// ReadBlob returns the plaintext of a blob; ErrBlobMissing when absent/unreadable.
func (v *Vault) ReadBlob(id string) ([]byte, error) { return nil, ErrBlobMissing }

// HasBlob reports whether the blob file exists.
func (v *Vault) HasBlob(id string) bool { return false }

// ListProjects returns the merged view of every project (plus warnings for skipped files).
func (v *Vault) ListProjects() ([]Project, []string, error) { return nil, nil, nil }

// ReadProject returns the merged view of one project.
func (v *Vault) ReadProject(id string) (*Project, []string, error) { return nil, nil, ErrNoProject }

// WriteProjectMeta writes this machine's meta file for a project.
func (v *Vault) WriteProjectMeta(projectID, machineID string, m ProjectMeta) error {
	return errors.New("vault.WriteProjectMeta: not implemented")
}

// ReadJournals reads every machine's journal for a project. An undecryptable
// journal returns ErrUnreadableJournal (wrapped, naming the file).
func (v *Vault) ReadJournals(projectID string) (map[string]*Journal, []string, error) {
	return nil, nil, nil
}

// ReadJournal reads one journal; (nil, nil) when absent.
func (v *Vault) ReadJournal(projectID, machineID string) (*Journal, error) { return nil, nil }

// WriteJournal bumps Seq and writes the journal atomically.
func (v *Vault) WriteJournal(projectID string, j *Journal) error {
	return errors.New("vault.WriteJournal: not implemented")
}

// ListMachines lists MachineInfo documents.
func (v *Vault) ListMachines() ([]MachineInfo, []string, error) { return nil, nil, nil }

// WriteMachine writes this machine's info document.
func (v *Vault) WriteMachine(m MachineInfo) error {
	return errors.New("vault.WriteMachine: not implemented")
}

// BlobPath returns the vault-relative path of a blob.
func BlobPath(id string) string { return "" }

// IsMachineFile reports whether a file name matches "<uuid>.json.enc".
func IsMachineFile(name string) (machineID string, ok bool) { return "", false }
