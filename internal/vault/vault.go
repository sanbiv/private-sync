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
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/fsutil"
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

// Additional sentinel errors (not part of the original contract, added as helpers).
var (
	// ErrClosed is returned by every operation on a closed (or nil) vault.
	ErrClosed = errors.New("vault is closed")
	// ErrBadMachineID is returned when a machine id does not have the uuid shape
	// required for <machineID>.json.enc file names (spec §5).
	ErrBadMachineID = errors.New("machine id must be a lowercase uuid (8-4-4-4-12 hex)")
	// ErrBadProjectID is returned when a project id cannot be used as a directory name.
	ErrBadProjectID = errors.New("invalid project id")
	// ErrBadBlobID is returned when a blob id is not a hex string.
	ErrBadBlobID = errors.New("invalid blob id")
	// ErrForeignMachine is returned when a vault handle opened with a writer id
	// tries to write another machine's meta, journal or machine file (spec §5:
	// a machine only ever writes its own <machineID> files).
	ErrForeignMachine = errors.New("a machine may only write its own files")
	// ErrBadDocPath is returned by SealDoc/OpenDoc when the document path is
	// not a non-empty, relative, slash-separated vault path (the AAD that
	// binds a document to its location).
	ErrBadDocPath = errors.New("document path must be a non-empty relative slash-separated vault path")
	// ErrBadEntryPath is returned by WriteJournal (and, wrapped in
	// ErrUnreadableJournal, by the journal readers) when a journal entry's
	// path is not a non-empty, relative, slash-separated project path, or when
	// Entry.Path disagrees with the entries map key. The sync engine writes
	// files at these paths, so "../x", "/abs" or "a\b" must never reach it.
	ErrBadEntryPath = errors.New("journal entry path must be a non-empty relative slash-separated project path")
)

// File and directory names inside the vault.
const (
	VaultFileName = "vault.json"
	BlobsDir      = "blobs"
	MachinesDir   = "machines"
	ProjectsDir   = "projects"
	MetaDir       = "meta"
	StateDir      = "state"
	DocSuffix     = ".json.enc"
	BlobSuffix    = ".enc"

	// VaultKeyAADPrefix prefixes the vault id in the wrapped-key AAD.
	VaultKeyAADPrefix = "vault-key:"
	// vaultIDLen is the length of a vault id: 16 hex characters (crypto.RandomHex(8)).
	vaultIDLen = 16
	// BlobIDLen is the length of a blob id: hex(HMAC-SHA256) is 64 characters.
	// BlobPath, ReadBlob and HasBlob accept nothing else.
	BlobIDLen = 2 * sha256.Size

	// maxVaultFileSize bounds vault.json reads (it is a few hundred bytes).
	maxVaultFileSize = 1 << 20
	// maxDocSize bounds encrypted document reads (journals can grow with the project).
	maxDocSize = 256 << 20
	// maxBlobSize bounds encrypted blob reads. Blobs hold single project files
	// (the scanner already skips anything above the configured max_file_size,
	// 2 MiB by default), so this only guards against a hostile or corrupt
	// remote handing us a multi-gigabyte file to load into memory.
	maxBlobSize = 1 << 30
)

// Kind of a journal entry.
type Kind int

const (
	KindFile      Kind = iota // regular tracked file
	KindDeleted               // tombstone: delete everywhere
	KindUntracked             // tombstone: stop syncing, keep local copies
)

// Valid reports whether k is one of the known kinds. Journals carrying any
// other value are refused (readJournal, WriteJournal): ResolveHeads would
// otherwise treat an unknown kind as a live file that can win a head.
func (k Kind) Valid() bool { return k >= KindFile && k <= KindUntracked }

// String renders the kind for logs and warnings.
func (k Kind) String() string {
	switch k {
	case KindFile:
		return "file"
	case KindDeleted:
		return "deleted"
	case KindUntracked:
		return "untracked"
	default:
		return fmt.Sprintf("Kind(%d)", int(k))
	}
}

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

// String renders the ordering for logs and test output.
func (o Ordering) String() string {
	switch o {
	case Equal:
		return "Equal"
	case Before:
		return "Before"
	case After:
		return "After"
	case Concurrent:
		return "Concurrent"
	default:
		return fmt.Sprintf("Ordering(%d)", int(o))
	}
}

// Compare returns the ordering of c relative to o. Missing components count
// as 0: Before when every component of c is <= o's and at least one is <;
// After symmetrically; Equal when every component matches; Concurrent otherwise.
func (c Clock) Compare(o Clock) Ordering {
	less, greater := false, false
	for k, a := range c {
		b := o[k]
		if a < b {
			less = true
		} else if a > b {
			greater = true
		}
	}
	for k, b := range o {
		if _, seen := c[k]; seen {
			continue
		}
		if b > 0 {
			less = true
		}
	}
	switch {
	case less && greater:
		return Concurrent
	case less:
		return Before
	case greater:
		return After
	default:
		return Equal
	}
}

// Merge returns the componentwise max of c and o (new map).
func (c Clock) Merge(o Clock) Clock {
	out := make(Clock, len(c)+len(o))
	for k, a := range c {
		out[k] = a
	}
	for k, b := range o {
		if b > out[k] {
			out[k] = b
		}
	}
	return out
}

// Tick returns a copy of c with machine's component incremented.
func (c Clock) Tick(machine string) Clock {
	out := make(Clock, len(c)+1)
	for k, a := range c {
		out[k] = a
	}
	out[machine]++
	return out
}

// Copy returns an independent copy of c (nil-safe, never nil).
func (c Clock) Copy() Clock {
	out := make(Clock, len(c))
	for k, a := range c {
		out[k] = a
	}
	return out
}

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
//
// When several candidates survive dominance filtering but still resolve to a
// single head (the same content written independently on two machines,
// deleted/deleted, untracked winning over a concurrent edit), Entry is a copy
// of the winning candidate whose Clock is the merge of every candidate's
// clock. A writer that derives its next clock as Entry.Clock.Tick(self)
// therefore dominates all of them, exactly as the spec's write rule
// (Clock = merge(all candidates' clocks).Tick(self)) requires, and the next
// fetch does not report a spurious concurrent head. Candidates keep their
// original clocks.
//
// Candidates is exactly the set of entries that survived dominance filtering:
// one per machine, in machine-id order, whether or not the head resolved.
// Entries with Equal clocks are never collapsed (the same version recorded by
// two machines yields two candidates), so len(Candidates) is the number of
// machines whose entry is not dominated; use Entry == nil (Concurrent) to
// detect a conflict, never the candidate count.
type Head struct {
	Path       string
	Candidates []Entry // survivors of dominance filtering, one per machine, >= 1
	Entry      *Entry  // non-nil when the candidates agree (single head)
	Base       string  // blob shared by all candidates' Parents; "" if none
}

// Concurrent reports whether the head is unresolved.
func (h Head) Concurrent() bool { return h.Entry == nil }

// candidate pairs an entry with the id of the journal it was read from
// (which may differ from Entry.Machine when a machine copies another's entry).
type candidate struct {
	owner string
	entry Entry
}

// ResolveHeads applies the head resolution rules of spec §5 to a set of journals.
func ResolveHeads(journals map[string]*Journal) map[string]Head {
	owners := make([]string, 0, len(journals))
	for id, j := range journals {
		if j == nil {
			continue
		}
		owners = append(owners, id)
	}
	sort.Strings(owners)

	paths := map[string]struct{}{}
	for _, id := range owners {
		for p := range journals[id].Entries {
			paths[p] = struct{}{}
		}
	}

	heads := make(map[string]Head, len(paths))
	for p := range paths {
		var cands []candidate
		for _, id := range owners {
			e, ok := journals[id].Entries[p]
			if !ok {
				continue
			}
			// The map key is the path (readJournal guarantees the two agree
			// for journals read from disk); Head.Path and Entry.Path never differ.
			e.Path = p
			if e.Machine == "" {
				e.Machine = id
			}
			cands = append(cands, candidate{owner: id, entry: e})
		}
		heads[p] = resolveHead(p, cands)
	}
	return heads
}

// resolveHead resolves one path from its per-machine candidates (already in
// machine-id order, len >= 1).
func resolveHead(p string, cands []candidate) Head {
	// Dominance: drop every candidate whose clock is Before another's.
	var survivors []candidate
	for i, c := range cands {
		dominated := false
		for j, o := range cands {
			if i == j {
				continue
			}
			if c.entry.Clock.Compare(o.entry.Clock) == Before {
				dominated = true
				break
			}
		}
		if !dominated {
			survivors = append(survivors, c)
		}
	}

	// Equal clocks are not collapsed (see Head): the same version recorded by
	// several machines stays one candidate per machine. The state comparison
	// below resolves them to a single head when they agree, and Equal clocks
	// with different state are concurrent like any other survivors.
	h := Head{Path: p, Candidates: make([]Entry, 0, len(survivors))}
	for _, c := range survivors {
		// Detached like Entry: a caller that writes into a candidate's Clock
		// or Parents must not silently edit the journal it was read from.
		h.Candidates = append(h.Candidates, detached(c.entry))
	}

	allSame := true
	allDeleted := true
	untracked := -1
	for i, c := range survivors {
		if !sameState(c.entry, survivors[0].entry) {
			allSame = false
		}
		if c.entry.Kind != KindDeleted {
			allDeleted = false
		}
		if c.entry.Kind == KindUntracked && (untracked < 0 || c.entry.UpdatedAt.After(survivors[untracked].entry.UpdatedAt)) {
			untracked = i
		}
	}

	switch {
	case allSame:
		h.Entry = singleHead(latest(survivors), survivors)
	case untracked >= 0:
		h.Entry = singleHead(survivors[untracked].entry, survivors)
	case allDeleted:
		h.Entry = singleHead(latest(survivors), survivors)
	default:
		h.Base = commonParent(survivors)
	}
	return h
}

// singleHead returns winner as the head entry with its Clock replaced by the
// merge of every survivor's clock (see Head), so that ticking it dominates all
// of them. The returned entry is independent of the candidates.
func singleHead(winner Entry, survivors []candidate) *Entry {
	if len(survivors) > 1 {
		merged := Clock{}
		for _, c := range survivors {
			merged = merged.Merge(c.entry.Clock)
		}
		winner.Clock = merged
	} else if winner.Clock != nil {
		winner.Clock = winner.Clock.Copy() // a lone candidate keeps its exact clock, detached
	}
	winner.Parents = append([]string(nil), winner.Parents...)
	return &winner
}

// detached returns e with its reference fields (Clock, Parents) copied, so
// the result shares nothing with the journal it came from. nil stays nil.
func detached(e Entry) Entry {
	if e.Clock != nil {
		e.Clock = e.Clock.Copy()
	}
	if e.Parents != nil {
		e.Parents = append(make([]string, 0, len(e.Parents)), e.Parents...)
	}
	return e
}

// sameState reports whether two entries carry the same (Kind, Blob) tuple.
func sameState(a, b Entry) bool { return a.Kind == b.Kind && a.Blob == b.Blob }

// latest returns the entry with the newest UpdatedAt (first on ties).
func latest(cands []candidate) Entry {
	best := cands[0].entry
	for _, c := range cands[1:] {
		if c.entry.UpdatedAt.After(best.UpdatedAt) {
			best = c.entry
		}
	}
	return best
}

// commonParent returns the first blob (in the first candidate's Parents order)
// present in every candidate's Parents; "" when any candidate has no parents
// or nothing is shared.
func commonParent(cands []candidate) string {
	if len(cands) == 0 {
		return ""
	}
	for _, c := range cands {
		if len(nonEmpty(c.entry.Parents)) == 0 {
			return ""
		}
	}
	for _, blob := range nonEmpty(cands[0].entry.Parents) {
		shared := true
		for _, c := range cands[1:] {
			if !contains(c.entry.Parents, blob) {
				shared = false
				break
			}
		}
		if shared {
			return blob
		}
	}
	return ""
}

func nonEmpty(ids []string) []string {
	out := ids[:0:0]
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

func contains(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// vaultFile is the plaintext vault.json document.
type vaultFile struct {
	Version    int              `json:"version"`
	ID         string           `json:"id"`
	KDF        crypto.KDFParams `json:"kdf"`
	WrappedKey []byte           `json:"wrapped_key"` // base64 in JSON
	CreatedAt  time.Time        `json:"created_at"`
}

// Vault is an opened vault.
type Vault struct {
	dir      string
	id       string
	keys     *crypto.Keys
	writerID string
	written  []string

	// Fields added by the implementation (appended, unexported).
	mu         sync.Mutex // guards written/writtenSet, vk, meta, closed
	writtenSet map[string]struct{}
	vk         []byte    // vault key, kept for Rekey; zeroed on Close
	meta       vaultFile // vault.json as read/written (KDF params, created_at)
	closed     bool
	// blobLocks serialises WriteBlob per blob directory (first id byte), so
	// two in-process writers of the same blob never share the same temp file
	// (fsutil names it <target>.psv-tmp-<writer suffix>, which is per handle,
	// not per call).
	blobLocks [256]sync.Mutex
	// docMu serialises document writes (journals, meta, machine files) for
	// the same reason: two WriteJournal calls for one project through one
	// handle would otherwise open the same <target>.psv-tmp-<suffix> with
	// O_TRUNC, interleave their bytes (an undecryptable journal makes the
	// whole project unreadable) or lose the rename race. Documents are small
	// and written sequentially by the sync engine, so one lock is enough.
	docMu sync.Mutex
}

// Exists reports whether dir contains a vault.json. An empty dir is never a
// vault (it would otherwise resolve to the process working directory).
func Exists(dir string) bool {
	if dir == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, VaultFileName))
	return err == nil && st.Mode().IsRegular()
}

// Create initialises a new vault: writes vault.json (0600) and nothing else.
// writerID is the machine id (used for temp file suffixes and to restrict
// writes to this machine's own files); see checkWriterID for its rules. It is
// an addition to the Create(dir, passphrase, params) signature listed in the
// spec's §5 API sketch: "" is the unrestricted tooling mode (temp suffix "w").
//
// A zero params selects crypto.DefaultKDFParams; params without a salt get a
// fresh random one. The passphrase buffer is zeroed before Create returns.
func Create(dir string, passphrase []byte, params crypto.KDFParams, writerID string) (*Vault, error) {
	defer crypto.Zero(passphrase)
	if dir == "" {
		return nil, errors.New("vault.Create: empty directory")
	}
	if err := checkWriterID(writerID); err != nil {
		return nil, fmt.Errorf("vault.Create: %w", err)
	}
	if Exists(dir) {
		return nil, fmt.Errorf("vault.Create: %w: %s", ErrVaultExists, dir)
	}
	var err error
	if params, err = completeKDFParams(params); err != nil {
		return nil, fmt.Errorf("vault.Create: %w", err)
	}
	if err := fsutil.EnsureDir(dir); err != nil {
		return nil, fmt.Errorf("vault.Create: %w", err)
	}
	id, err := crypto.RandomHex(8)
	if err != nil {
		return nil, fmt.Errorf("vault.Create: %w", err)
	}
	vk, err := crypto.NewVaultKey()
	if err != nil {
		return nil, fmt.Errorf("vault.Create: %w", err)
	}
	kek, err := crypto.DeriveKEK(passphrase, params)
	if err != nil {
		crypto.Zero(vk)
		return nil, fmt.Errorf("vault.Create: %w", err)
	}
	wrapped, err := crypto.WrapKey(kek, vk, VaultKeyAADPrefix+id)
	crypto.Zero(kek)
	if err != nil {
		crypto.Zero(vk)
		return nil, fmt.Errorf("vault.Create: %w", err)
	}
	keys, err := crypto.NewKeys(vk)
	if err != nil {
		crypto.Zero(vk)
		return nil, fmt.Errorf("vault.Create: %w", err)
	}
	v := &Vault{
		dir:        dir,
		id:         id,
		keys:       keys,
		writerID:   writerID,
		writtenSet: map[string]struct{}{},
		vk:         vk,
		meta: vaultFile{
			Version:    Version,
			ID:         id,
			KDF:        params,
			WrappedKey: wrapped,
			CreatedAt:  time.Now().UTC().Truncate(time.Second),
		},
	}
	if err := v.writeVaultFile(); err != nil {
		v.Close()
		return nil, fmt.Errorf("vault.Create: %w", err)
	}
	return v, nil
}

// checkWriterID validates the writer id given to Create/Open.
//
// A writer id is the machine id the handle writes as: it names the temp files
// of every atomic write (first 8 characters) and restricts WriteJournal,
// WriteProjectMeta and WriteMachine to that machine's own files, so it must
// have the uuid shape machine files use. The empty string is the
// unrestricted mode for tooling (inspection, tests, migrations): temp files
// get a generic suffix and any machine's files may be written.
func checkWriterID(writerID string) error {
	if writerID != "" && !ValidMachineID(writerID) {
		return fmt.Errorf("writer id: %w: %q", ErrBadMachineID, writerID)
	}
	return nil
}

// completeKDFParams fills defaults (zero params) or a missing salt, then validates.
func completeKDFParams(p crypto.KDFParams) (crypto.KDFParams, error) {
	if p.Algo == "" && len(p.Salt) == 0 && p.Time == 0 && p.Memory == 0 && p.Threads == 0 {
		return crypto.DefaultKDFParams()
	}
	if len(p.Salt) == 0 {
		fresh, err := crypto.DefaultKDFParams()
		if err != nil {
			return p, err
		}
		p.Salt = fresh.Salt
	}
	if p.Algo == "" {
		p.Algo = crypto.KDFAlgo
	}
	if err := p.Validate(); err != nil {
		return p, err
	}
	return p, nil
}

// Open validates vault.json, derives the KEK and unwraps the vault key.
// writerID follows the rules of checkWriterID ("" = unrestricted tooling mode).
//
// vault.json is fully validated (version, id, KDF bounds, wrapped key shape)
// before the KDF runs. The passphrase buffer is zeroed before Open returns,
// even on ErrWrongPassphrase; pass a copy when you intend to retry.
func Open(dir string, passphrase []byte, writerID string) (*Vault, error) {
	defer crypto.Zero(passphrase)
	if err := checkWriterID(writerID); err != nil {
		return nil, fmt.Errorf("vault.Open: %w", err)
	}
	vf, err := readVaultFile(dir)
	if err != nil {
		return nil, fmt.Errorf("vault.Open: %w", err)
	}
	if err := validateVaultFile(vf); err != nil {
		return nil, fmt.Errorf("vault.Open: %w", err)
	}
	kek, err := crypto.DeriveKEK(passphrase, vf.KDF)
	if err != nil {
		return nil, fmt.Errorf("vault.Open: %w: %w", ErrInvalidVault, err)
	}
	vk, err := crypto.UnwrapKey(kek, vf.WrappedKey, VaultKeyAADPrefix+vf.ID)
	crypto.Zero(kek)
	if err != nil {
		if errors.Is(err, crypto.ErrAuth) {
			// Deliberately not wrapped: a failed unwrap is the passphrase check
			// (spec §4), and callers must not mistake it for a corrupt file.
			return nil, fmt.Errorf("vault.Open: %w", ErrWrongPassphrase)
		}
		return nil, fmt.Errorf("vault.Open: %w: %w", ErrInvalidVault, err)
	}
	keys, err := crypto.NewKeys(vk)
	if err != nil {
		crypto.Zero(vk)
		return nil, fmt.Errorf("vault.Open: %w", err)
	}
	return &Vault{
		dir:        dir,
		id:         vf.ID,
		keys:       keys,
		writerID:   writerID,
		writtenSet: map[string]struct{}{},
		vk:         vk,
		meta:       vf,
	}, nil
}

// ReadID returns the vault id from vault.json without a passphrase.
func ReadID(dir string) (string, error) {
	vf, err := readVaultFile(dir)
	if err != nil {
		return "", fmt.Errorf("vault.ReadID: %w", err)
	}
	if vf.ID == "" {
		return "", fmt.Errorf("vault.ReadID: %w: empty id", ErrInvalidVault)
	}
	return vf.ID, nil
}

// readVaultFile reads and decodes vault.json: ErrNoVault when absent,
// ErrInvalidVault when it cannot be decoded.
func readVaultFile(dir string) (vaultFile, error) {
	var vf vaultFile
	if dir == "" {
		return vf, ErrNoVault
	}
	data, err := fsutil.ReadFileMax(filepath.Join(dir, VaultFileName), maxVaultFileSize)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return vf, fmt.Errorf("%w: %s", ErrNoVault, dir)
		}
		if errors.Is(err, fsutil.ErrTooLarge) {
			return vf, fmt.Errorf("%w: %w", ErrInvalidVault, err)
		}
		return vf, fmt.Errorf("read %s: %w", VaultFileName, err)
	}
	// The version is examined before the rest of the document is decoded: a
	// newer format may change the shape of kdf or wrapped_key, and §13 wants
	// that refused as "newer version", not reported as tampering.
	var probe struct {
		Version int `json:"version"`
	}
	if err := json.NewDecoder(bytes.NewReader(data)).Decode(&probe); err != nil {
		return vf, fmt.Errorf("%w: %w", ErrInvalidVault, err)
	}
	if probe.Version > Version {
		return vf, fmt.Errorf("%w: version %d (this build supports %d)", ErrNewerVersion, probe.Version, Version)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(&vf); err != nil {
		return vf, fmt.Errorf("%w: %w", ErrInvalidVault, err)
	}
	// Decode stops after the first JSON value; anything but whitespace after
	// it means the file is not the document we wrote.
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return vaultFile{}, fmt.Errorf("%w: trailing data after the JSON document", ErrInvalidVault)
	}
	return vf, nil
}

// validateVaultFile enforces every check that must happen before the KDF runs.
func validateVaultFile(vf vaultFile) error {
	if vf.Version > Version {
		return fmt.Errorf("%w: version %d (this build supports %d)", ErrNewerVersion, vf.Version, Version)
	}
	if vf.Version < 1 {
		return fmt.Errorf("%w: version %d", ErrInvalidVault, vf.Version)
	}
	if len(vf.ID) != vaultIDLen || !isHex(vf.ID) {
		return fmt.Errorf("%w: bad vault id", ErrInvalidVault)
	}
	if err := vf.KDF.Validate(); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidVault, err)
	}
	if want := crypto.MinCiphertextSize + crypto.KeySize; len(vf.WrappedKey) != want {
		return fmt.Errorf("%w: wrapped_key is %d bytes (want %d)", ErrInvalidVault, len(vf.WrappedKey), want)
	}
	if string(vf.WrappedKey[:len(crypto.Magic)]) != crypto.Magic {
		return fmt.Errorf("%w: wrapped_key has no %s header", ErrInvalidVault, crypto.Magic)
	}
	return nil
}

// writeVaultFile serialises v.meta to vault.json atomically (0600).
func (v *Vault) writeVaultFile() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.writeVaultFileLocked()
}

// writeVaultFileLocked is writeVaultFile for callers already holding v.mu.
func (v *Vault) writeVaultFileLocked() error {
	data, err := json.MarshalIndent(v.meta, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := fsutil.WriteFileAtomic(filepath.Join(v.dir, VaultFileName), data, 0o600, v.tempSuffix()); err != nil {
		return err
	}
	v.recordLocked(VaultFileName)
	return nil
}

// tempSuffix is the first 8 characters of the writer id (or all of it).
func (v *Vault) tempSuffix() string {
	if len(v.writerID) > 8 {
		return v.writerID[:8]
	}
	return v.writerID
}

// ownTempMarker is the tail every temp file written through this handle
// carries: fsutil.TempPrefix + the writer suffix (fsutil substitutes "w" for
// an empty suffix).
func (v *Vault) ownTempMarker() string {
	s := v.tempSuffix()
	if s == "" {
		s = "w"
	}
	return fsutil.TempPrefix + s
}

func (v *Vault) ID() string         { return v.id }
func (v *Vault) Dir() string        { return v.dir }
func (v *Vault) Keys() *crypto.Keys { return v.keys }

// WriterID returns the machine id this vault handle writes as.
func (v *Vault) WriterID() string { return v.writerID }

// KDFParams returns the KDF parameters currently stored in vault.json.
func (v *Vault) KDFParams() crypto.KDFParams {
	if v == nil {
		return crypto.KDFParams{}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	p := v.meta.KDF
	p.Salt = append([]byte(nil), p.Salt...)
	return p
}

// CreatedAt returns the vault creation time recorded in vault.json.
func (v *Vault) CreatedAt() time.Time {
	if v == nil {
		return time.Time{}
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.meta.CreatedAt
}

// Close zeroes the key material; every later operation fails with ErrClosed.
// Safe on nil and idempotent.
func (v *Vault) Close() {
	if v == nil {
		return
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	v.keys.Zero()
	crypto.Zero(v.vk)
	v.vk = nil
	crypto.Zero(v.meta.WrappedKey)
	v.closed = true
}

// Written returns the vault-relative (slash separated) paths written since
// Open/Create, in first-write order without duplicates.
func (v *Vault) Written() []string {
	if v == nil {
		return nil
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.written...)
}

// record adds rel to the written list (deduped, insertion order).
func (v *Vault) record(rel string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.recordLocked(rel)
}

// recordLocked is record for callers already holding v.mu.
func (v *Vault) recordLocked(rel string) {
	if v.writtenSet == nil {
		v.writtenSet = map[string]struct{}{}
	}
	if _, dup := v.writtenSet[rel]; dup {
		return
	}
	v.writtenSet[rel] = struct{}{}
	v.written = append(v.written, rel)
}

// ready returns ErrClosed when the vault is nil or closed.
func (v *Vault) ready() error {
	if v == nil {
		return ErrClosed
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.keys == nil {
		return ErrClosed
	}
	return nil
}

// closedOr maps an error from the key set to ErrClosed (wrapping err) when
// the vault has been closed since ready() last passed: ready() is not held
// across SealBlob/OpenDoc/..., so a Close racing an operation makes crypto
// report its own not-ready error, which is semantically ErrClosed to callers.
// Any other error, and nil, is returned unchanged.
func (v *Vault) closedOr(err error) error {
	if err == nil || v.ready() == nil {
		return err
	}
	return fmt.Errorf("%w: %w", ErrClosed, err)
}

// BlobID returns the content-addressed id of plaintext ("" on a nil or closed vault).
func (v *Vault) BlobID(p []byte) string {
	if v == nil {
		return ""
	}
	return v.keys.BlobID(p)
}

// Rekey rewraps the vault key with a new passphrase (vault.json only).
// The KDF costs come from params; the salt is always freshly generated.
// The passphrase buffer is zeroed before Rekey returns.
//
// The vault key is copied under v.mu before the (slow) KDF runs and the
// result is installed under v.mu again, so a concurrent Close can never make
// Rekey wrap a half-zeroed key: Close either happens before (Rekey fails with
// ErrClosed) or after (the new vault.json is complete).
func (v *Vault) Rekey(newPassphrase []byte, params crypto.KDFParams) error {
	defer crypto.Zero(newPassphrase)
	if v == nil {
		return fmt.Errorf("vault.Rekey: %w", ErrClosed)
	}

	// Phase 1: snapshot the key material and current params.
	v.mu.Lock()
	if v.closed || v.keys == nil || len(v.vk) != crypto.KeySize {
		v.mu.Unlock()
		return fmt.Errorf("vault.Rekey: %w", ErrClosed)
	}
	vk := append([]byte(nil), v.vk...)
	current := v.meta.KDF
	v.mu.Unlock()
	defer crypto.Zero(vk)

	if params.Algo == "" && params.Time == 0 && params.Memory == 0 && params.Threads == 0 {
		params = current
	}
	params.Salt = nil
	params, err := completeKDFParams(params)
	if err != nil {
		return fmt.Errorf("vault.Rekey: %w", err)
	}
	kek, err := crypto.DeriveKEK(newPassphrase, params)
	if err != nil {
		return fmt.Errorf("vault.Rekey: %w", err)
	}
	wrapped, err := crypto.WrapKey(kek, vk, VaultKeyAADPrefix+v.id)
	crypto.Zero(kek)
	if err != nil {
		return fmt.Errorf("vault.Rekey: %w", err)
	}

	// Phase 2: install and persist atomically with respect to Close and
	// other Rekeys; meta and vault.json always describe the same wrap.
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed {
		return fmt.Errorf("vault.Rekey: %w", ErrClosed)
	}
	old := v.meta
	v.meta.KDF = params
	v.meta.WrappedKey = wrapped
	if err := v.writeVaultFileLocked(); err != nil {
		v.meta = old
		return fmt.Errorf("vault.Rekey: %w", err)
	}
	crypto.Zero(old.WrappedKey)
	return nil
}

// ---- blobs -----------------------------------------------------------------

// BlobPath returns the vault-relative path of a blob ("" for an invalid id).
func BlobPath(id string) string {
	if !validBlobID(id) {
		return ""
	}
	return path.Join(BlobsDir, id[:2], id+BlobSuffix)
}

// validBlobID accepts exactly what SealBlob produces: BlobIDLen (64)
// lowercase hex characters. Anything shorter would probe or create paths
// like blobs/ab/ab.enc that no blob can ever have.
func validBlobID(id string) bool { return len(id) == BlobIDLen && isHex(id) }

func isHex(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// blobFile returns the absolute path of a blob.
func (v *Vault) blobFile(id string) string {
	return filepath.Join(v.dir, filepath.FromSlash(BlobPath(id)))
}

// WriteBlob stores plaintext as a deterministic blob; created is false when it existed.
// Safe for concurrent use: writers of the same blob are serialised, so the
// first one creates the file and the rest observe identical bytes.
func (v *Vault) WriteBlob(plaintext []byte) (id string, created bool, err error) {
	if err := v.ready(); err != nil {
		return "", false, fmt.Errorf("vault.WriteBlob: %w", err)
	}
	id, ct, err := v.keys.SealBlob(plaintext)
	if err != nil {
		return "", false, fmt.Errorf("vault.WriteBlob: %w", v.closedOr(err))
	}
	if !validBlobID(id) { // cannot happen with a ready key set; keeps blobLock in range
		return "", false, fmt.Errorf("vault.WriteBlob: %w: %q", ErrBadBlobID, id)
	}
	rel := BlobPath(id)
	file := v.blobFile(id)

	lock := v.blobLock(id)
	lock.Lock()
	defer lock.Unlock()
	if v.sameBlobOnDisk(file, ct) {
		return id, false, nil
	}
	if err := fsutil.WriteFileAtomic(file, ct, 0o600, v.tempSuffix()); err != nil {
		return "", false, fmt.Errorf("vault.WriteBlob: %s: %w", rel, err)
	}
	v.record(rel)
	return id, true, nil
}

// blobReadFile reads an existing blob for the WriteBlob dedupe comparison
// (a variable so tests can count the reads).
var blobReadFile = fsutil.ReadFileMax

// sameBlobOnDisk reports whether file already holds exactly ct. The size is
// checked with a stat first: a file of a different size (absent, truncated,
// a torn write) is never read into memory just to find out it differs.
func (v *Vault) sameBlobOnDisk(file string, ct []byte) bool {
	st, err := os.Stat(file)
	if err != nil || !st.Mode().IsRegular() || st.Size() != int64(len(ct)) {
		return false
	}
	existing, err := blobReadFile(file, maxBlobSize)
	return err == nil && bytes.Equal(existing, ct)
}

// blobLock returns the mutex serialising writes of blobs whose id starts
// with the same byte (the blob directory). id must be valid hex.
func (v *Vault) blobLock(id string) *sync.Mutex {
	return &v.blobLocks[hexByte(id[0])<<4|hexByte(id[1])]
}

// hexByte maps a lowercase hex digit to its value (0 for anything else).
func hexByte(c byte) int {
	switch {
	case c >= '0' && c <= '9':
		return int(c - '0')
	case c >= 'a' && c <= 'f':
		return int(c-'a') + 10
	default:
		return 0
	}
}

// ReadBlob returns the plaintext of a blob; ErrBlobMissing when absent/unreadable.
func (v *Vault) ReadBlob(id string) ([]byte, error) {
	if err := v.ready(); err != nil {
		return nil, fmt.Errorf("vault.ReadBlob: %w", err)
	}
	if !validBlobID(id) {
		return nil, fmt.Errorf("vault.ReadBlob: %w: %w %q", ErrBlobMissing, ErrBadBlobID, id)
	}
	// Every failure is ErrBlobMissing to the caller; the cause (fs.ErrNotExist,
	// fsutil.ErrTooLarge, crypto.ErrAuth/ErrFormat) stays in the chain for
	// diagnostics.
	rel := BlobPath(id)
	ct, err := fsutil.ReadFileMax(v.blobFile(id), maxBlobSize)
	if err != nil {
		return nil, fmt.Errorf("vault.ReadBlob: %w: %s: %w", ErrBlobMissing, rel, err)
	}
	pt, err := v.keys.OpenBlob(id, ct)
	if err != nil {
		// A vault closed while the read was in flight is not a missing blob.
		if cerr := v.closedOr(err); errors.Is(cerr, ErrClosed) {
			return nil, fmt.Errorf("vault.ReadBlob: %w", cerr)
		}
		return nil, fmt.Errorf("vault.ReadBlob: %w: %s: %w", ErrBlobMissing, rel, err)
	}
	return pt, nil
}

// HasBlob reports whether the blob file exists.
func (v *Vault) HasBlob(id string) bool {
	if v == nil || !validBlobID(id) {
		return false
	}
	st, err := os.Stat(v.blobFile(id))
	return err == nil && st.Mode().IsRegular()
}

// ---- documents -------------------------------------------------------------

// SealDoc encrypts a document bound to its vault-relative slash path
// (ErrBadDocPath when rel is not one; see checkDocPath).
func (v *Vault) SealDoc(rel string, plaintext []byte) ([]byte, error) {
	if err := v.ready(); err != nil {
		return nil, fmt.Errorf("vault.SealDoc: %w", err)
	}
	if err := checkDocPath(rel); err != nil {
		return nil, fmt.Errorf("vault.SealDoc: %w", err)
	}
	ct, err := v.keys.SealDoc(rel, plaintext)
	if err != nil {
		return nil, fmt.Errorf("vault.SealDoc: %w", v.closedOr(err))
	}
	return ct, nil
}

// OpenDoc decrypts a document sealed with SealDoc under the same path.
func (v *Vault) OpenDoc(rel string, ciphertext []byte) ([]byte, error) {
	if err := v.ready(); err != nil {
		return nil, fmt.Errorf("vault.OpenDoc: %w", err)
	}
	if err := checkDocPath(rel); err != nil {
		return nil, fmt.Errorf("vault.OpenDoc: %w", err)
	}
	pt, err := v.keys.OpenDoc(rel, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("vault.OpenDoc: %w", v.closedOr(err))
	}
	return pt, nil
}

// checkDocPath enforces the canonical document AAD form of spec §4/§5: a
// non-empty, relative, slash-separated vault path (no backslash, no leading
// slash, no "." / ".." elements). An empty path would silently drop the
// move-protection binding, and a platform-specific separator would make a
// document sealed on Windows unopenable elsewhere.
func checkDocPath(rel string) error { return checkRelPath(rel, ErrBadDocPath) }

// checkEntryPath applies the same rules to a journal entry path (spec §5:
// slash separated, relative to the project root), reported as ErrBadEntryPath.
func checkEntryPath(p string) error { return checkRelPath(p, ErrBadEntryPath) }

// checkRelPath is the shared rule set of checkDocPath and checkEntryPath;
// bad wraps the violation.
func checkRelPath(rel string, bad error) error {
	switch {
	case rel == "":
		return fmt.Errorf("%w: empty", bad)
	case strings.ContainsRune(rel, '\\'):
		return fmt.Errorf("%w: %q contains a backslash", bad, rel)
	case strings.HasPrefix(rel, "/"):
		return fmt.Errorf("%w: %q is absolute", bad, rel)
	case strings.ContainsRune(rel, 0):
		return fmt.Errorf("%w: %q contains a NUL byte", bad, rel)
	}
	for _, elem := range strings.Split(rel, "/") {
		if elem == "" || elem == "." || elem == ".." {
			return fmt.Errorf("%w: %q has an empty, \".\" or \"..\" element", bad, rel)
		}
	}
	return nil
}

// checkEntry validates one journal entry against its map key: a known kind,
// a well-formed path, and a Path field that (when set) matches the key.
// Shared by readJournal and WriteJournal so a machine never writes what
// every other machine would refuse.
func checkEntry(p string, e Entry) error {
	if !e.Kind.Valid() {
		return fmt.Errorf("entry %q has unknown kind %d", p, e.Kind)
	}
	if err := checkEntryPath(p); err != nil {
		return fmt.Errorf("entry %q: %w", p, err)
	}
	if e.Path != "" && e.Path != p {
		return fmt.Errorf("entry %q: %w: path field is %q", p, ErrBadEntryPath, e.Path)
	}
	return nil
}

// writeDoc JSON-encodes value, seals it under rel and writes it atomically (0600).
func (v *Vault) writeDoc(rel string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	ct, err := v.keys.SealDoc(rel, data)
	if err != nil {
		return v.closedOr(err)
	}
	abs := filepath.Join(v.dir, filepath.FromSlash(rel))
	v.docMu.Lock()
	err = fsutil.WriteFileAtomic(abs, ct, 0o600, v.tempSuffix())
	v.docMu.Unlock()
	if err != nil {
		return err
	}
	v.record(rel)
	return nil
}

// readDoc reads, decrypts and JSON-decodes the document at rel into value.
// The returned error wraps fs.ErrNotExist when the file is absent.
func (v *Vault) readDoc(rel string, value any) error {
	abs := filepath.Join(v.dir, filepath.FromSlash(rel))
	ct, err := fsutil.ReadFileMax(abs, maxDocSize)
	if err != nil {
		return err
	}
	pt, err := v.keys.OpenDoc(rel, ct)
	if err != nil {
		return v.closedOr(err)
	}
	if err := json.Unmarshal(pt, value); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	return nil
}

// ---- machine files ---------------------------------------------------------

var machineFileRe = regexp.MustCompile(`^([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})\.json\.enc$`)

// IsMachineFile reports whether a file name matches "<uuid>.json.enc".
func IsMachineFile(name string) (machineID string, ok bool) {
	m := machineFileRe.FindStringSubmatch(name)
	if m == nil {
		return "", false
	}
	return m[1], true
}

// ValidMachineID reports whether id has the uuid shape required for file names.
func ValidMachineID(id string) bool {
	_, ok := IsMachineFile(id + DocSuffix)
	return ok
}

// validProjectID accepts ids usable as a single directory name.
func validProjectID(id string) bool {
	if id == "" || id == "." || id == ".." {
		return false
	}
	return !strings.ContainsAny(id, `/\`+"\x00")
}

// listMachineFiles lists the machine ids of <relDir>/*.json.enc, warning about
// every other name (spec §5) except this handle's own in-flight temp files,
// which either become real files or are removed at Apply start. Another
// machine's temp file is reported like any other stray name: nobody cleans it
// up and the user should know it is there. A missing directory yields no ids
// and no error.
func (v *Vault) listMachineFiles(relDir string) (ids []string, warnings []string, err error) {
	entries, err := os.ReadDir(filepath.Join(v.dir, filepath.FromSlash(relDir)))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("list %s: %w", relDir, err)
	}
	for _, e := range entries {
		name := e.Name()
		if v.isOwnTemp(name) {
			continue
		}
		id, ok := IsMachineFile(name)
		if !ok || e.IsDir() {
			warnings = append(warnings, skippedWarning(relDir, name))
			continue
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, warnings, nil
}

// isOwnTemp reports whether name is a temp file of this handle's writer
// (fsutil.WriteFileAtomic naming: "<target>.psv-tmp-<suffix>").
func (v *Vault) isOwnTemp(name string) bool {
	return strings.HasSuffix(name, v.ownTempMarker())
}

// skippedWarning is the standard "skipped with a warning naming the file"
// message of spec §5, with a hint for stale temp files left by others.
func skippedWarning(relDir, name string) string {
	if fsutil.IsTemp(name) {
		return fmt.Sprintf("%s: skipped unexpected file %q (temp file of another writer; safe to delete if stale)", relDir, name)
	}
	return fmt.Sprintf("%s: skipped unexpected file %q", relDir, name)
}

// ---- projects --------------------------------------------------------------

func projectDir(projectID string) string { return path.Join(ProjectsDir, projectID) }

func metaPath(projectID, machineID string) string {
	return path.Join(ProjectsDir, projectID, MetaDir, machineID+DocSuffix)
}

func statePath(projectID, machineID string) string {
	return path.Join(ProjectsDir, projectID, StateDir, machineID+DocSuffix)
}

func machinePath(machineID string) string { return path.Join(MachinesDir, machineID+DocSuffix) }

// ListProjects returns the merged view of every project (plus warnings for skipped files).
func (v *Vault) ListProjects() ([]Project, []string, error) {
	if err := v.ready(); err != nil {
		return nil, nil, fmt.Errorf("vault.ListProjects: %w", err)
	}
	entries, err := os.ReadDir(filepath.Join(v.dir, ProjectsDir))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("vault.ListProjects: %w", err)
	}
	var (
		projects []Project
		warnings []string
	)
	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() {
			if !v.isOwnTemp(name) {
				warnings = append(warnings, skippedWarning(ProjectsDir, name))
			}
			continue
		}
		p, w, err := v.readProject(name)
		warnings = append(warnings, w...)
		if err != nil {
			if errors.Is(err, ErrNoProject) {
				warnings = append(warnings, fmt.Sprintf("%s: no readable project meta, skipped", projectDir(name)))
				continue
			}
			return nil, warnings, fmt.Errorf("vault.ListProjects: %w", err)
		}
		projects = append(projects, *p)
	}
	sort.Slice(projects, func(i, j int) bool { return projects[i].ID < projects[j].ID })
	return projects, warnings, nil
}

// ReadProject returns the merged view of one project.
func (v *Vault) ReadProject(id string) (*Project, []string, error) {
	if err := v.ready(); err != nil {
		return nil, nil, fmt.Errorf("vault.ReadProject: %w", err)
	}
	if !validProjectID(id) {
		return nil, nil, fmt.Errorf("vault.ReadProject: %w: %w %q", ErrNoProject, ErrBadProjectID, id)
	}
	p, w, err := v.readProject(id)
	if err != nil {
		return nil, w, fmt.Errorf("vault.ReadProject: %w", err)
	}
	return p, w, nil
}

// readProject reads and merges every machine's meta of a project. ErrNoProject
// when the directory is absent or no meta can be read.
func (v *Vault) readProject(id string) (*Project, []string, error) {
	dir := projectDir(id)
	st, err := os.Stat(filepath.Join(v.dir, filepath.FromSlash(dir)))
	if err != nil || !st.IsDir() {
		return nil, nil, fmt.Errorf("%w: %s", ErrNoProject, id)
	}
	metaIDs, warnings, err := v.listMachineFiles(path.Join(dir, MetaDir))
	if err != nil {
		return nil, warnings, err
	}
	stateIDs, w, err := v.listMachineFiles(path.Join(dir, StateDir))
	warnings = append(warnings, w...)
	if err != nil {
		return nil, warnings, err
	}

	type owned struct {
		machine string
		meta    ProjectMeta
	}
	var metas []owned
	for _, mid := range metaIDs {
		rel := metaPath(id, mid)
		var m ProjectMeta
		if err := v.readDoc(rel, &m); err != nil {
			if errors.Is(err, ErrClosed) {
				return nil, warnings, err
			}
			warnings = append(warnings, fmt.Sprintf("%s: unreadable project meta, skipped: %v", rel, err))
			continue
		}
		metas = append(metas, owned{machine: mid, meta: m})
	}
	if len(metas) == 0 {
		return nil, warnings, fmt.Errorf("%w: %s has no readable meta", ErrNoProject, id)
	}

	// metas are in machine-id order (listMachineFiles sorts), so the first
	// oldest wins ties by lowest machine id.
	oldest := 0
	for i := range metas {
		if olderThan(metas[i].meta.CreatedAt, metas[oldest].meta.CreatedAt) {
			oldest = i
		}
	}
	p := &Project{ID: id, Name: metas[oldest].meta.Name, CreatedAt: metas[oldest].meta.CreatedAt}
	if p.Name == "" {
		for _, m := range metas {
			if m.meta.Name != "" {
				p.Name = m.meta.Name
				break
			}
		}
	}
	lists := make([][]identity.Fingerprint, 0, len(metas))
	seen := map[string]struct{}{}
	for _, m := range metas {
		lists = append(lists, m.meta.Fingerprints)
		seen[m.machine] = struct{}{}
	}
	p.Fingerprints = identity.Union(lists...)
	if p.Fingerprints == nil {
		p.Fingerprints = []identity.Fingerprint{}
	}
	for _, sid := range stateIDs {
		seen[sid] = struct{}{}
	}
	p.Machines = make([]string, 0, len(seen))
	for mid := range seen {
		p.Machines = append(p.Machines, mid)
	}
	sort.Strings(p.Machines)
	return p, warnings, nil
}

// olderThan reports whether a is strictly older than b; a zero time is
// "unknown" and never older than a known time.
func olderThan(a, b time.Time) bool {
	if a.IsZero() {
		return false
	}
	if b.IsZero() {
		return true
	}
	return a.Before(b)
}

// WriteProjectMeta writes this machine's meta file for a project. When the
// vault handle has a writer id, machineID must equal it (ErrForeignMachine).
func (v *Vault) WriteProjectMeta(projectID, machineID string, m ProjectMeta) error {
	if err := v.ready(); err != nil {
		return fmt.Errorf("vault.WriteProjectMeta: %w", err)
	}
	if !validProjectID(projectID) {
		return fmt.Errorf("vault.WriteProjectMeta: %w: %q", ErrBadProjectID, projectID)
	}
	if !ValidMachineID(machineID) {
		return fmt.Errorf("vault.WriteProjectMeta: %w: %q", ErrBadMachineID, machineID)
	}
	if v.writerID != "" && machineID != v.writerID {
		return fmt.Errorf("vault.WriteProjectMeta: %w: meta of %q, writer is %q", ErrForeignMachine, machineID, v.writerID)
	}
	if m.Fingerprints == nil {
		m.Fingerprints = []identity.Fingerprint{}
	}
	if err := v.writeDoc(metaPath(projectID, machineID), m); err != nil {
		return fmt.Errorf("vault.WriteProjectMeta: %s: %w", metaPath(projectID, machineID), err)
	}
	return nil
}

// ReadProjectMeta reads one machine's meta file; (nil, nil) when absent.
func (v *Vault) ReadProjectMeta(projectID, machineID string) (*ProjectMeta, error) {
	if err := v.ready(); err != nil {
		return nil, fmt.Errorf("vault.ReadProjectMeta: %w", err)
	}
	if !validProjectID(projectID) {
		return nil, fmt.Errorf("vault.ReadProjectMeta: %w: %q", ErrBadProjectID, projectID)
	}
	if !ValidMachineID(machineID) {
		return nil, fmt.Errorf("vault.ReadProjectMeta: %w: %q", ErrBadMachineID, machineID)
	}
	rel := metaPath(projectID, machineID)
	var m ProjectMeta
	if err := v.readDoc(rel, &m); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("vault.ReadProjectMeta: %s: %w", rel, err)
	}
	return &m, nil
}

// ---- journals --------------------------------------------------------------

// ReadJournals reads every machine's journal for a project. An undecryptable
// journal returns ErrUnreadableJournal (wrapped, naming the file).
func (v *Vault) ReadJournals(projectID string) (map[string]*Journal, []string, error) {
	if err := v.ready(); err != nil {
		return nil, nil, fmt.Errorf("vault.ReadJournals: %w", err)
	}
	if !validProjectID(projectID) {
		return nil, nil, fmt.Errorf("vault.ReadJournals: %w: %q", ErrBadProjectID, projectID)
	}
	ids, warnings, err := v.listMachineFiles(path.Join(projectDir(projectID), StateDir))
	if err != nil {
		return nil, warnings, fmt.Errorf("vault.ReadJournals: %w", err)
	}
	out := make(map[string]*Journal, len(ids))
	for _, mid := range ids {
		j, err := v.readJournal(projectID, mid)
		if err != nil {
			return nil, warnings, fmt.Errorf("vault.ReadJournals: %w", err)
		}
		if j == nil { // vanished between listing and reading
			continue
		}
		out[mid] = j
	}
	return out, warnings, nil
}

// ReadJournal reads one journal; (nil, nil) when absent.
func (v *Vault) ReadJournal(projectID, machineID string) (*Journal, error) {
	if err := v.ready(); err != nil {
		return nil, fmt.Errorf("vault.ReadJournal: %w", err)
	}
	if !validProjectID(projectID) {
		return nil, fmt.Errorf("vault.ReadJournal: %w: %q", ErrBadProjectID, projectID)
	}
	if !ValidMachineID(machineID) {
		return nil, fmt.Errorf("vault.ReadJournal: %w: %q", ErrBadMachineID, machineID)
	}
	j, err := v.readJournal(projectID, machineID)
	if err != nil {
		return nil, fmt.Errorf("vault.ReadJournal: %w", err)
	}
	return j, nil
}

// readJournal reads and validates one journal: (nil, nil) when absent,
// ErrUnreadableJournal (naming the file) on any decrypt/decode/consistency failure.
func (v *Vault) readJournal(projectID, machineID string) (*Journal, error) {
	rel := statePath(projectID, machineID)
	var j Journal
	if err := v.readDoc(rel, &j); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		if errors.Is(err, ErrClosed) { // the vault, not the journal, is unusable
			return nil, err
		}
		// The cause stays in the chain (crypto.ErrAuth for a tampered file,
		// fs.ErrPermission, fsutil.ErrTooLarge, a JSON error), as ReadBlob does.
		return nil, fmt.Errorf("%w: %s: %w", ErrUnreadableJournal, rel, err)
	}
	switch j.Machine {
	case machineID:
	case "":
		j.Machine = machineID
	default:
		return nil, fmt.Errorf("%w: %s: journal claims machine %q", ErrUnreadableJournal, rel, j.Machine)
	}
	if j.Entries == nil {
		j.Entries = map[string]Entry{}
	}
	// Entries are validated beyond decoding (kind, path shape, Path == key):
	// the sync engine writes files at these paths, so a journal that names
	// "../x" or claims one path under another key is refused as unreadable,
	// with the same defense-in-depth as an unknown kind.
	for p, e := range j.Entries {
		if err := checkEntry(p, e); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrUnreadableJournal, rel, err)
		}
		if e.Path == "" {
			e.Path = p
			j.Entries[p] = e
		}
	}
	return &j, nil
}

// WriteJournal bumps Seq and writes the journal atomically.
// The file written is state/<j.Machine>.json.enc; when the vault handle has
// a writer id, j.Machine must equal it (a machine only writes its own files).
func (v *Vault) WriteJournal(projectID string, j *Journal) error {
	if err := v.ready(); err != nil {
		return fmt.Errorf("vault.WriteJournal: %w", err)
	}
	if j == nil {
		return errors.New("vault.WriteJournal: nil journal")
	}
	if !validProjectID(projectID) {
		return fmt.Errorf("vault.WriteJournal: %w: %q", ErrBadProjectID, projectID)
	}
	if !ValidMachineID(j.Machine) {
		return fmt.Errorf("vault.WriteJournal: %w: %q", ErrBadMachineID, j.Machine)
	}
	if v.writerID != "" && j.Machine != v.writerID {
		return fmt.Errorf("vault.WriteJournal: %w: journal of %q, writer is %q", ErrForeignMachine, j.Machine, v.writerID)
	}
	// Refuse what readJournal would refuse: a journal with an unknown kind, a
	// malformed path or a Path field that disagrees with its key would be
	// written fine and then make the whole project unreadable on the next
	// fetch, on every machine. Nothing is modified until every entry passes.
	for p, e := range j.Entries {
		if err := checkEntry(p, e); err != nil {
			return fmt.Errorf("vault.WriteJournal: %w", err)
		}
	}
	next := *j
	next.Seq = j.Seq + 1
	next.UpdatedAt = time.Now().UTC()
	if next.Entries == nil {
		next.Entries = map[string]Entry{}
	}
	// An unset Path is filled from the key (as readJournal does on the way
	// back), so the in-memory journal matches what lands on disk.
	for p, e := range next.Entries {
		if e.Path == "" {
			e.Path = p
			next.Entries[p] = e
		}
	}
	rel := statePath(projectID, j.Machine)
	if err := v.writeDoc(rel, &next); err != nil {
		return fmt.Errorf("vault.WriteJournal: %s: %w", rel, err)
	}
	j.Seq = next.Seq
	j.UpdatedAt = next.UpdatedAt
	j.Entries = next.Entries
	return nil
}

// ---- machines --------------------------------------------------------------

// ListMachines lists MachineInfo documents.
func (v *Vault) ListMachines() ([]MachineInfo, []string, error) {
	if err := v.ready(); err != nil {
		return nil, nil, fmt.Errorf("vault.ListMachines: %w", err)
	}
	ids, warnings, err := v.listMachineFiles(MachinesDir)
	if err != nil {
		return nil, warnings, fmt.Errorf("vault.ListMachines: %w", err)
	}
	var out []MachineInfo
	for _, mid := range ids {
		rel := machinePath(mid)
		var m MachineInfo
		if err := v.readDoc(rel, &m); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if errors.Is(err, ErrClosed) {
				return nil, warnings, fmt.Errorf("vault.ListMachines: %w", err)
			}
			warnings = append(warnings, fmt.Sprintf("%s: unreadable machine info, skipped: %v", rel, err))
			continue
		}
		// The file name is authoritative (a machine only writes its own file,
		// spec §5): an empty id is filled in, a different one means the file
		// was tampered with or copied over another machine's, so it is
		// skipped with a warning like any other unusable name.
		switch m.ID {
		case mid:
		case "":
			m.ID = mid
		default:
			warnings = append(warnings, fmt.Sprintf("%s: machine info claims id %q, skipped", rel, m.ID))
			continue
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, warnings, nil
}

// WriteMachine writes this machine's info document. When the vault handle
// has a writer id, m.ID must equal it (ErrForeignMachine).
func (v *Vault) WriteMachine(m MachineInfo) error {
	if err := v.ready(); err != nil {
		return fmt.Errorf("vault.WriteMachine: %w", err)
	}
	if !ValidMachineID(m.ID) {
		return fmt.Errorf("vault.WriteMachine: %w: %q", ErrBadMachineID, m.ID)
	}
	if v.writerID != "" && m.ID != v.writerID {
		return fmt.Errorf("vault.WriteMachine: %w: info of %q, writer is %q", ErrForeignMachine, m.ID, v.writerID)
	}
	if err := v.writeDoc(machinePath(m.ID), m); err != nil {
		return fmt.Errorf("vault.WriteMachine: %s: %w", machinePath(m.ID), err)
	}
	return nil
}
