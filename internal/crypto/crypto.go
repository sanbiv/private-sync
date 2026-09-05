// Package crypto implements the vault key hierarchy and the PSV1 file format.
//
//	KEK      = Argon2id(passphrase, salt)                      -> wraps the vault key VK
//	VK       = 32 random bytes                                 -> stored wrapped in vault.json
//	sub(l)   = HKDF-SHA256(ikm=VK, salt=nil, info="private-sync/v1/"+l), l in {enc, mac, nonce}
//	blob id  = hex(HMAC-SHA256(mac, plaintext))
//	blob     = "PSV1" || HMAC-SHA256(nonce, plaintext)[:24] || XChaCha20-Poly1305(enc, plaintext, aad="blob:"+id)
//	document = "PSV1" || random nonce                         || XChaCha20-Poly1305(enc, plaintext, aad=path)
package crypto

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/chacha20poly1305"
	"golang.org/x/crypto/hkdf"
)

const (
	// Magic prefixes every encrypted file.
	Magic = "PSV1"
	// KeySize is the size of every key in bytes.
	KeySize = 32
	// NonceSize is the XChaCha20-Poly1305 nonce size.
	NonceSize = 24
	// SaltSize is the Argon2id salt size.
	SaltSize = 16
	// InfoPrefix is the HKDF info prefix.
	InfoPrefix = "private-sync/v1/"

	// TagSize is the Poly1305 authentication tag size.
	TagSize = chacha20poly1305.Overhead
	// HeaderSize is the size of the PSV1 header: magic + nonce.
	HeaderSize = len(Magic) + NonceSize
	// MinCiphertextSize is the smallest valid PSV1 file (empty plaintext).
	MinCiphertextSize = HeaderSize + TagSize

	// KDFAlgo is the only supported KDF algorithm name.
	KDFAlgo = "argon2id"
	// KDF bounds from spec §4 (memory in KiB).
	KDFMinTime    uint32 = 1
	KDFMaxTime    uint32 = 16
	KDFMinMemory  uint32 = 8 * 1024
	KDFMaxMemory  uint32 = 1024 * 1024
	KDFMinThreads uint8  = 1
	KDFMaxThreads uint8  = 16

	// Default Argon2id costs (RFC 9106 second recommended option).
	DefaultKDFTime    uint32 = 3
	DefaultKDFMemory  uint32 = 64 * 1024
	DefaultKDFThreads uint8  = 4

	// BlobAADPrefix is prepended to the blob id to form the blob AAD.
	BlobAADPrefix = "blob:"
	// VaultKeyAADPrefix is prepended to the vault id to form the AAD of the
	// wrapped vault key stored in vault.json (spec §4: "vault-key:" + vaultID).
	VaultKeyAADPrefix = "vault-key:"
	// ProjectIDPrefix is the KeyedID label for project ids (§4/§7).
	ProjectIDPrefix = "project:"
	// ProjectIDLen is the number of hex characters kept from the project id digest.
	ProjectIDLen = 16
)

var (
	// ErrAuth is returned when decryption fails (wrong key or corrupted data).
	ErrAuth = errors.New("decryption failed: wrong key or corrupted data")
	// ErrFormat is returned for malformed ciphertext.
	ErrFormat = errors.New("malformed encrypted data")
	// ErrKDFParams is returned for out-of-range KDF parameters.
	ErrKDFParams = errors.New("invalid KDF parameters")
	// ErrKeySize is returned when a key has the wrong length.
	ErrKeySize = errors.New("key must be exactly 32 bytes")
	// ErrAAD is returned when a document AAD is empty or not in canonical
	// (slash-separated) form.
	ErrAAD = errors.New("document aad must be a non-empty slash-separated vault-relative path")
)

// KDFParams are the Argon2id parameters stored in vault.json.
type KDFParams struct {
	Algo    string `json:"algo"`    // "argon2id"
	Salt    []byte `json:"salt"`    // exactly 16 bytes (base64 in JSON)
	Time    uint32 `json:"time"`    // [1,16]
	Memory  uint32 `json:"memory"`  // KiB, [8 MiB, 1 GiB]
	Threads uint8  `json:"threads"` // [1,16]
}

// DefaultKDFParams returns time=3, memory=64MiB, threads=4 with a fresh random salt.
func DefaultKDFParams() (KDFParams, error) {
	salt := make([]byte, SaltSize)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return KDFParams{}, fmt.Errorf("crypto.DefaultKDFParams: random salt: %w", err)
	}
	return KDFParams{
		Algo:    KDFAlgo,
		Salt:    salt,
		Time:    DefaultKDFTime,
		Memory:  DefaultKDFMemory,
		Threads: DefaultKDFThreads,
	}, nil
}

// Validate enforces the bounds from spec §4 before the KDF is ever run.
func (p KDFParams) Validate() error {
	if p.Algo != KDFAlgo {
		return fmt.Errorf("%w: algo %q (want %q)", ErrKDFParams, p.Algo, KDFAlgo)
	}
	if len(p.Salt) != SaltSize {
		return fmt.Errorf("%w: salt is %d bytes (want %d)", ErrKDFParams, len(p.Salt), SaltSize)
	}
	if p.Time < KDFMinTime || p.Time > KDFMaxTime {
		return fmt.Errorf("%w: time %d outside [%d,%d]", ErrKDFParams, p.Time, KDFMinTime, KDFMaxTime)
	}
	if p.Memory < KDFMinMemory || p.Memory > KDFMaxMemory {
		return fmt.Errorf("%w: memory %d KiB outside [%d,%d]", ErrKDFParams, p.Memory, KDFMinMemory, KDFMaxMemory)
	}
	if p.Threads < KDFMinThreads || p.Threads > KDFMaxThreads {
		return fmt.Errorf("%w: threads %d outside [%d,%d]", ErrKDFParams, p.Threads, KDFMinThreads, KDFMaxThreads)
	}
	return nil
}

// DeriveKEK runs Argon2id and returns the 32-byte key-encryption key.
//
// The passphrase buffer is zeroed right after Argon2id has consumed it
// (spec §4), so a caller that needs it again (e.g. to derive under fresh
// parameters) must pass a copy. When p fails Validate the KDF never runs
// and the passphrase is left untouched, so the caller can retry with
// corrected parameters (or wipe it itself). The returned KEK is the
// caller's to Zero once the vault key is unwrapped.
func DeriveKEK(passphrase []byte, p KDFParams) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, fmt.Errorf("crypto.DeriveKEK: %w", err)
	}
	kek := argon2.IDKey(passphrase, p.Salt, p.Time, p.Memory, p.Threads, KeySize)
	Zero(passphrase)
	return kek, nil
}

// NewVaultKey returns 32 random bytes.
func NewVaultKey() ([]byte, error) {
	vk := make([]byte, KeySize)
	if _, err := io.ReadFull(rand.Reader, vk); err != nil {
		return nil, fmt.Errorf("crypto.NewVaultKey: %w", err)
	}
	return vk, nil
}

// WrapKey encrypts vk under kek with a random nonce (document format) and the given AAD.
func WrapKey(kek, vk []byte, aad string) ([]byte, error) {
	if len(vk) != KeySize {
		return nil, fmt.Errorf("crypto.WrapKey: vault key: %w", ErrKeySize)
	}
	out, err := sealRandom(kek, aad, vk)
	if err != nil {
		return nil, fmt.Errorf("crypto.WrapKey: %w", err)
	}
	return out, nil
}

// UnwrapKey reverses WrapKey; ErrAuth on wrong kek.
func UnwrapKey(kek, wrapped []byte, aad string) ([]byte, error) {
	vk, err := open(kek, aad, wrapped)
	if err != nil {
		return nil, fmt.Errorf("crypto.UnwrapKey: %w", err)
	}
	if len(vk) != KeySize {
		Zero(vk)
		return nil, fmt.Errorf("crypto.UnwrapKey: unwrapped key: %w", ErrKeySize)
	}
	return vk, nil
}

// Keys holds the three HKDF subkeys derived from the vault key.
//
// All methods are safe for concurrent use: readers (BlobID, Seal*, Open*,
// KeyedID, ProjectID) share a read lock and Zero takes the write lock, so
// a Zero racing with an in-flight operation either waits for it to finish
// or makes the next operation fail with the not-ready error — never a
// half-wiped key feeding the AEAD.
type Keys struct {
	enc, mac, nonce []byte

	mu sync.RWMutex // guards enc, mac, nonce
}

// NewKeys derives the enc/mac/nonce subkeys from vk.
func NewKeys(vk []byte) (*Keys, error) {
	if len(vk) != KeySize {
		return nil, fmt.Errorf("crypto.NewKeys: vault key: %w", ErrKeySize)
	}
	k := &Keys{}
	var err error
	if k.enc, err = deriveSubkey(vk, "enc"); err != nil {
		return nil, fmt.Errorf("crypto.NewKeys: %w", err)
	}
	if k.mac, err = deriveSubkey(vk, "mac"); err != nil {
		k.Zero()
		return nil, fmt.Errorf("crypto.NewKeys: %w", err)
	}
	if k.nonce, err = deriveSubkey(vk, "nonce"); err != nil {
		k.Zero()
		return nil, fmt.Errorf("crypto.NewKeys: %w", err)
	}
	return k, nil
}

// deriveSubkey returns HKDF-SHA256(ikm=vk, salt=nil, info=InfoPrefix+label, 32 bytes).
func deriveSubkey(vk []byte, label string) ([]byte, error) {
	r := hkdf.New(sha256.New, vk, nil, []byte(InfoPrefix+label))
	out := make([]byte, KeySize)
	if _, err := io.ReadFull(r, out); err != nil {
		return nil, fmt.Errorf("hkdf %q: %w", label, err)
	}
	return out, nil
}

// Zero wipes the subkeys and releases them, so every later method on k
// fails (errKeysNotReady) or returns an empty id instead of silently
// operating with all-zero keys. Safe on nil, idempotent, and safe to call
// while other goroutines are using k (it waits for in-flight operations).
func (k *Keys) Zero() {
	if k == nil {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	Zero(k.enc)
	Zero(k.mac)
	Zero(k.nonce)
	k.enc, k.mac, k.nonce = nil, nil, nil
}

// rlock takes the read lock and reports whether the subkeys are present and
// of the right size. The caller must release the lock (via the returned
// func) whatever the result; on a nil receiver there is no lock to take and
// the release is a no-op.
func (k *Keys) rlock() (ready bool, release func()) {
	if k == nil {
		return false, func() {}
	}
	k.mu.RLock()
	return k.ready(), k.mu.RUnlock
}

// ready reports whether the subkeys are present and of the right size.
// Callers must hold k.mu (read or write).
func (k *Keys) ready() bool {
	return k != nil && len(k.enc) == KeySize && len(k.mac) == KeySize && len(k.nonce) == KeySize
}

// BlobID returns hex(HMAC-SHA256(mac, plaintext)).
func (k *Keys) BlobID(plaintext []byte) string {
	ready, release := k.rlock()
	defer release()
	if !ready {
		return ""
	}
	return k.blobID(plaintext)
}

// blobID is BlobID without locking; the caller holds k.mu and has checked ready().
func (k *Keys) blobID(plaintext []byte) string {
	return hex.EncodeToString(hmacSHA256(k.mac, plaintext))
}

// SealBlob encrypts plaintext deterministically; returns the blob id and ciphertext.
func (k *Keys) SealBlob(plaintext []byte) (id string, ciphertext []byte, err error) {
	ready, release := k.rlock()
	defer release()
	if !ready {
		return "", nil, fmt.Errorf("crypto.SealBlob: %w", errKeysNotReady)
	}
	id = k.blobID(plaintext)
	nonce := hmacSHA256(k.nonce, plaintext)[:NonceSize]
	ciphertext, err = seal(k.enc, nonce, BlobAADPrefix+id, plaintext)
	if err != nil {
		return "", nil, fmt.Errorf("crypto.SealBlob: %w", err)
	}
	return id, ciphertext, nil
}

// OpenBlob decrypts a blob given its id (used as AAD).
func (k *Keys) OpenBlob(id string, ciphertext []byte) ([]byte, error) {
	ready, release := k.rlock()
	defer release()
	if !ready {
		return nil, fmt.Errorf("crypto.OpenBlob: %w", errKeysNotReady)
	}
	pt, err := open(k.enc, BlobAADPrefix+id, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("crypto.OpenBlob: %w", err)
	}
	return pt, nil
}

// SealDoc encrypts a document with a random nonce.
//
// aad binds the document to its location: the vault-relative path in
// canonical slash-separated form (build it with path.Join or
// filepath.ToSlash, never filepath.Join, so a document sealed on Windows
// opens on macOS/Linux and vice versa), or "trash:" + id for trash blobs.
// An empty aad or one containing a backslash is rejected with ErrAAD.
func (k *Keys) SealDoc(aad string, plaintext []byte) ([]byte, error) {
	ready, release := k.rlock()
	defer release()
	if !ready {
		return nil, fmt.Errorf("crypto.SealDoc: %w", errKeysNotReady)
	}
	if err := checkDocAAD(aad); err != nil {
		return nil, fmt.Errorf("crypto.SealDoc: %w", err)
	}
	out, err := sealRandom(k.enc, aad, plaintext)
	if err != nil {
		return nil, fmt.Errorf("crypto.SealDoc: %w", err)
	}
	return out, nil
}

// OpenDoc decrypts a document sealed with SealDoc under the same aad
// (see SealDoc for the canonical form). An empty aad or a backslash in aad
// is reported as ErrAAD rather than surfacing as a confusing ErrAuth.
func (k *Keys) OpenDoc(aad string, ciphertext []byte) ([]byte, error) {
	ready, release := k.rlock()
	defer release()
	if !ready {
		return nil, fmt.Errorf("crypto.OpenDoc: %w", errKeysNotReady)
	}
	if err := checkDocAAD(aad); err != nil {
		return nil, fmt.Errorf("crypto.OpenDoc: %w", err)
	}
	pt, err := open(k.enc, aad, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("crypto.OpenDoc: %w", err)
	}
	return pt, nil
}

// checkDocAAD rejects non-canonical document AADs. Spec §4 binds every
// document to its vault-relative path or "trash:"+id, so an empty AAD would
// silently drop the move-protection binding (a caller passing an unset path
// would still produce a valid-looking PSV1 file); spec §5 requires slash
// separated paths, so a backslash is refused too. Everything else is the
// caller's business.
func checkDocAAD(aad string) error {
	if aad == "" {
		return fmt.Errorf("%w: empty", ErrAAD)
	}
	if strings.ContainsRune(aad, '\\') {
		return fmt.Errorf("%w: %q contains a backslash", ErrAAD, aad)
	}
	return nil
}

// KeyedID returns the full 64-hex digest hex(HMAC-SHA256(mac, label || data)).
// label and data are plainly concatenated (no delimiter), so the label must
// carry its own terminator (e.g. "project:").
//
// Callers truncate as the spec requires: project ids are the first
// ProjectIDLen (16) hex characters (§4/§7) — use ProjectID for that.
func (k *Keys) KeyedID(label string, data []byte) string {
	ready, release := k.rlock()
	defer release()
	if !ready {
		return ""
	}
	return k.keyedID(label, data)
}

// keyedID is KeyedID without locking; the caller holds k.mu and has checked ready().
func (k *Keys) keyedID(label string, data []byte) string {
	m := hmac.New(sha256.New, k.mac)
	m.Write([]byte(label))
	m.Write(data)
	return hex.EncodeToString(m.Sum(nil))
}

// ProjectID returns the opaque project id for a fingerprint string
// (identity.Fingerprint.String(), i.e. "kind:value"):
// hex(HMAC-SHA256(mac, "project:" + fingerprint))[:16], as spec §4/§7 define.
// Returns "" when the keys are not ready.
func (k *Keys) ProjectID(fingerprint string) string {
	ready, release := k.rlock()
	defer release()
	if !ready {
		return ""
	}
	id := k.keyedID(ProjectIDPrefix, []byte(fingerprint))
	if len(id) < ProjectIDLen {
		return ""
	}
	return id[:ProjectIDLen]
}

// RandomHex returns n random bytes hex encoded (2n chars).
func RandomHex(n int) (string, error) {
	if n < 0 {
		return "", fmt.Errorf("crypto.RandomHex: negative length %d", n)
	}
	b := make([]byte, n)
	if _, err := io.ReadFull(rand.Reader, b); err != nil {
		return "", fmt.Errorf("crypto.RandomHex: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// Zero overwrites b with zeros.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

var errKeysNotReady = errors.New("subkeys not initialised")

// hmacSHA256 returns HMAC-SHA256(key, data).
func hmacSHA256(key, data []byte) []byte {
	m := hmac.New(sha256.New, key)
	m.Write(data)
	return m.Sum(nil)
}

// seal produces "PSV1" || nonce || AEAD(key, nonce, plaintext, aad).
func seal(key, nonce []byte, aad string, plaintext []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	if len(nonce) != NonceSize {
		return nil, fmt.Errorf("nonce must be %d bytes", NonceSize)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, HeaderSize+len(plaintext)+TagSize)
	out = append(out, Magic...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plaintext, []byte(aad)), nil
}

// sealRandom is seal with a fresh random nonce (document format).
func sealRandom(key []byte, aad string, plaintext []byte) ([]byte, error) {
	nonce := make([]byte, NonceSize)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("random nonce: %w", err)
	}
	return seal(key, nonce, aad, plaintext)
}

// open parses the PSV1 header (ErrFormat) and decrypts (ErrAuth).
func open(key []byte, aad string, ciphertext []byte) ([]byte, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	if len(ciphertext) < MinCiphertextSize {
		return nil, fmt.Errorf("%w: %d bytes, need at least %d", ErrFormat, len(ciphertext), MinCiphertextSize)
	}
	if string(ciphertext[:len(Magic)]) != Magic {
		return nil, fmt.Errorf("%w: bad magic", ErrFormat)
	}
	aead, err := chacha20poly1305.NewX(key)
	if err != nil {
		return nil, err
	}
	nonce := ciphertext[len(Magic):HeaderSize]
	pt, err := aead.Open(nil, nonce, ciphertext[HeaderSize:], []byte(aad))
	if err != nil {
		return nil, ErrAuth
	}
	return pt, nil
}
