package vault

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/sanbiv/private-sync/internal/crypto"
	"github.com/sanbiv/private-sync/internal/fsutil"
	"github.com/sanbiv/private-sync/internal/identity"
)

// Machine ids with the uuid shape the vault requires for file names.
const (
	mA = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	mB = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	mC = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"

	projA = "0123456789abcdef"
	projB = "fedcba9876543210"
)

// pass returns a fresh passphrase buffer (Create/Open zero their argument).
func pass() []byte { return []byte("correct horse battery staple") }

// fastParams returns the cheapest KDF parameters the spec allows (8 MiB,
// time 1, one thread) so Argon2id costs milliseconds in tests.
func fastParams(t *testing.T) crypto.KDFParams {
	t.Helper()
	p, err := crypto.DefaultKDFParams()
	if err != nil {
		t.Fatalf("DefaultKDFParams: %v", err)
	}
	p.Time = crypto.KDFMinTime
	p.Memory = crypto.KDFMinMemory
	p.Threads = crypto.KDFMinThreads
	return p
}

// newVault creates a vault in a temp dir (writer mA) and closes it on cleanup.
func newVault(t *testing.T) (*Vault, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "vault")
	v, err := Create(dir, pass(), fastParams(t), mA)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(v.Close)
	return v, dir
}

// openAs opens dir as writer machine and closes it on cleanup.
func openAs(t *testing.T, dir, machine string) *Vault {
	t.Helper()
	v, err := Open(dir, pass(), machine)
	if err != nil {
		t.Fatalf("Open as %s: %v", machine, err)
	}
	t.Cleanup(v.Close)
	return v
}

func writeRaw(t *testing.T, dir, rel string, data []byte) {
	t.Helper()
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func fileMode(t *testing.T, p string) os.FileMode {
	t.Helper()
	st, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return st.Mode().Perm()
}

func fp(kind, value string, level identity.Level) identity.Fingerprint {
	return identity.Fingerprint{Kind: kind, Value: value, Level: level}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// ---- vault.json lifecycle ---------------------------------------------------

func TestCreateOpenRoundTrip(t *testing.T) {
	v, dir := newVault(t)

	if len(v.ID()) != 16 || !isHex(v.ID()) {
		t.Errorf("ID() = %q, want 16 hex chars", v.ID())
	}
	if v.Dir() != dir {
		t.Errorf("Dir() = %q, want %q", v.Dir(), dir)
	}
	if v.WriterID() != mA {
		t.Errorf("WriterID() = %q, want %q", v.WriterID(), mA)
	}
	if v.Keys() == nil {
		t.Fatal("Keys() is nil")
	}
	if !Exists(dir) {
		t.Error("Exists(dir) = false after Create")
	}
	if Exists(filepath.Join(dir, "nope")) {
		t.Error("Exists(missing) = true")
	}
	if runtime.GOOS != "windows" {
		if m := fileMode(t, filepath.Join(dir, VaultFileName)); m != 0o600 {
			t.Errorf("vault.json mode = %o, want 600", m)
		}
		if m := fileMode(t, dir); m != 0o700 {
			t.Errorf("vault dir mode = %o, want 700", m)
		}
	}
	if got := v.Written(); !reflect.DeepEqual(got, []string{VaultFileName}) {
		t.Errorf("Written() after Create = %v, want [vault.json]", got)
	}
	if v.CreatedAt().IsZero() {
		t.Error("CreatedAt() is zero")
	}

	// Nothing but vault.json is written by Create.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != VaultFileName {
		t.Errorf("Create wrote %v, want only vault.json", entries)
	}

	// The passphrase buffer is zeroed.
	pw := pass()
	_, err = Create(filepath.Join(t.TempDir(), "v2"), pw, fastParams(t), mA)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Error("Create did not zero the passphrase")
	}

	// A second Create on the same dir is refused.
	if _, err := Create(dir, pass(), fastParams(t), mA); !errors.Is(err, ErrVaultExists) {
		t.Errorf("second Create: err = %v, want ErrVaultExists", err)
	}

	// ReadID needs no passphrase.
	id, err := ReadID(dir)
	if err != nil || id != v.ID() {
		t.Errorf("ReadID = %q, %v; want %q", id, err, v.ID())
	}

	// Open yields the same id and the same keys (same blob id for same bytes).
	blob, created, err := v.WriteBlob([]byte("hello"))
	if err != nil || !created {
		t.Fatalf("WriteBlob: %s %v %v", blob, created, err)
	}
	pw = pass()
	v2, err := Open(dir, pw, mB)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer v2.Close()
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Error("Open did not zero the passphrase")
	}
	if v2.ID() != v.ID() {
		t.Errorf("Open ID = %q, want %q", v2.ID(), v.ID())
	}
	if v2.BlobID([]byte("hello")) != blob {
		t.Error("reopened vault derives different keys")
	}
	pt, err := v2.ReadBlob(blob)
	if err != nil || string(pt) != "hello" {
		t.Errorf("ReadBlob after reopen = %q, %v", pt, err)
	}
	if got := v2.Written(); len(got) != 0 {
		t.Errorf("Written() after Open = %v, want empty", got)
	}
	if !reflect.DeepEqual(v2.KDFParams(), v.KDFParams()) {
		t.Error("KDFParams differ between Create and Open")
	}
}

func TestCreateDefaultsAndErrors(t *testing.T) {
	// Zero params select the defaults (with a fresh salt); the vault directory
	// (and its parents) are created.
	dir := filepath.Join(t.TempDir(), "a", "b", "vault")
	v, err := Create(dir, pass(), crypto.KDFParams{}, mA)
	if err != nil {
		t.Fatalf("Create with zero params: %v", err)
	}
	defer v.Close()
	p := v.KDFParams()
	if p.Algo != crypto.KDFAlgo || p.Time != crypto.DefaultKDFTime || p.Memory != crypto.DefaultKDFMemory ||
		p.Threads != crypto.DefaultKDFThreads || len(p.Salt) != crypto.SaltSize {
		t.Errorf("default params not applied: %+v", p)
	}

	// Params without a salt get one.
	dir2 := filepath.Join(t.TempDir(), "v")
	fp := fastParams(t)
	fp.Salt = nil
	v2, err := Create(dir2, pass(), fp, mA)
	if err != nil {
		t.Fatalf("Create without salt: %v", err)
	}
	defer v2.Close()
	if len(v2.KDFParams().Salt) != crypto.SaltSize {
		t.Error("missing salt was not generated")
	}

	// Out-of-range params are refused before anything is written.
	bad := fastParams(t)
	bad.Time = 99
	dir3 := filepath.Join(t.TempDir(), "v")
	if _, err := Create(dir3, pass(), bad, mA); !errors.Is(err, crypto.ErrKDFParams) {
		t.Errorf("Create with bad params: err = %v, want ErrKDFParams", err)
	}
	if fsutil.Exists(dir3) {
		t.Error("Create with bad params created the directory")
	}

	if _, err := Create("", pass(), fastParams(t), mA); err == nil {
		t.Error("Create(\"\") succeeded")
	}
}

func TestOpenErrors(t *testing.T) {
	_, dir := newVault(t)

	if _, err := Open(dir, []byte("wrong"), mA); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("wrong passphrase: err = %v, want ErrWrongPassphrase", err)
	}
	missing := filepath.Join(t.TempDir(), "none")
	if _, err := Open(missing, pass(), mA); !errors.Is(err, ErrNoVault) {
		t.Errorf("missing vault: err = %v, want ErrNoVault", err)
	}
	if _, err := ReadID(missing); !errors.Is(err, ErrNoVault) {
		t.Errorf("ReadID missing: err = %v, want ErrNoVault", err)
	}
	if _, err := Open("", pass(), mA); !errors.Is(err, ErrNoVault) {
		t.Errorf("Open(\"\"): err = %v, want ErrNoVault", err)
	}
}

// tamper rewrites vault.json through a generic map so any field can be mangled.
func tamper(t *testing.T, dir string, mutate func(m map[string]any)) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, VaultFileName))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	mutate(m)
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, VaultFileName), out, 0o600); err != nil {
		t.Fatal(err)
	}
}

func kdf(m map[string]any) map[string]any { return m["kdf"].(map[string]any) }

func TestOpenTamperedVaultFile(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(m map[string]any)
		want   error
	}{
		{"bumped version", func(m map[string]any) { m["version"] = Version + 1 }, ErrNewerVersion},
		{"version zero", func(m map[string]any) { m["version"] = 0 }, ErrInvalidVault},
		{"salt too short", func(m map[string]any) { kdf(m)["salt"] = base64.StdEncoding.EncodeToString([]byte("short")) }, ErrInvalidVault},
		{"salt too long", func(m map[string]any) { kdf(m)["salt"] = base64.StdEncoding.EncodeToString(make([]byte, 32)) }, ErrInvalidVault},
		// 1<<31 KiB = 2 TiB: if the KDF ran this would exhaust memory.
		{"huge memory", func(m map[string]any) { kdf(m)["memory"] = uint32(1) << 31 }, ErrInvalidVault},
		{"memory below minimum", func(m map[string]any) { kdf(m)["memory"] = 1024 }, ErrInvalidVault},
		{"time too high", func(m map[string]any) { kdf(m)["time"] = 1000000 }, ErrInvalidVault},
		{"threads zero", func(m map[string]any) { kdf(m)["threads"] = 0 }, ErrInvalidVault},
		{"unknown algo", func(m map[string]any) { kdf(m)["algo"] = "scrypt" }, ErrInvalidVault},
		{"empty id", func(m map[string]any) { m["id"] = "" }, ErrInvalidVault},
		{"non-hex id", func(m map[string]any) { m["id"] = "not-hex!" }, ErrInvalidVault},
		{"id too short", func(m map[string]any) { m["id"] = "abcd" }, ErrInvalidVault},
		{"id too long", func(m map[string]any) { m["id"] = strings.Repeat("ab", 16) }, ErrInvalidVault},
		{"wrapped key truncated", func(m map[string]any) {
			wk, _ := base64.StdEncoding.DecodeString(m["wrapped_key"].(string))
			m["wrapped_key"] = base64.StdEncoding.EncodeToString(wk[:len(wk)-1])
		}, ErrInvalidVault},
		{"wrapped key without magic", func(m map[string]any) {
			wk, _ := base64.StdEncoding.DecodeString(m["wrapped_key"].(string))
			copy(wk, "XXXX")
			m["wrapped_key"] = base64.StdEncoding.EncodeToString(wk)
		}, ErrInvalidVault},
		{"wrong type", func(m map[string]any) { m["kdf"] = "argon2id" }, ErrInvalidVault},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, dir := newVault(t)
			tamper(t, dir, tc.mutate)
			start := time.Now()
			_, err := Open(dir, pass(), mA)
			elapsed := time.Since(start)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Open: err = %v, want %v", err, tc.want)
			}
			if errors.Is(err, ErrWrongPassphrase) {
				t.Error("tampered vault reported as wrong passphrase")
			}
			// The KDF must not have run: validation is a few microseconds,
			// Argon2id at these sizes would take far longer (or never finish).
			if elapsed > time.Second {
				t.Errorf("Open took %v; the KDF ran before validation", elapsed)
			}
		})
	}

	t.Run("garbage json", func(t *testing.T) {
		_, dir := newVault(t)
		writeRaw(t, dir, VaultFileName, []byte("{not json"))
		if _, err := Open(dir, pass(), mA); !errors.Is(err, ErrInvalidVault) {
			t.Errorf("garbage: err = %v, want ErrInvalidVault", err)
		}
		if _, err := ReadID(dir); !errors.Is(err, ErrInvalidVault) {
			t.Errorf("ReadID garbage: err = %v, want ErrInvalidVault", err)
		}
	})

	t.Run("trailing data", func(t *testing.T) {
		// json.Decoder stops after the first value; junk appended to an
		// otherwise valid vault.json must still be refused.
		_, dir := newVault(t)
		raw, err := os.ReadFile(filepath.Join(dir, VaultFileName))
		if err != nil {
			t.Fatal(err)
		}
		for name, tail := range map[string][]byte{
			"junk bytes":      []byte("garbage"),
			"second document": []byte(`{"version": 99}`),
			"stray brace":     []byte("}"),
		} {
			writeRaw(t, dir, VaultFileName, append(append([]byte(nil), raw...), tail...))
			if _, err := Open(dir, pass(), mA); !errors.Is(err, ErrInvalidVault) {
				t.Errorf("%s: Open err = %v, want ErrInvalidVault", name, err)
			}
			if _, err := ReadID(dir); !errors.Is(err, ErrInvalidVault) {
				t.Errorf("%s: ReadID err = %v, want ErrInvalidVault", name, err)
			}
		}
		// Trailing whitespace is not data.
		writeRaw(t, dir, VaultFileName, append(append([]byte(nil), raw...), "\n\n  \t\n"...))
		v, err := Open(dir, pass(), mA)
		if err != nil {
			t.Fatalf("trailing whitespace: Open err = %v", err)
		}
		v.Close()
	})

	t.Run("wrapped key ciphertext flipped", func(t *testing.T) {
		// Same length and magic, but the ciphertext no longer authenticates:
		// indistinguishable from a wrong passphrase by design.
		_, dir := newVault(t)
		tamper(t, dir, func(m map[string]any) {
			wk, _ := base64.StdEncoding.DecodeString(m["wrapped_key"].(string))
			wk[len(wk)-1] ^= 0xff
			m["wrapped_key"] = base64.StdEncoding.EncodeToString(wk)
		})
		if _, err := Open(dir, pass(), mA); !errors.Is(err, ErrWrongPassphrase) {
			t.Errorf("flipped ciphertext: err = %v, want ErrWrongPassphrase", err)
		}
	})

	t.Run("id changed breaks aad", func(t *testing.T) {
		_, dir := newVault(t)
		tamper(t, dir, func(m map[string]any) { m["id"] = "00000000deadbeef" })
		if _, err := Open(dir, pass(), mA); !errors.Is(err, ErrWrongPassphrase) {
			t.Errorf("changed id: err = %v, want ErrWrongPassphrase (AAD mismatch)", err)
		}
	})
}

func TestVaultFileShape(t *testing.T) {
	v, dir := newVault(t)
	raw, err := os.ReadFile(filepath.Join(dir, VaultFileName))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "id", "kdf", "wrapped_key", "created_at"} {
		if _, ok := m[key]; !ok {
			t.Errorf("vault.json lacks %q", key)
		}
	}
	var version int
	if err := json.Unmarshal(m["version"], &version); err != nil || version != Version {
		t.Errorf("version = %s, want %d", m["version"], Version)
	}
	var id string
	if err := json.Unmarshal(m["id"], &id); err != nil || id != v.ID() {
		t.Errorf("id = %s, want %q", m["id"], v.ID())
	}
	var wk string
	if err := json.Unmarshal(m["wrapped_key"], &wk); err != nil {
		t.Fatalf("wrapped_key is not a string: %s", m["wrapped_key"])
	}
	dec, err := base64.StdEncoding.DecodeString(wk)
	if err != nil {
		t.Fatalf("wrapped_key is not base64: %v", err)
	}
	if !bytes.HasPrefix(dec, []byte(crypto.Magic)) {
		t.Error("wrapped_key lacks the PSV1 magic")
	}
	var k crypto.KDFParams
	if err := json.Unmarshal(m["kdf"], &k); err != nil || k.Validate() != nil {
		t.Errorf("kdf = %s does not validate: %v", m["kdf"], err)
	}
}

func TestRekey(t *testing.T) {
	v, dir := newVault(t)
	before := v.KDFParams()
	blob, _, err := v.WriteBlob([]byte("kept across rekey"))
	if err != nil {
		t.Fatal(err)
	}

	newParams := fastParams(t)
	newParams.Time = 2
	newPass := []byte("new passphrase")
	if err := v.Rekey(newPass, newParams); err != nil {
		t.Fatalf("Rekey: %v", err)
	}
	if !bytes.Equal(newPass, make([]byte, len(newPass))) {
		t.Error("Rekey did not zero the passphrase")
	}
	after := v.KDFParams()
	if bytes.Equal(after.Salt, before.Salt) {
		t.Error("Rekey reused the old salt")
	}
	if bytes.Equal(after.Salt, newParams.Salt) {
		t.Error("Rekey used the caller's salt instead of a fresh one")
	}
	if after.Time != 2 {
		t.Errorf("Rekey Time = %d, want 2", after.Time)
	}

	// Old passphrase no longer works; the new one does; id and blobs are unchanged.
	if _, err := Open(dir, pass(), mA); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("Open with old passphrase: err = %v, want ErrWrongPassphrase", err)
	}
	v2, err := Open(dir, []byte("new passphrase"), mA)
	if err != nil {
		t.Fatalf("Open with new passphrase: %v", err)
	}
	defer v2.Close()
	if v2.ID() != v.ID() {
		t.Error("Rekey changed the vault id")
	}
	pt, err := v2.ReadBlob(blob)
	if err != nil || string(pt) != "kept across rekey" {
		t.Errorf("blob after rekey = %q, %v", pt, err)
	}
	if id, _ := ReadID(dir); id != v.ID() {
		t.Error("ReadID changed after rekey")
	}

	// Zero params keep the current costs (fresh salt again).
	if err := v2.Rekey([]byte("third"), crypto.KDFParams{}); err != nil {
		t.Fatalf("Rekey with zero params: %v", err)
	}
	if p := v2.KDFParams(); p.Time != 2 || p.Memory != after.Memory || bytes.Equal(p.Salt, after.Salt) {
		t.Errorf("Rekey with zero params: %+v", p)
	}
	v3, err := Open(dir, []byte("third"), mA)
	if err != nil {
		t.Fatalf("Open after second rekey: %v", err)
	}
	v3.Close()

	// Invalid params are refused and vault.json left intact.
	bad := fastParams(t)
	bad.Threads = 0
	if err := v2.Rekey([]byte("x"), bad); !errors.Is(err, crypto.ErrKDFParams) {
		t.Errorf("Rekey bad params: err = %v, want ErrKDFParams", err)
	}
	v4, err := Open(dir, []byte("third"), mA)
	if err != nil {
		t.Fatalf("Open after failed rekey: %v", err)
	}
	v4.Close()
}

func TestClose(t *testing.T) {
	v, _ := newVault(t)
	blob, _, err := v.WriteBlob([]byte("x"))
	if err != nil {
		t.Fatal(err)
	}
	v.Close()
	v.Close() // idempotent

	if _, _, err := v.WriteBlob([]byte("y")); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteBlob after Close: %v", err)
	}
	if _, err := v.ReadBlob(blob); !errors.Is(err, ErrClosed) {
		t.Errorf("ReadBlob after Close: %v", err)
	}
	if _, err := v.SealDoc("a/b", []byte("x")); !errors.Is(err, ErrClosed) {
		t.Errorf("SealDoc after Close: %v", err)
	}
	if err := v.Rekey([]byte("p"), crypto.KDFParams{}); !errors.Is(err, ErrClosed) {
		t.Errorf("Rekey after Close: %v", err)
	}
	if _, _, err := v.ListProjects(); !errors.Is(err, ErrClosed) {
		t.Errorf("ListProjects after Close: %v", err)
	}
	if err := v.WriteJournal(projA, &Journal{Machine: mA}); !errors.Is(err, ErrClosed) {
		t.Errorf("WriteJournal after Close: %v", err)
	}
	if v.BlobID([]byte("x")) != "" {
		t.Error("BlobID after Close still derives ids")
	}

	var nilVault *Vault
	nilVault.Close()
	if got := nilVault.Written(); got != nil {
		t.Errorf("nil.Written() = %v", got)
	}
	if nilVault.HasBlob(blob) {
		t.Error("nil.HasBlob = true")
	}
	if _, err := nilVault.ReadBlob(blob); !errors.Is(err, ErrClosed) {
		t.Errorf("nil.ReadBlob: %v", err)
	}
}

// ---- blobs ------------------------------------------------------------------

func TestBlobPath(t *testing.T) {
	tests := []struct{ id, want string }{
		{"abcdef0123", "blobs/ab/abcdef0123.enc"},
		{"ab", "blobs/ab/ab.enc"},
		{"a", ""},
		{"", ""},
		{"ABCDEF", ""},
		{"../../x", ""},
		{"ab/cd", ""},
	}
	for _, tc := range tests {
		if got := BlobPath(tc.id); got != tc.want {
			t.Errorf("BlobPath(%q) = %q, want %q", tc.id, got, tc.want)
		}
	}
}

func TestBlobs(t *testing.T) {
	v, dir := newVault(t)
	pt := []byte("some secret content")

	id, created, err := v.WriteBlob(pt)
	if err != nil {
		t.Fatalf("WriteBlob: %v", err)
	}
	if !created {
		t.Error("first WriteBlob: created = false")
	}
	if id != v.BlobID(pt) || id != v.Keys().BlobID(pt) {
		t.Error("WriteBlob id differs from BlobID")
	}
	rel := BlobPath(id)
	abs := filepath.Join(dir, filepath.FromSlash(rel))
	if !fsutil.Exists(abs) {
		t.Fatalf("blob file %s missing", rel)
	}
	if runtime.GOOS != "windows" {
		if m := fileMode(t, abs); m != 0o600 {
			t.Errorf("blob mode = %o, want 600", m)
		}
	}
	first, err := os.ReadFile(abs)
	if err != nil {
		t.Fatal(err)
	}

	// Identical plaintext: deterministic bytes, no rewrite.
	id2, created, err := v.WriteBlob(append([]byte(nil), pt...))
	if err != nil {
		t.Fatal(err)
	}
	if id2 != id || created {
		t.Errorf("second WriteBlob: id=%s created=%v", id2, created)
	}
	second, _ := os.ReadFile(abs)
	if !bytes.Equal(first, second) {
		t.Error("blob bytes changed on rewrite")
	}
	if got := v.Written(); !reflect.DeepEqual(got, []string{VaultFileName, rel}) {
		t.Errorf("Written() = %v", got)
	}

	// Another machine writes the same plaintext: byte-identical file, created=false.
	other := openAs(t, dir, mB)
	if _, created, err := other.WriteBlob(pt); err != nil || created {
		t.Errorf("other machine WriteBlob: created=%v err=%v", created, err)
	}
	if len(other.Written()) != 0 {
		t.Errorf("other machine recorded a write it skipped: %v", other.Written())
	}

	got, err := v.ReadBlob(id)
	if err != nil || !bytes.Equal(got, pt) {
		t.Errorf("ReadBlob = %q, %v", got, err)
	}
	if !v.HasBlob(id) {
		t.Error("HasBlob = false for existing blob")
	}

	// Empty plaintext is a valid blob.
	eid, _, err := v.WriteBlob(nil)
	if err != nil {
		t.Fatalf("WriteBlob(nil): %v", err)
	}
	if e, err := v.ReadBlob(eid); err != nil || len(e) != 0 {
		t.Errorf("ReadBlob(empty) = %q, %v", e, err)
	}

	// Missing / invalid ids.
	missing := strings.Repeat("0", 64)
	if v.HasBlob(missing) {
		t.Error("HasBlob = true for missing blob")
	}
	if _, err := v.ReadBlob(missing); !errors.Is(err, ErrBlobMissing) {
		t.Errorf("ReadBlob missing: err = %v, want ErrBlobMissing", err)
	}
	for _, bad := range []string{"", "x", "../etc/passwd", "ZZ"} {
		if _, err := v.ReadBlob(bad); !errors.Is(err, ErrBlobMissing) {
			t.Errorf("ReadBlob(%q): err = %v, want ErrBlobMissing", bad, err)
		}
		if v.HasBlob(bad) {
			t.Errorf("HasBlob(%q) = true", bad)
		}
	}

	// Corrupted ciphertext → ErrBlobMissing naming the blob.
	corrupt := append([]byte(nil), first...)
	corrupt[len(corrupt)-1] ^= 0x01
	writeRaw(t, dir, rel, corrupt)
	if _, err := v.ReadBlob(id); !errors.Is(err, ErrBlobMissing) || !strings.Contains(err.Error(), rel) {
		t.Errorf("ReadBlob corrupted: err = %v, want ErrBlobMissing naming %s", err, rel)
	}
	if !v.HasBlob(id) {
		t.Error("HasBlob = false for a present (corrupt) file")
	}
	// A rewrite heals the corruption.
	if _, created, err := v.WriteBlob(pt); err != nil || !created {
		t.Errorf("WriteBlob over corrupt file: created=%v err=%v", created, err)
	}
	if got, err := v.ReadBlob(id); err != nil || !bytes.Equal(got, pt) {
		t.Errorf("ReadBlob after heal = %q, %v", got, err)
	}

	// Truncated / empty / wrong-magic files.
	for name, data := range map[string][]byte{
		"truncated": first[:len(first)/2],
		"empty":     {},
		"garbage":   []byte("not a PSV1 file at all, definitely not"),
	} {
		writeRaw(t, dir, rel, data)
		if _, err := v.ReadBlob(id); !errors.Is(err, ErrBlobMissing) {
			t.Errorf("ReadBlob %s: err = %v, want ErrBlobMissing", name, err)
		}
	}

	// A blob moved under another id fails authentication (AAD = blob id).
	otherID := "ab" + strings.Repeat("c", 62)
	writeRaw(t, dir, BlobPath(otherID), first)
	if _, err := v.ReadBlob(otherID); !errors.Is(err, ErrBlobMissing) {
		t.Errorf("ReadBlob moved blob: err = %v, want ErrBlobMissing", err)
	}
}

// ---- documents --------------------------------------------------------------

func TestSealOpenDoc(t *testing.T) {
	v, dir := newVault(t)
	rel := "projects/x/state/" + mA + DocSuffix
	ct, err := v.SealDoc(rel, []byte(`{"a":1}`))
	if err != nil {
		t.Fatalf("SealDoc: %v", err)
	}
	pt, err := v.OpenDoc(rel, ct)
	if err != nil || string(pt) != `{"a":1}` {
		t.Errorf("OpenDoc = %q, %v", pt, err)
	}
	if _, err := v.OpenDoc("projects/y/state/"+mA+DocSuffix, ct); !errors.Is(err, crypto.ErrAuth) {
		t.Errorf("OpenDoc under another path: err = %v, want ErrAuth", err)
	}
	// Non-canonical paths are refused by the vault itself (ErrBadDocPath),
	// independently of what the crypto layer checks: an empty AAD would drop
	// the move-protection binding, a backslash breaks cross-platform opens.
	for _, bad := range []string{"", `projects\x\meta\a.json.enc`, "/abs/path", "a//b", "./a", "a/../b", "a/.", "a\x00b"} {
		if _, err := v.SealDoc(bad, []byte("x")); !errors.Is(err, ErrBadDocPath) {
			t.Errorf("SealDoc(%q): err = %v, want ErrBadDocPath", bad, err)
		}
		if _, err := v.OpenDoc(bad, ct); !errors.Is(err, ErrBadDocPath) {
			t.Errorf("OpenDoc(%q): err = %v, want ErrBadDocPath", bad, err)
		}
	}
	if _, err := v.SealDoc("a.b", []byte("x")); err != nil {
		t.Errorf("SealDoc single element: %v", err)
	}
	// Random nonce: sealing twice gives different bytes.
	ct2, _ := v.SealDoc(rel, []byte(`{"a":1}`))
	if bytes.Equal(ct, ct2) {
		t.Error("documents are deterministic; they must use a random nonce")
	}
	// Another handle on the same vault can open it.
	other := openAs(t, dir, mB)
	if pt, err := other.OpenDoc(rel, ct); err != nil || string(pt) != `{"a":1}` {
		t.Errorf("OpenDoc from other handle = %q, %v", pt, err)
	}
}

// ---- machine files ----------------------------------------------------------

func TestIsMachineFile(t *testing.T) {
	tests := []struct {
		name   string
		wantID string
		ok     bool
	}{
		{mA + ".json.enc", mA, true},
		{"123e4567-e89b-12d3-a456-426614174000.json.enc", "123e4567-e89b-12d3-a456-426614174000", true}, // v1 shape accepted
		{"00000000-0000-0000-0000-000000000000.json.enc", "00000000-0000-0000-0000-000000000000", true},
		{strings.ToUpper(mA) + ".json.enc", "", false},
		{mA + " (1).json.enc", "", false},
		{mA + ".json (1).enc", "", false},
		{mA + " (conflicted copy 2026-09-05).json.enc", "", false},
		{mA + ".json.enc (1)", "", false},
		{mA + ".json.enc.bak", "", false},
		{mA + ".json", "", false},
		{mA + ".enc", "", false},
		{mA, "", false},
		{".DS_Store", "", false},
		{"", "", false},
		{"x" + mA + ".json.enc", "", false},
		{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaa.json.enc", "", false},   // 11 in last group
		{"aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaaa.json.enc", "", false}, // 13 in last group
		{"aaaaaaaaaaaa4aaa8aaaaaaaaaaaaaaa.json.enc", "", false},      // no dashes
		{mA + ".JSON.ENC", "", false},
		{mA + "\n.json.enc", "", false},
		{".psv-tmp-aaaaaaaa", "", false},
	}
	for _, tc := range tests {
		id, ok := IsMachineFile(tc.name)
		if ok != tc.ok || id != tc.wantID {
			t.Errorf("IsMachineFile(%q) = %q, %v; want %q, %v", tc.name, id, ok, tc.wantID, tc.ok)
		}
	}
	if !ValidMachineID(mA) || ValidMachineID("m1") || ValidMachineID("") {
		t.Error("ValidMachineID mismatch")
	}
}

// ---- projects ---------------------------------------------------------------

func TestProjectMetaMerge(t *testing.T) {
	v, dir := newVault(t)
	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := old.Add(48 * time.Hour)

	metaB := ProjectMeta{
		Name:         "renamed-on-b",
		Fingerprints: []identity.Fingerprint{fp("git", "github.com/x/y", identity.LevelStrong), fp("npm", "y", identity.LevelPackage)},
		CreatedAt:    old,
	}
	metaA := ProjectMeta{
		Name:         "original-on-a",
		Fingerprints: []identity.Fingerprint{fp("dir", "y", identity.LevelDir), fp("git", "github.com/x/y", identity.LevelStrong)},
		CreatedAt:    newer,
	}
	if err := v.WriteProjectMeta(projA, mA, metaA); err != nil {
		t.Fatalf("WriteProjectMeta A: %v", err)
	}
	// A machine only writes its own meta: the mA handle refuses mB's file and
	// leaves nothing behind; mB's own handle writes it.
	if err := v.WriteProjectMeta(projA, mB, metaB); !errors.Is(err, ErrForeignMachine) {
		t.Fatalf("WriteProjectMeta for mB through mA's handle: err = %v, want ErrForeignMachine", err)
	}
	if fsutil.Exists(filepath.Join(dir, "projects", projA, "meta", mB+DocSuffix)) {
		t.Fatal("foreign meta was written despite ErrForeignMachine")
	}
	vb := openAs(t, dir, mB)
	if err := vb.WriteProjectMeta(projA, mB, metaB); err != nil {
		t.Fatalf("WriteProjectMeta B: %v", err)
	}
	relA := "projects/" + projA + "/meta/" + mA + DocSuffix
	if !fsutil.Exists(filepath.Join(dir, filepath.FromSlash(relA))) {
		t.Fatalf("meta file %s missing", relA)
	}

	p, warnings, err := v.ReadProject(projA)
	if err != nil {
		t.Fatalf("ReadProject: %v", err)
	}
	if len(warnings) != 0 {
		t.Errorf("unexpected warnings: %v", warnings)
	}
	if p.ID != projA {
		t.Errorf("ID = %q", p.ID)
	}
	if p.Name != "renamed-on-b" {
		t.Errorf("Name = %q, want the oldest meta's name", p.Name)
	}
	if !p.CreatedAt.Equal(old) {
		t.Errorf("CreatedAt = %v, want %v", p.CreatedAt, old)
	}
	if !reflect.DeepEqual(p.Machines, []string{mA, mB}) {
		t.Errorf("Machines = %v", p.Machines)
	}
	// Union in machine-id order: A's list first, then B's new ones.
	wantFP := []identity.Fingerprint{
		fp("dir", "y", identity.LevelDir),
		fp("git", "github.com/x/y", identity.LevelStrong),
		fp("npm", "y", identity.LevelPackage),
	}
	if !reflect.DeepEqual(p.Fingerprints, wantFP) {
		t.Errorf("Fingerprints = %v, want %v", p.Fingerprints, wantFP)
	}

	// Round trip of one machine's meta; absent → nil, nil.
	got, err := v.ReadProjectMeta(projA, mB)
	if err != nil || got == nil || got.Name != metaB.Name || !got.CreatedAt.Equal(old) || !reflect.DeepEqual(got.Fingerprints, metaB.Fingerprints) {
		t.Errorf("ReadProjectMeta B = %+v, %v", got, err)
	}
	if got, err := v.ReadProjectMeta(projA, mC); err != nil || got != nil {
		t.Errorf("ReadProjectMeta absent = %+v, %v; want nil, nil", got, err)
	}

	// Tie on CreatedAt: lowest machine id wins.
	tieC := ProjectMeta{Name: "tie-c", CreatedAt: old}
	if err := openAs(t, dir, mC).WriteProjectMeta(projA, mC, tieC); err != nil {
		t.Fatal(err)
	}
	p, _, err = v.ReadProject(projA)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "renamed-on-b" {
		t.Errorf("tie: Name = %q, want B's (lowest id among oldest)", p.Name)
	}
	if !reflect.DeepEqual(p.Machines, []string{mA, mB, mC}) {
		t.Errorf("Machines = %v", p.Machines)
	}

	// ListProjects with a second project, junk files and an unreadable meta.
	if err := v.WriteProjectMeta(projB, mA, ProjectMeta{Name: "second", CreatedAt: newer}); err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, "projects/"+projA+"/meta/.DS_Store", []byte("junk"))
	writeRaw(t, dir, "projects/"+projA+"/meta/"+mA+" (1).json.enc", []byte("drive copy"))
	writeRaw(t, dir, "projects/"+projB+"/meta/"+mB+DocSuffix, []byte("PSV1 not really encrypted"))
	// Our own in-flight temp file (suffix = first 8 chars of the writer id) is
	// silently skipped; a temp file left by another writer is a stray name
	// nobody will clean up, so it is reported like any other junk (spec §5).
	ownTemp := mC + ".json.enc" + fsutil.TempPrefix + mA[:8]
	foreignTemp := mC + ".json.enc" + fsutil.TempPrefix + "deadbeef"
	writeRaw(t, dir, "projects/"+projB+"/meta/"+ownTemp, []byte("in flight"))
	writeRaw(t, dir, "projects/"+projB+"/meta/"+foreignTemp, []byte("stale"))
	writeRaw(t, dir, "projects/stray.txt", []byte("junk"))
	writeRaw(t, dir, "projects/stray.txt"+fsutil.TempPrefix+mA[:8], []byte("own temp at projects/ level"))
	writeRaw(t, dir, "projects/stray.txt"+fsutil.TempPrefix+"deadbeef", []byte("foreign temp at projects/ level"))
	if err := os.MkdirAll(filepath.Join(dir, "projects", "emptyproject", "meta"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "projects", "nometa", "state"), 0o700); err != nil {
		t.Fatal(err)
	}

	projects, warnings, err := v.ListProjects()
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	var ids []string
	for _, p := range projects {
		ids = append(ids, p.ID)
	}
	if !reflect.DeepEqual(ids, []string{projA, projB}) {
		t.Errorf("ListProjects ids = %v, want [%s %s]", ids, projA, projB)
	}
	if projects[0].Name != "renamed-on-b" || projects[1].Name != "second" {
		t.Errorf("ListProjects names = %q, %q", projects[0].Name, projects[1].Name)
	}
	if !reflect.DeepEqual(projects[1].Machines, []string{mA}) {
		t.Errorf("projB machines = %v (unreadable meta must not count)", projects[1].Machines)
	}
	for _, want := range []string{".DS_Store", mA + " (1).json.enc", mB + DocSuffix, "stray.txt", "emptyproject", "nometa"} {
		if !hasWarning(warnings, want) {
			t.Errorf("no warning naming %q in %v", want, warnings)
		}
	}
	if hasWarning(warnings, ownTemp) || hasWarning(warnings, fsutil.TempPrefix+mA[:8]) {
		t.Errorf("own in-flight temp file produced a warning: %v", warnings)
	}
	if !hasWarning(warnings, foreignTemp) || !hasWarning(warnings, "stray.txt"+fsutil.TempPrefix+"deadbeef") {
		t.Errorf("another writer's temp file was not reported: %v", warnings)
	}

	// ReadProject errors.
	if _, _, err := v.ReadProject("doesnotexist"); !errors.Is(err, ErrNoProject) {
		t.Errorf("ReadProject missing: err = %v, want ErrNoProject", err)
	}
	if _, _, err := v.ReadProject("emptyproject"); !errors.Is(err, ErrNoProject) {
		t.Errorf("ReadProject without meta: err = %v, want ErrNoProject", err)
	}
	if _, _, err := v.ReadProject("../"); !errors.Is(err, ErrNoProject) {
		t.Errorf("ReadProject bad id: err = %v, want ErrNoProject", err)
	}
	if err := v.WriteProjectMeta("a/b", mA, ProjectMeta{}); !errors.Is(err, ErrBadProjectID) {
		t.Errorf("WriteProjectMeta bad project id: %v", err)
	}
	if err := v.WriteProjectMeta(projA, "m1", ProjectMeta{}); !errors.Is(err, ErrBadMachineID) {
		t.Errorf("WriteProjectMeta bad machine id: %v", err)
	}

	// No projects dir at all → empty, no error.
	v2, _ := newVault(t)
	projects, warnings, err = v2.ListProjects()
	if err != nil || len(projects) != 0 || len(warnings) != 0 {
		t.Errorf("ListProjects on empty vault = %v, %v, %v", projects, warnings, err)
	}
}

func TestProjectMetaZeroCreatedAt(t *testing.T) {
	v, dir := newVault(t)
	known := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	if err := v.WriteProjectMeta(projA, mA, ProjectMeta{Name: "unknown-age"}); err != nil {
		t.Fatal(err)
	}
	if err := openAs(t, dir, mB).WriteProjectMeta(projA, mB, ProjectMeta{Name: "dated", CreatedAt: known}); err != nil {
		t.Fatal(err)
	}
	p, _, err := v.ReadProject(projA)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "dated" || !p.CreatedAt.Equal(known) {
		t.Errorf("zero CreatedAt must not win: %+v", p)
	}
	if p.Fingerprints == nil {
		t.Error("Fingerprints is nil, want empty slice")
	}
}

// ---- journals ---------------------------------------------------------------

func entry(path, blob string, clock Clock, parents ...string) Entry {
	return Entry{Path: path, Kind: KindFile, Blob: blob, Clock: clock, Parents: parents, Machine: mA, UpdatedAt: time.Now().UTC()}
}

func TestJournals(t *testing.T) {
	v, dir := newVault(t)

	// Absent → nil, nil; missing state dir → empty map.
	if j, err := v.ReadJournal(projA, mA); err != nil || j != nil {
		t.Errorf("ReadJournal absent = %+v, %v", j, err)
	}
	js, warnings, err := v.ReadJournals(projA)
	if err != nil || len(js) != 0 || len(warnings) != 0 || js == nil {
		t.Errorf("ReadJournals absent = %v, %v, %v", js, warnings, err)
	}

	j := &Journal{Machine: mA, Entries: map[string]Entry{
		".env": entry(".env", "ab12", Clock{mA: 1}),
	}}
	before := time.Now().Add(-time.Second)
	if err := v.WriteJournal(projA, j); err != nil {
		t.Fatalf("WriteJournal: %v", err)
	}
	if j.Seq != 1 {
		t.Errorf("Seq after first write = %d, want 1", j.Seq)
	}
	if j.UpdatedAt.Before(before) {
		t.Errorf("UpdatedAt not set: %v", j.UpdatedAt)
	}
	rel := "projects/" + projA + "/state/" + mA + DocSuffix
	if got := v.Written(); !reflect.DeepEqual(got, []string{VaultFileName, rel}) {
		t.Errorf("Written() = %v", got)
	}
	if err := v.WriteJournal(projA, j); err != nil {
		t.Fatal(err)
	}
	if j.Seq != 2 {
		t.Errorf("Seq after second write = %d, want 2", j.Seq)
	}

	got, err := v.ReadJournal(projA, mA)
	if err != nil {
		t.Fatalf("ReadJournal: %v", err)
	}
	if got.Machine != mA || got.Seq != 2 || !got.UpdatedAt.Equal(j.UpdatedAt) {
		t.Errorf("ReadJournal = %+v", got)
	}
	e := got.Entries[".env"]
	if e.Path != ".env" || e.Blob != "ab12" || e.Clock[mA] != 1 || e.Kind != KindFile {
		t.Errorf("entry = %+v", e)
	}

	// Nil entries are written as an empty map.
	empty := &Journal{Machine: mA}
	if err := v.WriteJournal(projB, empty); err != nil {
		t.Fatal(err)
	}
	if got, err := v.ReadJournal(projB, mA); err != nil || got.Entries == nil || len(got.Entries) != 0 {
		t.Errorf("empty journal = %+v, %v", got, err)
	}

	// A second machine writes its own journal through its own handle.
	vb := openAs(t, dir, mB)
	jb := &Journal{Machine: mB, Entries: map[string]Entry{"b.txt": entry("b.txt", "cd34", Clock{mB: 1})}}
	if err := vb.WriteJournal(projA, jb); err != nil {
		t.Fatalf("WriteJournal B: %v", err)
	}
	js, warnings, err = v.ReadJournals(projA)
	if err != nil {
		t.Fatalf("ReadJournals: %v", err)
	}
	if len(js) != 2 || js[mA] == nil || js[mB] == nil || js[mA].Seq != 2 || js[mB].Seq != 1 {
		t.Errorf("ReadJournals = %v", js)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v", warnings)
	}

	// Own-journal rule and validation.
	if err := v.WriteJournal(projA, &Journal{Machine: mB}); !errors.Is(err, ErrForeignMachine) {
		t.Errorf("WriteJournal foreign: err = %v, want ErrForeignMachine", err)
	}
	if err := v.WriteJournal(projA, &Journal{Machine: "laptop"}); !errors.Is(err, ErrBadMachineID) {
		t.Errorf("WriteJournal bad machine: err = %v, want ErrBadMachineID", err)
	}
	if err := v.WriteJournal("../x", &Journal{Machine: mA}); !errors.Is(err, ErrBadProjectID) {
		t.Errorf("WriteJournal bad project: err = %v, want ErrBadProjectID", err)
	}
	if err := v.WriteJournal(projA, nil); err == nil {
		t.Error("WriteJournal(nil) succeeded")
	}
	if _, err := v.ReadJournal(projA, "nope"); !errors.Is(err, ErrBadMachineID) {
		t.Errorf("ReadJournal bad machine: %v", err)
	}
	if _, _, err := v.ReadJournals(""); !errors.Is(err, ErrBadProjectID) {
		t.Errorf("ReadJournals bad project: %v", err)
	}

	// Junk in state/ → warning naming it, still readable. Our own in-flight
	// temp file is silent; a stale temp file of another writer is reported.
	writeRaw(t, dir, "projects/"+projA+"/state/"+mA+" (1).json.enc", []byte("drive copy"))
	writeRaw(t, dir, "projects/"+projA+"/state/.DS_Store", []byte("junk"))
	writeRaw(t, dir, "projects/"+projA+"/state/"+mA+DocSuffix+fsutil.TempPrefix+mA[:8], []byte("own"))
	writeRaw(t, dir, "projects/"+projA+"/state/"+mC+DocSuffix+fsutil.TempPrefix+"deadbeef", []byte("foreign"))
	js, warnings, err = v.ReadJournals(projA)
	if err != nil || len(js) != 2 {
		t.Fatalf("ReadJournals with junk = %v, %v", js, err)
	}
	if !hasWarning(warnings, mA+" (1).json.enc") || !hasWarning(warnings, ".DS_Store") {
		t.Errorf("warnings = %v", warnings)
	}
	if hasWarning(warnings, fsutil.TempPrefix+mA[:8]) {
		t.Errorf("own temp file reported: %v", warnings)
	}
	if !hasWarning(warnings, mC+DocSuffix+fsutil.TempPrefix+"deadbeef") {
		t.Errorf("foreign temp file not reported: %v", warnings)
	}

	// An unreadable journal makes the whole project unreadable.
	relB := "projects/" + projA + "/state/" + mB + DocSuffix
	ct, err := os.ReadFile(filepath.Join(dir, filepath.FromSlash(relB)))
	if err != nil {
		t.Fatal(err)
	}
	corrupt := append([]byte(nil), ct...)
	corrupt[len(corrupt)-3] ^= 0x80
	writeRaw(t, dir, relB, corrupt)
	if _, _, err := v.ReadJournals(projA); !errors.Is(err, ErrUnreadableJournal) || !strings.Contains(err.Error(), relB) {
		t.Errorf("ReadJournals corrupt: err = %v, want ErrUnreadableJournal naming %s", err, relB)
	}
	if _, err := v.ReadJournal(projA, mB); !errors.Is(err, ErrUnreadableJournal) {
		t.Errorf("ReadJournal corrupt: err = %v, want ErrUnreadableJournal", err)
	}

	// Valid ciphertext but not JSON.
	notJSON, err := v.SealDoc(relB, []byte("not json"))
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, relB, notJSON)
	if _, _, err := v.ReadJournals(projA); !errors.Is(err, ErrUnreadableJournal) {
		t.Errorf("ReadJournals non-json: err = %v, want ErrUnreadableJournal", err)
	}

	// A journal moved to another machine's file name fails (AAD binds the path).
	writeRaw(t, dir, relB, ct[:0]) // shrink to nothing first
	writeRaw(t, dir, relB, mustRead(t, filepath.Join(dir, filepath.FromSlash(rel))))
	if _, _, err := v.ReadJournals(projA); !errors.Is(err, ErrUnreadableJournal) {
		t.Errorf("ReadJournals moved journal: err = %v, want ErrUnreadableJournal", err)
	}

	// A journal claiming a different machine than its file name is corrupt.
	claim, err := v.SealDoc(relB, []byte(`{"machine":"`+mC+`","seq":1,"entries":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, relB, claim)
	if _, _, err := v.ReadJournals(projA); !errors.Is(err, ErrUnreadableJournal) {
		t.Errorf("ReadJournals wrong machine: err = %v, want ErrUnreadableJournal", err)
	}

	// An empty machine field is tolerated (filled from the file name).
	noMachine, err := v.SealDoc(relB, []byte(`{"seq":7,"entries":{"x":{"kind":0,"blob":"ff"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, relB, noMachine)
	js, _, err = v.ReadJournals(projA)
	if err != nil || js[mB] == nil || js[mB].Machine != mB || js[mB].Seq != 7 || js[mB].Entries["x"].Path != "x" {
		t.Errorf("ReadJournals tolerant = %v, %v", js, err)
	}
}

func mustRead(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestWriteJournalWithoutWriterID(t *testing.T) {
	_, dir := newVault(t)
	v, err := Open(dir, pass(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	if err := v.WriteJournal(projA, &Journal{Machine: mB}); err != nil {
		t.Errorf("handle without writer id must not enforce ownership: %v", err)
	}
}

// ---- machines ---------------------------------------------------------------

func TestMachines(t *testing.T) {
	v, dir := newVault(t)
	ms, warnings, err := v.ListMachines()
	if err != nil || len(ms) != 0 || len(warnings) != 0 {
		t.Errorf("ListMachines empty = %v, %v, %v", ms, warnings, err)
	}
	seen := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	// A machine only writes its own info file.
	if err := v.WriteMachine(MachineInfo{ID: mB, Name: "laptop"}); !errors.Is(err, ErrForeignMachine) {
		t.Fatalf("WriteMachine for mB through mA's handle: err = %v, want ErrForeignMachine", err)
	}
	if fsutil.Exists(filepath.Join(dir, MachinesDir, mB+DocSuffix)) {
		t.Fatal("foreign machine info was written despite ErrForeignMachine")
	}
	vb := openAs(t, dir, mB)
	if err := vb.WriteMachine(MachineInfo{ID: mB, Name: "laptop", Hostname: "lap.local", LastSeen: seen}); err != nil {
		t.Fatalf("WriteMachine: %v", err)
	}
	if err := v.WriteMachine(MachineInfo{ID: mA, Name: "desk"}); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteMachine(MachineInfo{ID: "desk"}); !errors.Is(err, ErrBadMachineID) {
		t.Errorf("WriteMachine bad id: %v", err)
	}
	writeRaw(t, dir, "machines/.DS_Store", []byte("junk"))
	writeRaw(t, dir, "machines/"+mC+DocSuffix, []byte("PSV1garbage"))

	ms, warnings, err = v.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(ms) != 2 || ms[0].ID != mA || ms[1].ID != mB {
		t.Errorf("ListMachines = %+v", ms)
	}
	if ms[1].Name != "laptop" || ms[1].Hostname != "lap.local" || !ms[1].LastSeen.Equal(seen) {
		t.Errorf("machine B = %+v", ms[1])
	}
	if !hasWarning(warnings, ".DS_Store") || !hasWarning(warnings, mC+DocSuffix) {
		t.Errorf("warnings = %v", warnings)
	}
	if got := v.Written(); !reflect.DeepEqual(got, []string{VaultFileName, "machines/" + mA + DocSuffix}) {
		t.Errorf("A.Written() = %v", got)
	}
	if got := vb.Written(); !reflect.DeepEqual(got, []string{"machines/" + mB + DocSuffix}) {
		t.Errorf("B.Written() = %v", got)
	}

	// The unrestricted (tooling) handle may write any machine's file.
	tool, err := Open(dir, pass(), "")
	if err != nil {
		t.Fatal(err)
	}
	defer tool.Close()
	if err := tool.WriteMachine(MachineInfo{ID: mA, Name: "renamed by tool"}); err != nil {
		t.Errorf("tooling handle WriteMachine: %v", err)
	}
	if err := tool.WriteProjectMeta(projA, mB, ProjectMeta{Name: "by tool"}); err != nil {
		t.Errorf("tooling handle WriteProjectMeta: %v", err)
	}
}

// ---- Written ----------------------------------------------------------------

func TestWrittenOrderAndDedupe(t *testing.T) {
	v, _ := newVault(t)
	b1, _, _ := v.WriteBlob([]byte("one"))
	if err := v.WriteJournal(projA, &Journal{Machine: mA}); err != nil {
		t.Fatal(err)
	}
	b2, _, _ := v.WriteBlob([]byte("two"))
	if err := v.WriteJournal(projA, &Journal{Machine: mA, Seq: 1}); err != nil { // same path again
		t.Fatal(err)
	}
	if _, _, err := v.WriteBlob([]byte("one")); err != nil { // dedup, no write
		t.Fatal(err)
	}
	if err := v.WriteProjectMeta(projA, mA, ProjectMeta{Name: "p"}); err != nil {
		t.Fatal(err)
	}
	if err := v.WriteMachine(MachineInfo{ID: mA}); err != nil {
		t.Fatal(err)
	}
	if err := v.Rekey([]byte("n"), crypto.KDFParams{}); err != nil {
		t.Fatal(err)
	}
	want := []string{
		VaultFileName,
		BlobPath(b1),
		"projects/" + projA + "/state/" + mA + DocSuffix,
		BlobPath(b2),
		"projects/" + projA + "/meta/" + mA + DocSuffix,
		"machines/" + mA + DocSuffix,
	}
	got := v.Written()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Written() = %v\nwant %v", got, want)
	}
	for _, p := range got {
		if strings.Contains(p, `\`) || strings.HasPrefix(p, "/") {
			t.Errorf("Written() path %q is not slash-separated vault-relative", p)
		}
	}
	// The returned slice is a copy.
	got[0] = "mutated"
	if v.Written()[0] != VaultFileName {
		t.Error("Written() returned internal slice")
	}
}

// ---- clocks -----------------------------------------------------------------

func TestClockCompare(t *testing.T) {
	tests := []struct {
		name string
		a, b Clock
		want Ordering
	}{
		{"both nil", nil, nil, Equal},
		{"nil vs empty", nil, Clock{}, Equal},
		{"equal", Clock{"a": 1, "b": 2}, Clock{"a": 1, "b": 2}, Equal},
		{"equal with explicit zero", Clock{"a": 1}, Clock{"a": 1, "b": 0}, Equal},
		{"before", Clock{"a": 1}, Clock{"a": 3}, Before},
		{"after", Clock{"a": 3}, Clock{"a": 1}, After},
		{"before missing component", Clock{"a": 1}, Clock{"a": 1, "b": 1}, Before},
		{"after missing component", Clock{"a": 1, "b": 1}, Clock{"a": 1}, After},
		{"nil before", nil, Clock{"a": 1}, Before},
		{"after nil", Clock{"a": 1}, nil, After},
		{"concurrent", Clock{"a": 1, "b": 1}, Clock{"a": 3}, Concurrent},
		{"concurrent disjoint", Clock{"a": 1}, Clock{"b": 1}, Concurrent},
		{"concurrent mixed", Clock{"a": 2, "b": 1}, Clock{"a": 1, "b": 2}, Concurrent},
		{"before all components", Clock{"a": 1, "b": 1}, Clock{"a": 2, "b": 2}, Before},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Compare(tc.b); got != tc.want {
				t.Errorf("%v.Compare(%v) = %v, want %v", tc.a, tc.b, got, tc.want)
			}
			// Symmetry.
			var mirror Ordering
			switch tc.want {
			case Before:
				mirror = After
			case After:
				mirror = Before
			default:
				mirror = tc.want
			}
			if got := tc.b.Compare(tc.a); got != mirror {
				t.Errorf("%v.Compare(%v) = %v, want %v", tc.b, tc.a, got, mirror)
			}
		})
	}
	for _, o := range []Ordering{Equal, Before, After, Concurrent, Ordering(9)} {
		if o.String() == "" {
			t.Errorf("Ordering(%d).String() is empty", o)
		}
	}
}

func TestClockMergeTickCopy(t *testing.T) {
	a := Clock{"a": 2, "b": 1}
	b := Clock{"b": 3, "c": 1}
	m := a.Merge(b)
	if want := (Clock{"a": 2, "b": 3, "c": 1}); !reflect.DeepEqual(m, want) {
		t.Errorf("Merge = %v, want %v", m, want)
	}
	if a["b"] != 1 || len(b) != 2 {
		t.Error("Merge mutated its inputs")
	}
	if got := Clock(nil).Merge(nil); got == nil || len(got) != 0 {
		t.Errorf("nil.Merge(nil) = %v", got)
	}

	tk := a.Tick("a")
	if tk["a"] != 3 || tk["b"] != 1 || a["a"] != 2 {
		t.Errorf("Tick = %v (orig %v)", tk, a)
	}
	tk2 := Clock(nil).Tick("z")
	if tk2["z"] != 1 || len(tk2) != 1 {
		t.Errorf("nil.Tick = %v", tk2)
	}
	if tk.Compare(a) != After {
		t.Error("ticked clock is not After the original")
	}

	c := a.Copy()
	c["a"] = 99
	if a["a"] != 2 {
		t.Error("Copy shares storage")
	}
	if got := Clock(nil).Copy(); got == nil {
		t.Error("nil.Copy() = nil")
	}
	for _, k := range []Kind{KindFile, KindDeleted, KindUntracked, Kind(7)} {
		if k.String() == "" {
			t.Errorf("Kind(%d).String() is empty", k)
		}
	}
}

// ---- head resolution --------------------------------------------------------

type he struct {
	machine string
	kind    Kind
	blob    string
	clock   Clock
	parents []string
	updated time.Time
}

func journalsOf(path string, entries ...he) map[string]*Journal {
	out := map[string]*Journal{}
	for i, e := range entries {
		updated := e.updated
		if updated.IsZero() {
			updated = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		}
		out[e.machine] = &Journal{Machine: e.machine, Entries: map[string]Entry{
			path: {Path: path, Kind: e.kind, Blob: e.blob, Clock: e.clock, Parents: e.parents, Machine: e.machine, UpdatedAt: updated},
		}}
	}
	return out
}

func candidateMachines(h Head) []string {
	var out []string
	for _, c := range h.Candidates {
		out = append(out, c.Machine)
	}
	return out
}

func TestResolveHeads(t *testing.T) {
	t1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)

	tests := []struct {
		name        string
		entries     []he
		wantEntry   bool
		wantMachine string   // Entry.Machine when wantEntry
		wantKind    Kind     // Entry.Kind when wantEntry
		wantBlob    string   // Entry.Blob when wantEntry
		wantCands   []string // machines of Candidates, in order
		wantBase    string
	}{
		{
			name:        "single machine",
			entries:     []he{{mA, KindFile, "x1", Clock{mA: 1}, nil, t1}},
			wantEntry:   true,
			wantMachine: mA, wantKind: KindFile, wantBlob: "x1",
			wantCands: []string{mA},
		},
		{
			name: "stale entry dominated",
			entries: []he{
				{mA, KindFile, "x3", Clock{mA: 3}, []string{"x2"}, t1},
				{mB, KindFile, "x1", Clock{mA: 1}, nil, t2}, // newer UpdatedAt but older clock
			},
			wantEntry:   true,
			wantMachine: mA, wantKind: KindFile, wantBlob: "x3",
			wantCands: []string{mA},
		},
		{
			name: "dominated through a chain",
			entries: []he{
				{mA, KindFile, "x1", Clock{mA: 1}, nil, t1},
				{mB, KindFile, "x2", Clock{mA: 1, mB: 1}, []string{"x1"}, t1},
				{mC, KindFile, "x3", Clock{mA: 1, mB: 1, mC: 1}, []string{"x2"}, t1},
			},
			wantEntry:   true,
			wantMachine: mC, wantKind: KindFile, wantBlob: "x3",
			wantCands: []string{mC},
		},
		{
			name: "concurrent edits with shared parent",
			entries: []he{
				{mB, KindFile, "b1", Clock{mA: 1, mB: 1}, []string{"base"}, t1},
				{mA, KindFile, "a2", Clock{mA: 2}, []string{"base"}, t1},
			},
			wantEntry: false,
			wantCands: []string{mA, mB},
			wantBase:  "base",
		},
		{
			name: "concurrent with several parents picks first common in candidate order",
			entries: []he{
				{mA, KindFile, "a2", Clock{mA: 2}, []string{"p1", "shared"}, t1},
				{mB, KindFile, "b1", Clock{mB: 1}, []string{"shared", "p2"}, t1},
			},
			wantEntry: false,
			wantCands: []string{mA, mB},
			wantBase:  "shared",
		},
		{
			name: "concurrent without shared parent",
			entries: []he{
				{mA, KindFile, "a1", Clock{mA: 1}, []string{"pa"}, t1},
				{mB, KindFile, "b1", Clock{mB: 1}, []string{"pb"}, t1},
			},
			wantEntry: false,
			wantCands: []string{mA, mB},
			wantBase:  "",
		},
		{
			name: "concurrent where one side has no parents",
			entries: []he{
				{mA, KindFile, "a1", Clock{mA: 1}, nil, t1},
				{mB, KindFile, "b1", Clock{mB: 1}, []string{"base"}, t1},
			},
			wantEntry: false,
			wantCands: []string{mA, mB},
			wantBase:  "",
		},
		{
			name: "empty parent ids are ignored",
			entries: []he{
				{mA, KindFile, "a1", Clock{mA: 1}, []string{"", "base"}, t1},
				{mB, KindFile, "b1", Clock{mB: 1}, []string{"base", ""}, t1},
			},
			wantEntry: false,
			wantCands: []string{mA, mB},
			wantBase:  "base",
		},
		{
			name: "untracked vs file concurrent: untracked wins",
			entries: []he{
				{mA, KindFile, "a1", Clock{mA: 1}, nil, t2},
				{mB, KindUntracked, "", Clock{mB: 1}, nil, t1},
			},
			wantEntry:   true,
			wantMachine: mB, wantKind: KindUntracked, wantBlob: "",
			wantCands: []string{mA, mB},
		},
		{
			name: "untracked vs deleted: untracked wins",
			entries: []he{
				{mA, KindDeleted, "", Clock{mA: 1}, nil, t1},
				{mB, KindUntracked, "", Clock{mB: 1}, nil, t1},
			},
			wantEntry:   true,
			wantMachine: mB, wantKind: KindUntracked,
			wantCands: []string{mA, mB},
		},
		{
			name: "untracked dominated by a later file edit",
			entries: []he{
				{mA, KindUntracked, "", Clock{mA: 1}, nil, t1},
				{mB, KindFile, "b2", Clock{mA: 1, mB: 1}, nil, t1},
			},
			wantEntry:   true,
			wantMachine: mB, wantKind: KindFile, wantBlob: "b2",
			wantCands: []string{mB},
		},
		{
			name: "deleted vs deleted concurrent",
			entries: []he{
				{mA, KindDeleted, "", Clock{mA: 2}, nil, t1},
				{mB, KindDeleted, "", Clock{mB: 2}, nil, t1},
			},
			wantEntry: true,
			wantKind:  KindDeleted,
			wantCands: []string{mA, mB},
		},
		{
			name: "deleted vs file concurrent",
			entries: []he{
				{mA, KindDeleted, "", Clock{mA: 2}, []string{"old"}, t1},
				{mB, KindFile, "b1", Clock{mA: 1, mB: 1}, []string{"old"}, t1},
			},
			wantEntry: false,
			wantCands: []string{mA, mB},
			wantBase:  "old",
		},
		{
			name: "equal clocks same content: single head",
			entries: []he{
				{mA, KindFile, "same", Clock{mA: 1, mB: 1}, nil, t1},
				{mB, KindFile, "same", Clock{mA: 1, mB: 1}, nil, t2},
			},
			wantEntry:   true,
			wantMachine: mA, wantKind: KindFile, wantBlob: "same",
			wantCands: []string{mA},
		},
		{
			name: "equal clocks different content: concurrent",
			entries: []he{
				{mA, KindFile, "x", Clock{mA: 1}, []string{"base"}, t1},
				{mB, KindFile, "y", Clock{mA: 1}, []string{"base"}, t1},
			},
			wantEntry: false,
			wantCands: []string{mA, mB},
			wantBase:  "base",
		},
		{
			name: "same content different clocks: latest UpdatedAt wins",
			entries: []he{
				{mA, KindFile, "same", Clock{mA: 1}, nil, t2},
				{mB, KindFile, "same", Clock{mB: 1}, nil, t1},
			},
			wantEntry:   true,
			wantMachine: mA, wantKind: KindFile, wantBlob: "same",
			wantCands: []string{mA, mB},
		},
		{
			name: "revert to earlier content still resolves by clocks",
			entries: []he{
				// A wrote x1 {A:1}; B edited to y {A:1,B:1}; A reverted to x1 {A:2,B:1}.
				{mA, KindFile, "x1", Clock{mA: 2, mB: 1}, []string{"y"}, t1},
				{mB, KindFile, "y", Clock{mA: 1, mB: 1}, []string{"x1"}, t2},
			},
			wantEntry:   true,
			wantMachine: mA, wantKind: KindFile, wantBlob: "x1",
			wantCands: []string{mA},
		},
		{
			name: "three way concurrent",
			entries: []he{
				{mC, KindFile, "c", Clock{mC: 1}, []string{"base"}, t1},
				{mA, KindFile, "a", Clock{mA: 1}, []string{"base"}, t1},
				{mB, KindFile, "b", Clock{mB: 1}, []string{"base"}, t1},
			},
			wantEntry: false,
			wantCands: []string{mA, mB, mC},
			wantBase:  "base",
		},
		{
			name: "three way with one shared only by two",
			entries: []he{
				{mA, KindFile, "a", Clock{mA: 1}, []string{"base"}, t1},
				{mB, KindFile, "b", Clock{mB: 1}, []string{"base"}, t1},
				{mC, KindFile, "c", Clock{mC: 1}, []string{"other"}, t1},
			},
			wantEntry: false,
			wantCands: []string{mA, mB, mC},
			wantBase:  "",
		},
		{
			name: "nil clocks on both sides are equal",
			entries: []he{
				{mA, KindFile, "same", nil, nil, t1},
				{mB, KindFile, "same", nil, nil, t1},
			},
			wantEntry:   true,
			wantMachine: mA, wantKind: KindFile, wantBlob: "same",
			wantCands: []string{mA},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			heads := ResolveHeads(journalsOf("dir/file.txt", tc.entries...))
			h, ok := heads["dir/file.txt"]
			if !ok || len(heads) != 1 {
				t.Fatalf("heads = %v", heads)
			}
			if h.Path != "dir/file.txt" {
				t.Errorf("Path = %q", h.Path)
			}
			if got := candidateMachines(h); !reflect.DeepEqual(got, tc.wantCands) {
				t.Errorf("Candidates = %v, want %v", got, tc.wantCands)
			}
			if len(h.Candidates) < 1 {
				t.Error("Candidates is empty")
			}
			if (h.Entry != nil) != tc.wantEntry {
				t.Fatalf("Entry = %+v, wantEntry=%v", h.Entry, tc.wantEntry)
			}
			if h.Concurrent() == tc.wantEntry {
				t.Errorf("Concurrent() = %v", h.Concurrent())
			}
			if tc.wantEntry {
				if tc.wantMachine != "" && h.Entry.Machine != tc.wantMachine {
					t.Errorf("Entry.Machine = %s, want %s", h.Entry.Machine, tc.wantMachine)
				}
				if h.Entry.Kind != tc.wantKind || h.Entry.Blob != tc.wantBlob {
					t.Errorf("Entry = (%v, %q), want (%v, %q)", h.Entry.Kind, h.Entry.Blob, tc.wantKind, tc.wantBlob)
				}
				if h.Entry.Path != "dir/file.txt" {
					t.Errorf("Entry.Path = %q", h.Entry.Path)
				}
			}
			if h.Base != tc.wantBase {
				t.Errorf("Base = %q, want %q", h.Base, tc.wantBase)
			}
		})
	}
}

func TestResolveHeadsMultiplePathsAndFillIns(t *testing.T) {
	if got := ResolveHeads(nil); len(got) != 0 {
		t.Errorf("ResolveHeads(nil) = %v", got)
	}
	if got := ResolveHeads(map[string]*Journal{mA: nil, mB: {Machine: mB}}); len(got) != 0 {
		t.Errorf("ResolveHeads(nil journal) = %v", got)
	}

	js := map[string]*Journal{
		mA: {Machine: mA, Entries: map[string]Entry{
			"a.txt":  {Kind: KindFile, Blob: "a", Clock: Clock{mA: 1}}, // Path and Machine unset
			"both":   {Path: "both", Kind: KindFile, Blob: "v1", Clock: Clock{mA: 1}, Machine: mA},
			"gone":   {Path: "gone", Kind: KindDeleted, Clock: Clock{mA: 5}, Machine: mA},
			"shared": {Path: "shared", Kind: KindFile, Blob: "s", Clock: Clock{mA: 1}, Machine: mA},
		}},
		mB: {Machine: mB, Entries: map[string]Entry{
			"b.txt":  {Path: "b.txt", Kind: KindFile, Blob: "b", Clock: Clock{mB: 1}, Machine: mB},
			"both":   {Path: "both", Kind: KindFile, Blob: "v2", Clock: Clock{mA: 1, mB: 1}, Parents: []string{"v1"}, Machine: mB},
			"shared": {Path: "shared", Kind: KindFile, Blob: "s", Clock: Clock{mA: 1}, Machine: mB},
		}},
	}
	heads := ResolveHeads(js)
	var paths []string
	for p := range heads {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if want := []string{"a.txt", "b.txt", "both", "gone", "shared"}; !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths = %v, want %v", paths, want)
	}
	if h := heads["a.txt"]; h.Entry == nil || h.Entry.Path != "a.txt" || h.Entry.Machine != mA {
		t.Errorf("a.txt fill-ins: %+v", h.Entry)
	}
	if h := heads["both"]; h.Entry == nil || h.Entry.Blob != "v2" || len(h.Candidates) != 1 {
		t.Errorf("both: %+v", h)
	}
	if h := heads["gone"]; h.Entry == nil || h.Entry.Kind != KindDeleted {
		t.Errorf("gone: %+v", h)
	}
	if h := heads["shared"]; h.Entry == nil || len(h.Candidates) != 1 {
		t.Errorf("shared (equal clocks, same content): %+v", h)
	}
	if h := heads["b.txt"]; h.Entry == nil || h.Entry.Machine != mB {
		t.Errorf("b.txt: %+v", h)
	}
}

// ---- end-to-end: two machines through the on-disk vault ---------------------

func TestTwoMachinesEndToEnd(t *testing.T) {
	va, dir := newVault(t)
	vb := openAs(t, dir, mB)

	base, _, err := va.WriteBlob([]byte("v1"))
	if err != nil {
		t.Fatal(err)
	}
	ja := &Journal{Machine: mA, Entries: map[string]Entry{
		"secret.env": {Path: "secret.env", Kind: KindFile, Blob: base, Clock: Clock{mA: 1}, Machine: mA, UpdatedAt: time.Now()},
	}}
	if err := va.WriteJournal(projA, ja); err != nil {
		t.Fatal(err)
	}

	// B fetches, edits from base; A edits from base concurrently.
	js, _, err := vb.ReadJournals(projA)
	if err != nil {
		t.Fatal(err)
	}
	heads := ResolveHeads(js)
	h := heads["secret.env"]
	if h.Entry == nil || h.Entry.Blob != base {
		t.Fatalf("B sees head %+v", h)
	}
	bBlob, _, _ := vb.WriteBlob([]byte("v2 from B"))
	jb := &Journal{Machine: mB, Entries: map[string]Entry{
		"secret.env": {Path: "secret.env", Kind: KindFile, Blob: bBlob, Clock: h.Entry.Clock.Tick(mB), Parents: []string{base}, Machine: mB, UpdatedAt: time.Now()},
	}}
	if err := vb.WriteJournal(projA, jb); err != nil {
		t.Fatal(err)
	}
	aBlob, _, _ := va.WriteBlob([]byte("v2 from A"))
	ja.Entries["secret.env"] = Entry{Path: "secret.env", Kind: KindFile, Blob: aBlob, Clock: Clock{mA: 2}, Parents: []string{base}, Machine: mA, UpdatedAt: time.Now()}
	if err := va.WriteJournal(projA, ja); err != nil {
		t.Fatal(err)
	}

	js, _, err = va.ReadJournals(projA)
	if err != nil {
		t.Fatal(err)
	}
	h = ResolveHeads(js)["secret.env"]
	if h.Entry != nil || len(h.Candidates) != 2 || h.Base != base {
		t.Fatalf("expected concurrent head with base %s, got %+v", base, h)
	}
	// Both sides' content is readable by either machine.
	for _, c := range h.Candidates {
		if _, err := vb.ReadBlob(c.Blob); err != nil {
			t.Errorf("ReadBlob(%s): %v", c.Blob, err)
		}
	}

	// A merges: clock = merge of candidates ticked, parents = both blobs.
	merged, _, _ := va.WriteBlob([]byte("merged"))
	clock := h.Candidates[0].Clock.Merge(h.Candidates[1].Clock).Tick(mA)
	ja.Entries["secret.env"] = Entry{Path: "secret.env", Kind: KindFile, Blob: merged, Clock: clock, Parents: []string{aBlob, bBlob}, Machine: mA, UpdatedAt: time.Now()}
	if err := va.WriteJournal(projA, ja); err != nil {
		t.Fatal(err)
	}
	js, _, _ = vb.ReadJournals(projA)
	h = ResolveHeads(js)["secret.env"]
	if h.Entry == nil || h.Entry.Blob != merged || len(h.Candidates) != 1 {
		t.Fatalf("after merge: %+v", h)
	}
	if ja.Seq != 3 {
		t.Errorf("A's Seq = %d, want 3", ja.Seq)
	}

	// Everything A pushed is listed, slash separated, in order, once.
	written := va.Written()
	want := []string{VaultFileName, BlobPath(base), "projects/" + projA + "/state/" + mA + DocSuffix, BlobPath(aBlob), BlobPath(merged)}
	if !reflect.DeepEqual(written, want) {
		t.Errorf("A.Written() = %v\nwant %v", written, want)
	}
}

// ---- regression tests -------------------------------------------------------

// A writer id names temp files and gates foreign writes, so it must be a
// machine-shaped uuid or empty (unrestricted tooling mode).
func TestWriterIDValidation(t *testing.T) {
	_, dir := newVault(t)
	for _, bad := range []string{"desk", "a/b", `a\b`, strings.ToUpper(mA), mA + " (1)", "../../etc"} {
		newDir := filepath.Join(t.TempDir(), "v")
		p := pass()
		if _, err := Create(newDir, p, fastParams(t), bad); !errors.Is(err, ErrBadMachineID) {
			t.Errorf("Create(writer %q): err = %v, want ErrBadMachineID", bad, err)
		}
		if fsutil.Exists(newDir) {
			t.Errorf("Create(writer %q) created the directory", bad)
		}
		if !bytes.Equal(p, make([]byte, len(p))) {
			t.Errorf("Create(writer %q) left the passphrase in memory", bad)
		}
		p = pass()
		if _, err := Open(dir, p, bad); !errors.Is(err, ErrBadMachineID) {
			t.Errorf("Open(writer %q): err = %v, want ErrBadMachineID", bad, err)
		}
		if !bytes.Equal(p, make([]byte, len(p))) {
			t.Errorf("Open(writer %q) left the passphrase in memory", bad)
		}
	}

	// The empty writer id is the unrestricted mode: generic temp suffix,
	// any machine's files may be written, and the temp files it leaves are
	// its own (silently skipped) rather than reported.
	tool, err := Open(dir, pass(), "")
	if err != nil {
		t.Fatalf("Open(writer \"\"): %v", err)
	}
	defer tool.Close()
	if tool.WriterID() != "" {
		t.Errorf("WriterID() = %q", tool.WriterID())
	}
	for _, m := range []string{mA, mB} {
		if err := tool.WriteJournal(projA, &Journal{Machine: m}); err != nil {
			t.Errorf("tooling WriteJournal(%s): %v", m, err)
		}
	}
	writeRaw(t, dir, "projects/"+projA+"/state/"+mC+DocSuffix+fsutil.TempPrefix+"w", []byte("own"))
	js, warnings, err := tool.ReadJournals(projA)
	if err != nil || len(js) != 2 || len(warnings) != 0 {
		t.Errorf("tooling ReadJournals = %d journals, %v, %v", len(js), warnings, err)
	}
	if err := tool.Rekey([]byte("tool"), fastParams(t)); err != nil {
		t.Errorf("tooling Rekey: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, VaultFileName)); err != nil {
		t.Errorf("vault.json after tooling Rekey: %v", err)
	}
}

// Rekey and Close may race: whatever the interleaving, vault.json must open
// with exactly one of the two passphrases and the vault key must be intact
// (a Close that zeroes the key mid-Rekey would persist an all-zero key).
func TestRekeyConcurrentClose(t *testing.T) {
	for i := 0; i < 6; i++ {
		dir := filepath.Join(t.TempDir(), "vault")
		v, err := Create(dir, pass(), fastParams(t), mA)
		if err != nil {
			t.Fatal(err)
		}
		blob, _, err := v.WriteBlob([]byte("survives"))
		if err != nil {
			t.Fatal(err)
		}
		rekeyErr := make(chan error, 1)
		go func() { rekeyErr <- v.Rekey([]byte("new"), fastParams(t)) }()
		if i%2 == 1 {
			time.Sleep(time.Duration(i) * time.Millisecond)
		}
		v.Close()
		err = <-rekeyErr

		var reopened *Vault
		switch {
		case err == nil:
			if reopened, err = Open(dir, []byte("new"), mA); err != nil {
				t.Fatalf("iteration %d: Rekey succeeded but Open(new) failed: %v", i, err)
			}
			if _, err := Open(dir, pass(), mA); !errors.Is(err, ErrWrongPassphrase) {
				t.Errorf("iteration %d: old passphrase still opens after Rekey: %v", i, err)
			}
		case errors.Is(err, ErrClosed):
			if reopened, err = Open(dir, pass(), mA); err != nil {
				t.Fatalf("iteration %d: Rekey lost to Close but Open(old) failed: %v", i, err)
			}
		default:
			t.Fatalf("iteration %d: Rekey err = %v, want nil or ErrClosed", i, err)
		}
		if pt, err := reopened.ReadBlob(blob); err != nil || string(pt) != "survives" {
			t.Errorf("iteration %d: blob after Rekey/Close race = %q, %v (vault key damaged?)", i, pt, err)
		}
		reopened.Close()
	}
}

// Two Rekeys racing each other leave meta and vault.json describing the same
// wrap: the last one to install wins and its passphrase opens the vault.
func TestRekeyConcurrentRekey(t *testing.T) {
	v, dir := newVault(t)
	errs := make(chan error, 2)
	go func() { errs <- v.Rekey([]byte("one"), fastParams(t)) }()
	go func() { errs <- v.Rekey([]byte("two"), fastParams(t)) }()
	for i := 0; i < 2; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("Rekey: %v", err)
		}
	}
	opened := 0
	for _, p := range []string{"one", "two"} {
		v2, err := Open(dir, []byte(p), mA)
		if err == nil {
			opened++
			v2.Close()
		} else if !errors.Is(err, ErrWrongPassphrase) {
			t.Errorf("Open(%q): %v", p, err)
		}
	}
	if opened != 1 {
		t.Errorf("%d of the two passphrases open the vault, want exactly 1", opened)
	}
	if _, err := Open(dir, pass(), mA); !errors.Is(err, ErrWrongPassphrase) {
		t.Errorf("original passphrase after two rekeys: %v", err)
	}
}

// Accessors reading vault.json metadata are safe on nil and closed handles.
func TestMetaAccessorsNilAndClosed(t *testing.T) {
	var nilVault *Vault
	if nilVault.BlobID([]byte("x")) != "" {
		t.Error("nil.BlobID() != \"\"")
	}
	if p := nilVault.KDFParams(); !reflect.DeepEqual(p, crypto.KDFParams{}) {
		t.Errorf("nil.KDFParams() = %+v", p)
	}
	if !nilVault.CreatedAt().IsZero() {
		t.Error("nil.CreatedAt() is not zero")
	}
	if err := nilVault.Rekey([]byte("x"), crypto.KDFParams{}); !errors.Is(err, ErrClosed) {
		t.Errorf("nil.Rekey: %v", err)
	}

	v, _ := newVault(t)
	params, created := v.KDFParams(), v.CreatedAt()
	v.Close()
	if p := v.KDFParams(); !reflect.DeepEqual(p, params) {
		t.Errorf("KDFParams after Close = %+v, want %+v", p, params)
	}
	if !v.CreatedAt().Equal(created) {
		t.Error("CreatedAt changed after Close")
	}
	// KDFParams returns a detached salt.
	p := v.KDFParams()
	p.Salt[0] ^= 0xff
	if bytes.Equal(v.KDFParams().Salt, p.Salt) {
		t.Error("KDFParams() shares its salt buffer with the vault")
	}
}

// Blob reads are bounded like document reads: a file above the cap is
// reported as unavailable without being loaded (a sparse file stands in for
// a multi-gigabyte one).
func TestReadBlobTooLarge(t *testing.T) {
	v, dir := newVault(t)
	id := "ab" + strings.Repeat("cd", 31)
	abs := filepath.Join(dir, filepath.FromSlash(BlobPath(id)))
	if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxBlobSize + 1); err != nil {
		f.Close()
		t.Skipf("cannot create a sparse %d-byte file here: %v", maxBlobSize+1, err)
	}
	f.Close()
	if st, err := os.Stat(abs); err != nil || st.Size() != maxBlobSize+1 {
		t.Skipf("sparse file not supported here: %v", err)
	}

	start := time.Now()
	_, err = v.ReadBlob(id)
	if !errors.Is(err, ErrBlobMissing) || !errors.Is(err, fsutil.ErrTooLarge) {
		t.Errorf("ReadBlob oversized: err = %v, want ErrBlobMissing wrapping ErrTooLarge", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("ReadBlob read the oversized file instead of refusing it by size")
	}
	if !v.HasBlob(id) {
		t.Error("HasBlob = false for an existing (oversized) blob file")
	}
	// WriteBlob of a plaintext colliding with an oversized file simply rewrites it.
	// (Not reachable with real ids; exercises the "existing unreadable" path.)
	if _, created, err := v.WriteBlob([]byte("fresh")); err != nil || !created {
		t.Errorf("WriteBlob after oversized file: created=%v err=%v", created, err)
	}
}

// Concurrent WriteBlob calls for the same content share one temp file name
// (per-writer suffix); they must be serialised so the blob is never left
// truncated and exactly one caller reports creating it.
func TestWriteBlobConcurrent(t *testing.T) {
	v, _ := newVault(t)
	const workers = 24
	pt := bytes.Repeat([]byte("the same secret content, repeated to make the write non-trivial\n"), 512)

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		createdN  int
		ids       = map[string]struct{}{}
		firstErr  error
		different = make([][]byte, workers)
	)
	for i := range different {
		different[i] = []byte(fmt.Sprintf("distinct content %d", i))
	}
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id, created, err := v.WriteBlob(pt)
			_, _, err2 := v.WriteBlob(different[i])
			mu.Lock()
			defer mu.Unlock()
			if err != nil && firstErr == nil {
				firstErr = err
			}
			if err2 != nil && firstErr == nil {
				firstErr = err2
			}
			if created {
				createdN++
			}
			ids[id] = struct{}{}
		}(i)
	}
	wg.Wait()
	if firstErr != nil {
		t.Fatalf("WriteBlob: %v", firstErr)
	}
	if createdN != 1 {
		t.Errorf("created reported by %d writers, want exactly 1", createdN)
	}
	if len(ids) != 1 {
		t.Errorf("ids = %v, want one id", ids)
	}
	got, err := v.ReadBlob(v.BlobID(pt))
	if err != nil || !bytes.Equal(got, pt) {
		t.Errorf("blob after concurrent writes: len=%d err=%v (truncated?)", len(got), err)
	}
	for i, d := range different {
		if got, err := v.ReadBlob(v.BlobID(d)); err != nil || !bytes.Equal(got, d) {
			t.Errorf("distinct blob %d: %q, %v", i, got, err)
		}
	}
	written := v.Written()
	seen := map[string]int{}
	for _, w := range written {
		seen[w]++
	}
	if seen[BlobPath(v.BlobID(pt))] != 1 || len(written) != 1+1+workers {
		t.Errorf("Written() = %d entries (%v), want vault.json + %d blobs once each", len(written), written, workers+1)
	}
}

// A single head produced by several surviving candidates carries the merge
// of their clocks, so a writer that ticks Entry.Clock dominates all of them.
func TestResolveHeadsMergedClock(t *testing.T) {
	t1 := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		entries   []he
		wantKind  Kind
		wantClock Clock
	}{
		{
			name: "same content written independently",
			entries: []he{
				{mA, KindFile, "same", Clock{mA: 1}, nil, t1},
				{mB, KindFile, "same", Clock{mB: 1}, nil, t1},
			},
			wantKind: KindFile, wantClock: Clock{mA: 1, mB: 1},
		},
		{
			name: "deleted on both sides concurrently",
			entries: []he{
				{mA, KindDeleted, "", Clock{mA: 2, mB: 1}, nil, t1},
				{mB, KindDeleted, "", Clock{mA: 1, mB: 3}, nil, t1},
			},
			wantKind: KindDeleted, wantClock: Clock{mA: 2, mB: 3},
		},
		{
			name: "untracked wins over a concurrent edit",
			entries: []he{
				{mA, KindUntracked, "", Clock{mA: 2}, nil, t1},
				{mB, KindFile, "edit", Clock{mA: 1, mB: 1}, nil, t1},
			},
			wantKind: KindUntracked, wantClock: Clock{mA: 2, mB: 1},
		},
		{
			name: "three machines agree",
			entries: []he{
				{mA, KindFile, "x", Clock{mA: 1}, nil, t1},
				{mB, KindFile, "x", Clock{mB: 1}, nil, t1},
				{mC, KindFile, "x", Clock{mC: 1}, nil, t1},
			},
			wantKind: KindFile, wantClock: Clock{mA: 1, mB: 1, mC: 1},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			js := journalsOf("f", tc.entries...)
			h := ResolveHeads(js)["f"]
			if h.Entry == nil || h.Entry.Kind != tc.wantKind {
				t.Fatalf("head = %+v, want single %v head", h, tc.wantKind)
			}
			if !reflect.DeepEqual(h.Entry.Clock, tc.wantClock) {
				t.Errorf("Entry.Clock = %v, want merge %v", h.Entry.Clock, tc.wantClock)
			}
			// Candidates keep their own clocks.
			for _, c := range h.Candidates {
				if !reflect.DeepEqual(c.Clock, js[c.Machine].Entries["f"].Clock) {
					t.Errorf("candidate %s clock changed to %v", c.Machine, c.Clock)
				}
			}
			// The spec's write rule via the head: the next write dominates
			// every survivor, so the following fetch sees a single head.
			next := *h.Entry
			next.Machine, next.Kind, next.Blob = mC, KindFile, "next"
			next.Clock = h.Entry.Clock.Tick(mC)
			js[mC] = &Journal{Machine: mC, Entries: map[string]Entry{"f": next}}
			h2 := ResolveHeads(js)["f"]
			if h2.Entry == nil || h2.Entry.Blob != "next" || len(h2.Candidates) != 1 || h2.Candidates[0].Machine != mC {
				t.Errorf("after ticking the merged clock: %+v, want single head from %s", h2, mC)
			}
			// Ticking only one survivor's clock (the pre-fix behaviour) would
			// have left a spurious concurrent head.
			if len(tc.entries) > 1 {
				stale := next
				stale.Clock = js[tc.entries[0].machine].Entries["f"].Clock.Tick(mC)
				js[mC] = &Journal{Machine: mC, Entries: map[string]Entry{"f": stale}}
				if h3 := ResolveHeads(js)["f"]; h3.Entry != nil && tc.wantKind != KindUntracked {
					t.Errorf("ticking a single survivor's clock resolved to %+v; the merge is needed", h3.Entry)
				}
			}
		})
	}

	// A lone survivor keeps its exact clock (nil stays nil) and the head's
	// entry is detached from the candidate.
	h := ResolveHeads(journalsOf("f", he{mA, KindFile, "x", nil, []string{"p"}, t1}))["f"]
	if h.Entry.Clock != nil {
		t.Errorf("lone nil clock became %v", h.Entry.Clock)
	}
	h = ResolveHeads(journalsOf("f", he{mA, KindFile, "x", Clock{mA: 4}, []string{"p"}, t1}))["f"]
	if !reflect.DeepEqual(h.Entry.Clock, Clock{mA: 4}) {
		t.Errorf("lone clock = %v", h.Entry.Clock)
	}
	h.Entry.Clock[mB] = 9
	h.Entry.Parents[0] = "changed"
	if _, leaked := h.Candidates[0].Clock[mB]; leaked || h.Candidates[0].Parents[0] != "p" {
		t.Error("Head.Entry shares its Clock/Parents with Candidates[0]")
	}
}
