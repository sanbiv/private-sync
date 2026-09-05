package crypto

import (
	"bytes"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// Known-answer vectors. Generated once from fixedVK() / katPlaintext and
// cross-checked against an independent HKDF/HMAC implementation. If any of
// these change, the on-disk format has drifted and existing vaults break.
const (
	katEnc     = "d0fbca8b4860e87139ae99c61325439562637a6c0e79fece3a990c2a0f7d3d3b"
	katMac     = "dab65b02a41c99bb154c7de62d1e1b934aa41b1dc5f816d384251feb84044fad"
	katNonce   = "91c7f5f9143e11dc4379f528bcb7437a63a9977983d30025bcd4ef26c5bd5c06"
	katBlobID  = "ed479a41b636c5a4d042efe1a5f3930326ca52f58b10d1aa6dcfd32e0b114866"
	katBlob    = "505356318b11abb76d5db70b01b909a7468fc6eb178e98f202034f6e49a72a0b2c6a371cbca7d5e1550fcc2a4dde595ee3a2fa01bcbc587917c7e9022cae9b4339ee4dc0ac6bfce8d6174c8bdd4fb1a2c8b4059db0bae15e"
	katKeyedID = "d2bbd7952435d43f771203a12ec0fb8987c7c0bbc3d5cfca25a025a319743119"
	// Argon2id(passphrase="correct horse battery staple", salt=0xA0..0xAF, t=1, m=8192 KiB, p=1).
	katKEK = "69c063433c5ed8277914f61461e8d8e3635e6c10a68fc75aca95b069ef25c71c"
	// vault.json wrapped_key: seal(katKEK, nonce=0x10..0x27, aad=katWrappedAAD, fixedVK()).
	katWrappedAAD = "vault-key:0123456789abcdef"
	katWrapped    = "50535631101112131415161718191a1b1c1d1e1f20212223242526279e5c59c2402bcfb30791e389e34a3415023667dbaecf1290252081f63e05bb85a17092d0d79af3a9ad0d6db1941a52f9"
	// Project id: hex(HMAC-SHA256(katMac, "project:"+katProjectFP))[:16].
	katProjectFP      = "git:github.com/sanbiv/private-sync"
	katProjectKeyedID = "a585756c4474a0b0ab8bf722c8b836bccb949f7786d2d15ee4fa51186a8a9710"
	katProjectID      = "a585756c4474a0b0"
)

// fixedNonce returns the 24-byte nonce 0x10..0x27 used by the wrapped-key KAT.
func fixedNonce() []byte {
	n := make([]byte, NonceSize)
	for i := range n {
		n[i] = byte(0x10 + i)
	}
	return n
}

const katPlaintext = "The quick brown fox jumps over the lazy dog\n"

func fixedVK() []byte {
	vk := make([]byte, KeySize)
	for i := range vk {
		vk[i] = byte(i)
	}
	return vk
}

func fixedSalt() []byte {
	salt := make([]byte, SaltSize)
	for i := range salt {
		salt[i] = byte(0xA0 + i)
	}
	return salt
}

// cheapKDF returns valid but fast parameters for tests.
func cheapKDF() KDFParams {
	return KDFParams{Algo: KDFAlgo, Salt: fixedSalt(), Time: 1, Memory: KDFMinMemory, Threads: 1}
}

func mustKeys(t *testing.T, vk []byte) *Keys {
	t.Helper()
	k, err := NewKeys(vk)
	if err != nil {
		t.Fatalf("NewKeys: %v", err)
	}
	return k
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex constant: %v", err)
	}
	return b
}

func TestKnownAnswer(t *testing.T) {
	k := mustKeys(t, fixedVK())
	if got := hex.EncodeToString(k.enc); got != katEnc {
		t.Errorf("enc subkey = %s, want %s", got, katEnc)
	}
	if got := hex.EncodeToString(k.mac); got != katMac {
		t.Errorf("mac subkey = %s, want %s", got, katMac)
	}
	if got := hex.EncodeToString(k.nonce); got != katNonce {
		t.Errorf("nonce subkey = %s, want %s", got, katNonce)
	}

	pt := []byte(katPlaintext)
	if got := k.BlobID(pt); got != katBlobID {
		t.Errorf("BlobID = %s, want %s", got, katBlobID)
	}
	id, ct, err := k.SealBlob(pt)
	if err != nil {
		t.Fatalf("SealBlob: %v", err)
	}
	if id != katBlobID {
		t.Errorf("SealBlob id = %s, want %s", id, katBlobID)
	}
	if got := hex.EncodeToString(ct); got != katBlob {
		t.Errorf("SealBlob ciphertext =\n%s\nwant\n%s", got, katBlob)
	}
	if got := k.KeyedID("project:", []byte("fingerprint")); got != katKeyedID {
		t.Errorf("KeyedID = %s, want %s", got, katKeyedID)
	}
	if got := k.KeyedID(ProjectIDPrefix, []byte(katProjectFP)); got != katProjectKeyedID {
		t.Errorf("KeyedID(project) = %s, want %s", got, katProjectKeyedID)
	}
	if got := k.ProjectID(katProjectFP); got != katProjectID {
		t.Errorf("ProjectID = %s, want %s", got, katProjectID)
	}

	// The frozen ciphertext must still open.
	back, err := k.OpenBlob(katBlobID, mustHex(t, katBlob))
	if err != nil {
		t.Fatalf("OpenBlob(frozen): %v", err)
	}
	if string(back) != katPlaintext {
		t.Errorf("OpenBlob(frozen) = %q, want %q", back, katPlaintext)
	}
}

func TestKnownAnswerKEK(t *testing.T) {
	kek, err := DeriveKEK([]byte("correct horse battery staple"), cheapKDF())
	if err != nil {
		t.Fatalf("DeriveKEK: %v", err)
	}
	if got := hex.EncodeToString(kek); got != katKEK {
		t.Errorf("KEK = %s, want %s", got, katKEK)
	}
	if len(kek) != KeySize {
		t.Errorf("KEK len = %d, want %d", len(kek), KeySize)
	}
}

func TestBlobFormatLayout(t *testing.T) {
	k := mustKeys(t, fixedVK())
	pt := []byte(katPlaintext)
	id, ct, err := k.SealBlob(pt)
	if err != nil {
		t.Fatal(err)
	}
	if len(ct) != HeaderSize+len(pt)+TagSize {
		t.Errorf("len = %d, want %d", len(ct), HeaderSize+len(pt)+TagSize)
	}
	if string(ct[:len(Magic)]) != Magic {
		t.Errorf("magic = %q", ct[:len(Magic)])
	}
	wantNonce := hmacSHA256(k.nonce, pt)[:NonceSize]
	if !bytes.Equal(ct[len(Magic):HeaderSize], wantNonce) {
		t.Errorf("nonce is not HMAC(nonceKey, plaintext)[:24]")
	}
	if id != hex.EncodeToString(hmacSHA256(k.mac, pt)) {
		t.Errorf("id is not hex(HMAC(mac, plaintext))")
	}
}

func TestSealBlobDeterministic(t *testing.T) {
	k := mustKeys(t, fixedVK())
	for _, pt := range [][]byte{nil, {}, []byte("a"), []byte(katPlaintext), bytes.Repeat([]byte{0xff}, 4096)} {
		id1, ct1, err := k.SealBlob(pt)
		if err != nil {
			t.Fatalf("SealBlob(%d bytes): %v", len(pt), err)
		}
		id2, ct2, err := k.SealBlob(pt)
		if err != nil {
			t.Fatalf("SealBlob(%d bytes) again: %v", len(pt), err)
		}
		if id1 != id2 {
			t.Errorf("ids differ for %d-byte plaintext", len(pt))
		}
		if !bytes.Equal(ct1, ct2) {
			t.Errorf("ciphertexts differ for %d-byte plaintext", len(pt))
		}
		if len(id1) != 64 {
			t.Errorf("id len = %d, want 64", len(id1))
		}
		back, err := k.OpenBlob(id1, ct1)
		if err != nil {
			t.Fatalf("OpenBlob(%d bytes): %v", len(pt), err)
		}
		if !bytes.Equal(back, pt) && !(len(back) == 0 && len(pt) == 0) {
			t.Errorf("round trip mismatch for %d-byte plaintext", len(pt))
		}
	}
}

func TestDifferentInputsDifferentOutputs(t *testing.T) {
	k1 := mustKeys(t, fixedVK())
	vk2 := fixedVK()
	vk2[0] ^= 1
	k2 := mustKeys(t, vk2)
	pt := []byte(katPlaintext)

	if hex.EncodeToString(k1.enc) == hex.EncodeToString(k2.enc) {
		t.Error("enc subkeys equal for different vks")
	}
	if k1.BlobID(pt) == k2.BlobID(pt) {
		t.Error("blob ids equal for different vks")
	}
	_, ct1, _ := k1.SealBlob(pt)
	_, ct2, _ := k2.SealBlob(pt)
	if bytes.Equal(ct1, ct2) {
		t.Error("ciphertexts equal for different vks")
	}
	if k1.BlobID(pt) == k1.BlobID([]byte(katPlaintext+"x")) {
		t.Error("blob ids equal for different plaintexts")
	}
	// Subkeys must be pairwise distinct.
	if bytes.Equal(k1.enc, k1.mac) || bytes.Equal(k1.enc, k1.nonce) || bytes.Equal(k1.mac, k1.nonce) {
		t.Error("subkeys are not distinct")
	}
	// Blob from k1 must not open under k2.
	id, ct, _ := k1.SealBlob(pt)
	if _, err := k2.OpenBlob(id, ct); !errors.Is(err, ErrAuth) {
		t.Errorf("OpenBlob with other key: err = %v, want ErrAuth", err)
	}
}

func TestOpenBlobFailures(t *testing.T) {
	k := mustKeys(t, fixedVK())
	pt := []byte(katPlaintext)
	id, ct, err := k.SealBlob(pt)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		id   string
		ct   func() []byte
		want error
	}{
		{"nil", id, func() []byte { return nil }, ErrFormat},
		{"empty", id, func() []byte { return []byte{} }, ErrFormat},
		{"magic only", id, func() []byte { return []byte(Magic) }, ErrFormat},
		{"header only", id, func() []byte { return ct[:HeaderSize] }, ErrFormat},
		{"one byte short of minimum", id, func() []byte { return ct[:MinCiphertextSize-1] }, ErrFormat},
		{"truncated tail", id, func() []byte { return ct[:len(ct)-1] }, ErrAuth},
		{"bad magic", id, func() []byte {
			c := bytes.Clone(ct)
			c[0] = 'X'
			return c
		}, ErrFormat},
		{"lowercase magic", id, func() []byte {
			c := bytes.Clone(ct)
			copy(c, "psv1")
			return c
		}, ErrFormat},
		{"tamper nonce", id, func() []byte {
			c := bytes.Clone(ct)
			c[len(Magic)] ^= 0x01
			return c
		}, ErrAuth},
		{"tamper body", id, func() []byte {
			c := bytes.Clone(ct)
			c[HeaderSize+3] ^= 0x80
			return c
		}, ErrAuth},
		{"tamper tag", id, func() []byte {
			c := bytes.Clone(ct)
			c[len(c)-1] ^= 0x01
			return c
		}, ErrAuth},
		{"wrong id (AAD)", strings.Repeat("0", 64), func() []byte { return ct }, ErrAuth},
		{"empty id (AAD)", "", func() []byte { return ct }, ErrAuth},
		{"extra byte appended", id, func() []byte { return append(bytes.Clone(ct), 0) }, ErrAuth},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := k.OpenBlob(tc.id, tc.ct())
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Errorf("plaintext returned on failure: %q", got)
			}
		})
	}
}

func TestDocRoundTrip(t *testing.T) {
	k := mustKeys(t, fixedVK())
	for _, tc := range []struct {
		aad string
		pt  []byte
	}{
		{"journals/abc.enc", []byte(`{"seq":1}`)},
		{"", []byte("no aad")},
		{"trash:0123", nil},
		{"machines/x", bytes.Repeat([]byte("z"), 10000)},
	} {
		ct, err := k.SealDoc(tc.aad, tc.pt)
		if err != nil {
			t.Fatalf("SealDoc(%q): %v", tc.aad, err)
		}
		if len(ct) != HeaderSize+len(tc.pt)+TagSize {
			t.Errorf("SealDoc(%q) len = %d", tc.aad, len(ct))
		}
		if string(ct[:len(Magic)]) != Magic {
			t.Errorf("SealDoc(%q): bad magic", tc.aad)
		}
		back, err := k.OpenDoc(tc.aad, ct)
		if err != nil {
			t.Fatalf("OpenDoc(%q): %v", tc.aad, err)
		}
		if !bytes.Equal(back, tc.pt) && !(len(back) == 0 && len(tc.pt) == 0) {
			t.Errorf("OpenDoc(%q) mismatch", tc.aad)
		}
	}
}

func TestDocRandomNonce(t *testing.T) {
	k := mustKeys(t, fixedVK())
	pt := []byte("same document")
	ct1, err := k.SealDoc("p", pt)
	if err != nil {
		t.Fatal(err)
	}
	ct2, err := k.SealDoc("p", pt)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(ct1, ct2) {
		t.Error("SealDoc produced identical output twice; nonce is not random")
	}
	if bytes.Equal(ct1[len(Magic):HeaderSize], ct2[len(Magic):HeaderSize]) {
		t.Error("SealDoc reused a nonce")
	}
}

func TestOpenDocFailures(t *testing.T) {
	k := mustKeys(t, fixedVK())
	ct, err := k.SealDoc("journals/a.enc", []byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		aad  string
		ct   []byte
		want error
	}{
		{"moved file (wrong path AAD)", "journals/b.enc", ct, ErrAuth},
		{"empty AAD", "", ct, ErrAuth},
		{"tampered", "journals/a.enc", func() []byte {
			c := bytes.Clone(ct)
			c[HeaderSize] ^= 1
			return c
		}(), ErrAuth},
		{"truncated", "journals/a.enc", ct[:HeaderSize+2], ErrFormat},
		{"garbage", "journals/a.enc", bytes.Repeat([]byte{1}, 100), ErrFormat},
		{"nil", "journals/a.enc", nil, ErrFormat},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := k.OpenDoc(tc.aad, tc.ct); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
	// Blob and doc formats are not interchangeable via AAD confusion.
	id, bct, _ := k.SealBlob([]byte("payload"))
	if _, err := k.OpenDoc("blob:"+id, bct); err != nil {
		t.Errorf("OpenDoc with blob AAD should work (same construction): %v", err)
	}
	if _, err := k.OpenDoc(id, bct); !errors.Is(err, ErrAuth) {
		t.Errorf("OpenDoc without blob: prefix: err = %v, want ErrAuth", err)
	}
}

func TestWrapUnwrapKey(t *testing.T) {
	kek, err := DeriveKEK([]byte("pw"), cheapKDF())
	if err != nil {
		t.Fatal(err)
	}
	vk, err := NewVaultKey()
	if err != nil {
		t.Fatal(err)
	}
	const aad = "vault-key:0123456789abcdef"

	wrapped, err := WrapKey(kek, vk, aad)
	if err != nil {
		t.Fatalf("WrapKey: %v", err)
	}
	if len(wrapped) != HeaderSize+KeySize+TagSize {
		t.Errorf("wrapped len = %d, want %d", len(wrapped), HeaderSize+KeySize+TagSize)
	}
	if string(wrapped[:len(Magic)]) != Magic {
		t.Error("wrapped key lacks magic")
	}
	if bytes.Contains(wrapped, vk) {
		t.Error("wrapped key contains vk in clear")
	}

	got, err := UnwrapKey(kek, wrapped, aad)
	if err != nil {
		t.Fatalf("UnwrapKey: %v", err)
	}
	if !bytes.Equal(got, vk) {
		t.Error("UnwrapKey returned different vk")
	}

	// Two wraps differ (random nonce) but both unwrap.
	wrapped2, err := WrapKey(kek, vk, aad)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(wrapped, wrapped2) {
		t.Error("WrapKey is deterministic; nonce must be random")
	}
	if got2, err := UnwrapKey(kek, wrapped2, aad); err != nil || !bytes.Equal(got2, vk) {
		t.Errorf("second unwrap: %v", err)
	}

	// Wrong kek (different passphrase).
	badKek, err := DeriveKEK([]byte("pW"), cheapKDF())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapKey(badKek, wrapped, aad); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong kek: err = %v, want ErrAuth", err)
	}
	// Wrong salt gives a different kek too.
	p := cheapKDF()
	p.Salt[0] ^= 1
	saltKek, err := DeriveKEK([]byte("pw"), p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapKey(saltKek, wrapped, aad); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong salt kek: err = %v, want ErrAuth", err)
	}
	// Wrong vault id (AAD).
	if _, err := UnwrapKey(kek, wrapped, "vault-key:other"); !errors.Is(err, ErrAuth) {
		t.Errorf("wrong aad: err = %v, want ErrAuth", err)
	}
	// Tampered.
	tampered := bytes.Clone(wrapped)
	tampered[HeaderSize+5] ^= 0x10
	if _, err := UnwrapKey(kek, tampered, aad); !errors.Is(err, ErrAuth) {
		t.Errorf("tampered: err = %v, want ErrAuth", err)
	}
	// Malformed.
	for _, bad := range [][]byte{nil, {}, []byte("PSV1"), wrapped[:10], append([]byte("XXXX"), wrapped[4:]...)} {
		if _, err := UnwrapKey(kek, bad, aad); !errors.Is(err, ErrFormat) {
			t.Errorf("malformed %d bytes: err = %v, want ErrFormat", len(bad), err)
		}
	}
	// Wrapped payload that is valid but not 32 bytes.
	notKey, err := sealRandom(kek, aad, []byte("short"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := UnwrapKey(kek, notKey, aad); !errors.Is(err, ErrKeySize) {
		t.Errorf("non-32-byte payload: err = %v, want ErrKeySize", err)
	}
}

func TestWrapKeyBadInputs(t *testing.T) {
	kek := bytes.Repeat([]byte{7}, KeySize)
	if _, err := WrapKey(kek, []byte("short"), "a"); !errors.Is(err, ErrKeySize) {
		t.Errorf("short vk: err = %v, want ErrKeySize", err)
	}
	if _, err := WrapKey(kek[:16], fixedVK(), "a"); !errors.Is(err, ErrKeySize) {
		t.Errorf("short kek: err = %v, want ErrKeySize", err)
	}
	if _, err := WrapKey(nil, fixedVK(), "a"); !errors.Is(err, ErrKeySize) {
		t.Errorf("nil kek: err = %v, want ErrKeySize", err)
	}
	if _, err := UnwrapKey(nil, bytes.Repeat([]byte{1}, 100), "a"); !errors.Is(err, ErrKeySize) {
		t.Errorf("nil kek unwrap: err = %v, want ErrKeySize", err)
	}
}

func TestKDFParamsValidate(t *testing.T) {
	good := func() KDFParams { return cheapKDF() }
	tests := []struct {
		name    string
		mutate  func(*KDFParams)
		wantErr bool
		field   string
	}{
		{"default valid", func(p *KDFParams) {}, false, ""},
		{"max bounds", func(p *KDFParams) { p.Time = 16; p.Memory = 1024 * 1024; p.Threads = 16 }, false, ""},
		{"min bounds", func(p *KDFParams) { p.Time = 1; p.Memory = 8192; p.Threads = 1 }, false, ""},
		{"empty algo", func(p *KDFParams) { p.Algo = "" }, true, "algo"},
		{"wrong algo", func(p *KDFParams) { p.Algo = "argon2i" }, true, "algo"},
		{"algo case", func(p *KDFParams) { p.Algo = "Argon2id" }, true, "algo"},
		{"nil salt", func(p *KDFParams) { p.Salt = nil }, true, "salt"},
		{"short salt", func(p *KDFParams) { p.Salt = p.Salt[:15] }, true, "salt"},
		{"long salt", func(p *KDFParams) { p.Salt = append(p.Salt, 0) }, true, "salt"},
		{"time 0", func(p *KDFParams) { p.Time = 0 }, true, "time"},
		{"time 17", func(p *KDFParams) { p.Time = 17 }, true, "time"},
		{"time huge", func(p *KDFParams) { p.Time = ^uint32(0) }, true, "time"},
		{"memory 0", func(p *KDFParams) { p.Memory = 0 }, true, "memory"},
		{"memory 8191", func(p *KDFParams) { p.Memory = 8191 }, true, "memory"},
		{"memory 1GiB+1", func(p *KDFParams) { p.Memory = 1024*1024 + 1 }, true, "memory"},
		{"memory huge", func(p *KDFParams) { p.Memory = ^uint32(0) }, true, "memory"},
		{"threads 0", func(p *KDFParams) { p.Threads = 0 }, true, "threads"},
		{"threads 17", func(p *KDFParams) { p.Threads = 17 }, true, "threads"},
		{"threads 255", func(p *KDFParams) { p.Threads = 255 }, true, "threads"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := good()
			tc.mutate(&p)
			err := p.Validate()
			if !tc.wantErr {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrKDFParams) {
				t.Fatalf("err = %v, want ErrKDFParams", err)
			}
			if !strings.Contains(err.Error(), tc.field) {
				t.Errorf("error %q does not name field %q", err, tc.field)
			}
		})
	}
	// Zero value is invalid.
	if err := (KDFParams{}).Validate(); !errors.Is(err, ErrKDFParams) {
		t.Errorf("zero params: err = %v", err)
	}
}

func TestDeriveKEKValidatesFirst(t *testing.T) {
	// Absurd memory must be rejected before argon2 tries to allocate it.
	p := cheapKDF()
	p.Memory = ^uint32(0)
	if _, err := DeriveKEK([]byte("pw"), p); !errors.Is(err, ErrKDFParams) {
		t.Errorf("err = %v, want ErrKDFParams", err)
	}
	p = cheapKDF()
	p.Threads = 0
	if _, err := DeriveKEK([]byte("pw"), p); !errors.Is(err, ErrKDFParams) {
		t.Errorf("threads=0: err = %v, want ErrKDFParams", err)
	}
	// Empty passphrase is allowed by the KDF itself (policy lives elsewhere).
	if kek, err := DeriveKEK(nil, cheapKDF()); err != nil || len(kek) != KeySize {
		t.Errorf("empty passphrase: kek len %d, err %v", len(kek), err)
	}
}

// Regression: spec §4 says the passphrase buffer is zeroed right after
// Argon2id. DeriveKEK owns that wipe so no caller can forget it.
func TestDeriveKEKZeroesPassphrase(t *testing.T) {
	pw := []byte("correct horse battery staple")
	kek, err := DeriveKEK(pw, cheapKDF())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Errorf("passphrase not zeroed after DeriveKEK: %q", pw)
	}
	// Wiping the input must not change the output.
	if got := hex.EncodeToString(kek); got != katKEK {
		t.Errorf("KEK = %s, want %s", got, katKEK)
	}
	// Zeroed even when validation rejects the parameters.
	pw = []byte("secret")
	bad := cheapKDF()
	bad.Time = 0
	if _, err := DeriveKEK(pw, bad); !errors.Is(err, ErrKDFParams) {
		t.Fatalf("err = %v, want ErrKDFParams", err)
	}
	if !bytes.Equal(pw, make([]byte, len(pw))) {
		t.Errorf("passphrase not zeroed on validation failure: %q", pw)
	}
	// A second derivation from the (now zeroed) buffer is a different key,
	// i.e. the wipe really happened at the byte level.
	again, err := DeriveKEK(pw, cheapKDF())
	if err != nil {
		t.Fatal(err)
	}
	empty, err := DeriveKEK(make([]byte, 6), cheapKDF())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again, empty) {
		t.Error("zeroed buffer should derive like an all-zero passphrase")
	}
	// nil and empty passphrases must not panic.
	if _, err := DeriveKEK(nil, cheapKDF()); err != nil {
		t.Errorf("nil passphrase: %v", err)
	}
	if _, err := DeriveKEK([]byte{}, cheapKDF()); err != nil {
		t.Errorf("empty passphrase: %v", err)
	}
}

func TestDefaultKDFParams(t *testing.T) {
	p, err := DefaultKDFParams()
	if err != nil {
		t.Fatal(err)
	}
	if p.Algo != "argon2id" || p.Time != 3 || p.Memory != 65536 || p.Threads != 4 {
		t.Errorf("unexpected defaults: %+v", p)
	}
	if len(p.Salt) != SaltSize {
		t.Errorf("salt len = %d", len(p.Salt))
	}
	if err := p.Validate(); err != nil {
		t.Errorf("defaults do not validate: %v", err)
	}
	q, err := DefaultKDFParams()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(p.Salt, q.Salt) {
		t.Error("two DefaultKDFParams calls produced the same salt")
	}
	if bytes.Equal(p.Salt, make([]byte, SaltSize)) {
		t.Error("salt is all zeros")
	}
}

func TestNewVaultKey(t *testing.T) {
	a, err := NewVaultKey()
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewVaultKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != KeySize || len(b) != KeySize {
		t.Errorf("lens %d %d", len(a), len(b))
	}
	if bytes.Equal(a, b) {
		t.Error("two vault keys are equal")
	}
	if bytes.Equal(a, make([]byte, KeySize)) {
		t.Error("vault key is all zeros")
	}
}

func TestNewKeysRejectsBadVK(t *testing.T) {
	for _, vk := range [][]byte{nil, {}, make([]byte, 16), make([]byte, 31), make([]byte, 33), make([]byte, 64)} {
		k, err := NewKeys(vk)
		if !errors.Is(err, ErrKeySize) {
			t.Errorf("len %d: err = %v, want ErrKeySize", len(vk), err)
		}
		if k != nil {
			t.Errorf("len %d: keys returned on error", len(vk))
		}
	}
}

func TestKeyedID(t *testing.T) {
	k := mustKeys(t, fixedVK())
	a := k.KeyedID("project:", []byte("fp"))
	if len(a) != 64 {
		t.Errorf("len = %d, want 64", len(a))
	}
	if _, err := hex.DecodeString(a); err != nil {
		t.Errorf("not hex: %v", err)
	}
	// Equivalent to hex(HMAC(mac, label||data)); label/data boundary is not delimited.
	if a != hex.EncodeToString(hmacSHA256(k.mac, []byte("project:fp"))) {
		t.Error("KeyedID != hex(HMAC(mac, label||data))")
	}
	if a != k.KeyedID("project:f", []byte("p")) {
		t.Error("KeyedID depends on label/data split (should be plain concatenation)")
	}
	if a == k.KeyedID("machine:", []byte("fp")) {
		t.Error("different labels collide")
	}
	if a == k.KeyedID("project:", []byte("fq")) {
		t.Error("different data collide")
	}
	if k.KeyedID("", nil) != hex.EncodeToString(hmacSHA256(k.mac, nil)) {
		t.Error("empty KeyedID mismatch")
	}
	if a == k.BlobID([]byte("project:fp")) {
		// Same construction by design: both are HMAC(mac, bytes). Document it.
		t.Log("KeyedID and BlobID share the mac key (expected; labels keep domains apart)")
	}
	other := mustKeys(t, append([]byte{1}, fixedVK()[1:]...))
	if a == other.KeyedID("project:", []byte("fp")) {
		t.Error("KeyedID equal under different vks")
	}
}

func TestRandomHex(t *testing.T) {
	for _, n := range []int{0, 1, 8, 16, 32} {
		s, err := RandomHex(n)
		if err != nil {
			t.Fatalf("RandomHex(%d): %v", n, err)
		}
		if len(s) != 2*n {
			t.Errorf("RandomHex(%d) len = %d", n, len(s))
		}
		if _, err := hex.DecodeString(s); err != nil {
			t.Errorf("RandomHex(%d) not hex: %v", n, err)
		}
	}
	a, _ := RandomHex(16)
	b, _ := RandomHex(16)
	if a == b {
		t.Error("RandomHex repeated")
	}
	if _, err := RandomHex(-1); err == nil {
		t.Error("RandomHex(-1) should fail")
	}
}

func TestZero(t *testing.T) {
	b := []byte{1, 2, 3}
	Zero(b)
	if !bytes.Equal(b, []byte{0, 0, 0}) {
		t.Errorf("Zero: %v", b)
	}
	Zero(nil)
	Zero([]byte{})

	var nk *Keys
	nk.Zero() // must not panic
	(&Keys{}).Zero()
}

// Regression: after Zero the backing arrays are wiped AND the Keys value is
// unusable — a use-after-Close must fail loudly, never encrypt with all-zero
// keys and emit valid-looking PSV1 output.
func TestKeysZeroDisablesKeys(t *testing.T) {
	k := mustKeys(t, fixedVK())
	pt := []byte(katPlaintext)
	id, blob, err := k.SealBlob(pt)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := k.SealDoc("journals/a.enc", pt)
	if err != nil {
		t.Fatal(err)
	}
	// Capture the slices before Zero so we can check the arrays were wiped.
	enc, mac, nonce := k.enc, k.mac, k.nonce
	if !k.ready() {
		t.Fatal("keys not ready before Zero")
	}

	k.Zero()

	zeros := make([]byte, KeySize)
	if !bytes.Equal(enc, zeros) || !bytes.Equal(mac, zeros) || !bytes.Equal(nonce, zeros) {
		t.Error("Keys.Zero did not wipe the subkey bytes")
	}
	if k.enc != nil || k.mac != nil || k.nonce != nil {
		t.Error("Keys.Zero left subkey slices allocated")
	}
	if k.ready() {
		t.Fatal("ready() is true after Zero")
	}
	if got := k.BlobID(pt); got != "" {
		t.Errorf("BlobID after Zero = %q, want empty", got)
	}
	if got := k.KeyedID("project:", []byte("fp")); got != "" {
		t.Errorf("KeyedID after Zero = %q, want empty", got)
	}
	if got := k.ProjectID("git:x"); got != "" {
		t.Errorf("ProjectID after Zero = %q, want empty", got)
	}
	if gotID, gotCT, err := k.SealBlob(pt); err == nil || gotID != "" || gotCT != nil {
		t.Errorf("SealBlob after Zero = (%q, %d bytes, %v), want error", gotID, len(gotCT), err)
	}
	if gotCT, err := k.SealDoc("journals/a.enc", pt); err == nil || gotCT != nil {
		t.Errorf("SealDoc after Zero = (%d bytes, %v), want error", len(gotCT), err)
	}
	if got, err := k.OpenBlob(id, blob); err == nil || got != nil {
		t.Errorf("OpenBlob after Zero = (%q, %v), want error", got, err)
	}
	if got, err := k.OpenDoc("journals/a.enc", doc); err == nil || got != nil {
		t.Errorf("OpenDoc after Zero = (%q, %v), want error", got, err)
	}
	// Errors must be the not-ready sentinel, not ErrAuth/ErrFormat, so the
	// caller sees a programming error rather than a corrupt vault.
	if _, err := k.OpenBlob(id, blob); errors.Is(err, ErrAuth) || errors.Is(err, ErrFormat) || !errors.Is(err, errKeysNotReady) {
		t.Errorf("OpenBlob after Zero: err = %v, want errKeysNotReady", err)
	}
	// Idempotent.
	k.Zero()
	if k.ready() {
		t.Error("ready() after second Zero")
	}
	// A fresh Keys from the same vk is unaffected (Zero touched only k).
	k2 := mustKeys(t, fixedVK())
	back, err := k2.OpenBlob(id, blob)
	if err != nil || string(back) != katPlaintext {
		t.Errorf("fresh keys cannot open blob sealed before Zero: %v", err)
	}
}

func TestNilAndEmptyKeys(t *testing.T) {
	var nk *Keys
	if nk.BlobID([]byte("x")) != "" || nk.KeyedID("a", nil) != "" {
		t.Error("nil Keys should return empty ids")
	}
	if _, _, err := nk.SealBlob([]byte("x")); err == nil {
		t.Error("nil Keys SealBlob should fail")
	}
	if _, err := nk.OpenBlob("", nil); err == nil {
		t.Error("nil Keys OpenBlob should fail")
	}
	if _, err := nk.SealDoc("", nil); err == nil {
		t.Error("nil Keys SealDoc should fail")
	}
	if _, err := nk.OpenDoc("", nil); err == nil {
		t.Error("nil Keys OpenDoc should fail")
	}
	ek := &Keys{}
	if ek.BlobID([]byte("x")) != "" {
		t.Error("zero Keys should return empty id")
	}
	if _, _, err := ek.SealBlob([]byte("x")); err == nil {
		t.Error("zero Keys SealBlob should fail")
	}
}

func TestSubkeysMatchHKDFDefinition(t *testing.T) {
	// Each subkey must be exactly HKDF(vk, nil, InfoPrefix+label).
	vk := fixedVK()
	k := mustKeys(t, vk)
	for _, tc := range []struct {
		label string
		got   []byte
	}{{"enc", k.enc}, {"mac", k.mac}, {"nonce", k.nonce}} {
		want, err := deriveSubkey(vk, tc.label)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(tc.got, want) {
			t.Errorf("%s subkey mismatch", tc.label)
		}
		// And a different info prefix must give a different key.
		other, _ := deriveSubkey(vk, tc.label+"x")
		if bytes.Equal(tc.got, other) {
			t.Errorf("%s subkey insensitive to info", tc.label)
		}
	}
}

// TestKnownAnswerWrappedKey freezes the vault.json wrapped_key path, the one
// artifact that lives in git history forever: fixed KEK + fixed nonce + fixed
// VK must produce katWrapped, and katWrapped must unwrap back to fixedVK().
func TestKnownAnswerWrappedKey(t *testing.T) {
	kek := mustHex(t, katKEK)
	want := mustHex(t, katWrapped)

	got, err := seal(kek, fixedNonce(), katWrappedAAD, fixedVK())
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("wrapped key =\n%x\nwant\n%s", got, katWrapped)
	}
	if len(want) != HeaderSize+KeySize+TagSize {
		t.Errorf("katWrapped len = %d, want %d", len(want), HeaderSize+KeySize+TagSize)
	}
	if !bytes.Equal(want[len(Magic):HeaderSize], fixedNonce()) {
		t.Error("katWrapped does not carry the fixed nonce in its header")
	}

	// The frozen bytes unwrap through the public API.
	vk, err := UnwrapKey(kek, want, katWrappedAAD)
	if err != nil {
		t.Fatalf("UnwrapKey(frozen): %v", err)
	}
	if !bytes.Equal(vk, fixedVK()) {
		t.Errorf("UnwrapKey(frozen) = %x, want fixedVK", vk)
	}
	// And the unwrapped key derives the frozen subkeys, tying both KATs together.
	k := mustKeys(t, vk)
	if hex.EncodeToString(k.enc) != katEnc {
		t.Error("unwrapped vk does not derive katEnc")
	}

	failures := []struct {
		name string
		kek  []byte
		ct   []byte
		aad  string
		want error
	}{
		{"wrong vault id (AAD)", kek, want, "vault-key:fedcba9876543210", ErrAuth},
		{"empty AAD", kek, want, "", ErrAuth},
		{"AAD without prefix", kek, want, "0123456789abcdef", ErrAuth},
		{"wrong kek", bytes.Repeat([]byte{0x42}, KeySize), want, katWrappedAAD, ErrAuth},
		{"tampered nonce", kek, func() []byte { c := bytes.Clone(want); c[len(Magic)] ^= 1; return c }(), katWrappedAAD, ErrAuth},
		{"tampered body", kek, func() []byte { c := bytes.Clone(want); c[HeaderSize+7] ^= 1; return c }(), katWrappedAAD, ErrAuth},
		{"tampered tag", kek, func() []byte { c := bytes.Clone(want); c[len(c)-1] ^= 1; return c }(), katWrappedAAD, ErrAuth},
		{"truncated", kek, want[:MinCiphertextSize-1], katWrappedAAD, ErrFormat},
		{"bad magic", kek, append([]byte("PSV2"), want[4:]...), katWrappedAAD, ErrFormat},
	}
	for _, tc := range failures {
		t.Run(tc.name, func(t *testing.T) {
			got, err := UnwrapKey(tc.kek, tc.ct, tc.aad)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
			if got != nil {
				t.Errorf("key returned on failure")
			}
		})
	}

	// WrapKey (random nonce) under the frozen KEK still unwraps with the frozen AAD.
	w, err := WrapKey(kek, fixedVK(), katWrappedAAD)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(w, want) {
		t.Error("WrapKey reproduced the fixed-nonce vector; nonce is not random")
	}
	if vk2, err := UnwrapKey(kek, w, katWrappedAAD); err != nil || !bytes.Equal(vk2, fixedVK()) {
		t.Errorf("UnwrapKey(WrapKey) = %v", err)
	}
}

func TestProjectID(t *testing.T) {
	k := mustKeys(t, fixedVK())
	id := k.ProjectID(katProjectFP)
	if id != katProjectID {
		t.Errorf("ProjectID = %s, want %s", id, katProjectID)
	}
	if len(id) != ProjectIDLen {
		t.Errorf("len = %d, want %d", len(id), ProjectIDLen)
	}
	if _, err := hex.DecodeString(id); err != nil {
		t.Errorf("not hex: %v", err)
	}
	// It is exactly the truncated KeyedID with the "project:" label.
	if full := k.KeyedID(ProjectIDPrefix, []byte(katProjectFP)); !strings.HasPrefix(full, id) || full[:ProjectIDLen] != id {
		t.Errorf("ProjectID is not KeyedID[:16]: %s vs %s", id, full)
	}
	// Matches the by-hand definition from spec §4.
	if want := hex.EncodeToString(hmacSHA256(k.mac, []byte("project:"+katProjectFP)))[:16]; id != want {
		t.Errorf("ProjectID != hex(HMAC(mac, \"project:\"+fp))[:16]")
	}
	// Different fingerprints and different vaults give different ids.
	if id == k.ProjectID("git:github.com/sanbiv/other") {
		t.Error("distinct fingerprints collide")
	}
	if id == k.ProjectID("dir:private-sync") {
		t.Error("distinct fingerprint kinds collide")
	}
	other := mustKeys(t, append([]byte{0xff}, fixedVK()[1:]...))
	if id == other.ProjectID(katProjectFP) {
		t.Error("same fingerprint under different vk collides")
	}
	// Deterministic.
	if id != k.ProjectID(katProjectFP) {
		t.Error("ProjectID not deterministic")
	}
	// Empty fingerprint still yields a well-formed id (policy lives in identity).
	if e := k.ProjectID(""); len(e) != ProjectIDLen {
		t.Errorf("ProjectID(\"\") len = %d", len(e))
	}
	// Not-ready keys yield "".
	var nk *Keys
	if nk.ProjectID(katProjectFP) != "" || (&Keys{}).ProjectID(katProjectFP) != "" {
		t.Error("not-ready keys should return empty project id")
	}
}

// Regression: document AADs must be slash separated so a document sealed on
// Windows opens on macOS/Linux. A backslash is rejected up front with ErrAAD
// instead of surfacing later as a baffling ErrAuth on another machine.
func TestDocAADCanonicalForm(t *testing.T) {
	k := mustKeys(t, fixedVK())
	pt := []byte(`{"seq":1}`)

	good := []string{
		"projects/a585756c4474a0b0/state/m.json.enc",
		"journals/abc.enc",
		"trash:0123456789abcdef",
		"machines/x",
		"",
		"unicode/ünïcødé.enc",
	}
	for _, aad := range good {
		ct, err := k.SealDoc(aad, pt)
		if err != nil {
			t.Errorf("SealDoc(%q): %v", aad, err)
			continue
		}
		if back, err := k.OpenDoc(aad, ct); err != nil || !bytes.Equal(back, pt) {
			t.Errorf("OpenDoc(%q): %v", aad, err)
		}
	}

	bad := []string{
		`projects\a585756c4474a0b0\state\m.json.enc`,
		`journals\abc.enc`,
		`\`,
		`mixed/dir\file.enc`,
		`trash:\x`,
	}
	for _, aad := range bad {
		ct, err := k.SealDoc(aad, pt)
		if !errors.Is(err, ErrAAD) {
			t.Errorf("SealDoc(%q): err = %v, want ErrAAD", aad, err)
		}
		if ct != nil {
			t.Errorf("SealDoc(%q) returned ciphertext on error", aad)
		}
		if !strings.Contains(err.Error(), "backslash") {
			t.Errorf("SealDoc(%q) error does not explain itself: %v", aad, err)
		}
		got, err := k.OpenDoc(aad, bytes.Repeat([]byte{1}, 100))
		if !errors.Is(err, ErrAAD) {
			t.Errorf("OpenDoc(%q): err = %v, want ErrAAD", aad, err)
		}
		if got != nil {
			t.Errorf("OpenDoc(%q) returned plaintext on error", aad)
		}
	}

	// The guard runs before AEAD: a valid document opened with the backslash
	// form of its own path is ErrAAD (a programming error), not ErrAuth.
	ct, err := k.SealDoc("projects/x/state/m.json.enc", pt)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := k.OpenDoc(`projects\x\state\m.json.enc`, ct); !errors.Is(err, ErrAAD) || errors.Is(err, ErrAuth) {
		t.Errorf("backslash path: err = %v, want ErrAAD only", err)
	}
	// Not-ready keys report the not-ready error first (no AAD inspection needed).
	var nk *Keys
	if _, err := nk.SealDoc(`a\b`, pt); !errors.Is(err, errKeysNotReady) {
		t.Errorf("nil keys: err = %v, want errKeysNotReady", err)
	}
	// Blobs are unaffected: their AAD is derived, never caller supplied.
	if _, _, err := k.SealBlob([]byte(`C:\Users\x`)); err != nil {
		t.Errorf("SealBlob with backslashes in plaintext: %v", err)
	}
	// WrapKey/UnwrapKey use the caller's AAD verbatim (no path semantics).
	kek := mustHex(t, katKEK)
	if _, err := WrapKey(kek, fixedVK(), `vault-key:a\b`); err != nil {
		t.Errorf("WrapKey does not impose path rules: %v", err)
	}
}
