// Package crypto implements the vault key hierarchy and the PSV1 file format.
//
//	KEK      = Argon2id(passphrase, salt)                      -> wraps the vault key VK
//	VK       = 32 random bytes                                 -> stored wrapped in vault.json
//	sub(l)   = HKDF-SHA256(ikm=VK, salt=nil, info="private-sync/v1/"+l), l in {enc, mac, nonce}
//	blob id  = hex(HMAC-SHA256(mac, plaintext))
//	blob     = "PSV1" || HMAC-SHA256(nonce, plaintext)[:24] || XChaCha20-Poly1305(enc, plaintext, aad="blob:"+id)
//	document = "PSV1" || random nonce                         || XChaCha20-Poly1305(enc, plaintext, aad=path)
package crypto

import "errors"

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
)

var (
	// ErrAuth is returned when decryption fails (wrong key or corrupted data).
	ErrAuth = errors.New("decryption failed: wrong key or corrupted data")
	// ErrFormat is returned for malformed ciphertext.
	ErrFormat = errors.New("malformed encrypted data")
	// ErrKDFParams is returned for out-of-range KDF parameters.
	ErrKDFParams = errors.New("invalid KDF parameters")
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
	return KDFParams{}, errors.New("crypto.DefaultKDFParams: not implemented")
}

// Validate enforces the bounds from spec §4 before the KDF is ever run.
func (p KDFParams) Validate() error { return errors.New("crypto.KDFParams.Validate: not implemented") }

// DeriveKEK runs Argon2id and returns the 32-byte key-encryption key.
func DeriveKEK(passphrase []byte, p KDFParams) ([]byte, error) {
	return nil, errors.New("crypto.DeriveKEK: not implemented")
}

// NewVaultKey returns 32 random bytes.
func NewVaultKey() ([]byte, error) { return nil, errors.New("crypto.NewVaultKey: not implemented") }

// WrapKey encrypts vk under kek with a random nonce (document format) and the given AAD.
func WrapKey(kek, vk []byte, aad string) ([]byte, error) {
	return nil, errors.New("crypto.WrapKey: not implemented")
}

// UnwrapKey reverses WrapKey; ErrAuth on wrong kek.
func UnwrapKey(kek, wrapped []byte, aad string) ([]byte, error) {
	return nil, errors.New("crypto.UnwrapKey: not implemented")
}

// Keys holds the three HKDF subkeys derived from the vault key.
type Keys struct {
	enc, mac, nonce []byte
}

// NewKeys derives the enc/mac/nonce subkeys from vk.
func NewKeys(vk []byte) (*Keys, error) { return nil, errors.New("crypto.NewKeys: not implemented") }

// Zero wipes the subkeys.
func (k *Keys) Zero() {}

// BlobID returns hex(HMAC-SHA256(mac, plaintext)).
func (k *Keys) BlobID(plaintext []byte) string { return "" }

// SealBlob encrypts plaintext deterministically; returns the blob id and ciphertext.
func (k *Keys) SealBlob(plaintext []byte) (id string, ciphertext []byte, err error) {
	return "", nil, errors.New("crypto.SealBlob: not implemented")
}

// OpenBlob decrypts a blob given its id (used as AAD).
func (k *Keys) OpenBlob(id string, ciphertext []byte) ([]byte, error) {
	return nil, errors.New("crypto.OpenBlob: not implemented")
}

// SealDoc encrypts a document with a random nonce; aad is the vault-relative path.
func (k *Keys) SealDoc(aad string, plaintext []byte) ([]byte, error) {
	return nil, errors.New("crypto.SealDoc: not implemented")
}

// OpenDoc decrypts a document.
func (k *Keys) OpenDoc(aad string, ciphertext []byte) ([]byte, error) {
	return nil, errors.New("crypto.OpenDoc: not implemented")
}

// KeyedID returns hex(HMAC-SHA256(mac, label || data)) — used for project ids.
func (k *Keys) KeyedID(label string, data []byte) string { return "" }

// RandomHex returns n random bytes hex encoded (2n chars).
func RandomHex(n int) (string, error) { return "", errors.New("crypto.RandomHex: not implemented") }

// Zero overwrites b with zeros.
func Zero(b []byte) {
	for i := range b {
		b[i] = 0
	}
}
