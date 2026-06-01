package dbcrypto

import (
	"bytes"
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
)

// --- HKDF ---

// TestHKDF_RFC5869_TC1 pins the stdlib HKDF-SHA256 implementation against
// RFC 5869 §A.1 Test Case 1. If this ever drifts, every existing ciphertext
// in the database becomes unreadable — so it lives here as a tripwire.
func TestHKDF_RFC5869_TC1(t *testing.T) {
	ikm := bytes.Repeat([]byte{0x0b}, 22)
	salt, _ := hex.DecodeString("000102030405060708090a0b0c")
	info, _ := hex.DecodeString("f0f1f2f3f4f5f6f7f8f9")
	wantHex := "3cb25f25faacd57a90434f64d0362f2a" +
		"2d2d0a90cf1a5a4c5db02d56ecc4c5bf" +
		"34007208d5b887185865"

	got, err := hkdf.Key(sha256.New, ikm, salt, string(info), 42)
	if err != nil {
		t.Fatalf("hkdf.Key: %v", err)
	}
	if hex.EncodeToString(got) != wantHex {
		t.Errorf("OKM mismatch:\n got=%x\nwant=%s", got, wantHex)
	}
}

func TestDeriveSubKey_DeterministicAndLabelled(t *testing.T) {
	master := bytes.Repeat([]byte{0x42}, MasterKeyLen)

	a, err := DeriveSubKey(master, LabelSubject)
	if err != nil {
		t.Fatalf("derive a: %v", err)
	}
	b, err := DeriveSubKey(master, LabelSubject)
	if err != nil {
		t.Fatalf("derive b: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Error("DeriveSubKey is non-deterministic for the same (master, label)")
	}
	if len(a) != KeyLen {
		t.Errorf("sub-key length = %d, want %d", len(a), KeyLen)
	}

	other, err := DeriveSubKey(master, LabelBody)
	if err != nil {
		t.Fatalf("derive other: %v", err)
	}
	if bytes.Equal(a, other) {
		t.Error("different labels produced identical sub-keys — HKDF info is not in effect")
	}
}

func TestDeriveSubKey_RejectsBadMasterLen(t *testing.T) {
	for _, n := range []int{0, 16, 31, 33, 64} {
		_, err := DeriveSubKey(make([]byte, n), LabelBody)
		if !errors.Is(err, ErrInvalidKey) {
			t.Errorf("master len %d: err = %v, want ErrInvalidKey", n, err)
		}
	}
}

// --- Encrypt envelope shape ---

func TestEncrypt_EnvelopeShape(t *testing.T) {
	key := bytes.Repeat([]byte{0x11}, KeyLen)
	plain := []byte("hello, world")

	ct, err := Encrypt(key, plain)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if got, want := len(ct), envelopeOverhead+len(plain); got != want {
		t.Errorf("ciphertext length = %d, want %d", got, want)
	}
	if ct[0] != EnvelopeV1 {
		t.Errorf("version byte = 0x%02x, want 0x%02x", ct[0], EnvelopeV1)
	}
}

func TestEncrypt_NoncesAreUnique(t *testing.T) {
	key := bytes.Repeat([]byte{0x22}, KeyLen)
	seen := map[string]struct{}{}
	for i := range 100 {
		ct, err := Encrypt(key, []byte("x"))
		if err != nil {
			t.Fatalf("Encrypt #%d: %v", i, err)
		}
		nonce := string(ct[1 : 1+NonceLen])
		if _, dup := seen[nonce]; dup {
			t.Fatalf("nonce collision after %d encryptions", i+1)
		}
		seen[nonce] = struct{}{}
	}
}

func TestEncrypt_RejectsBadKey(t *testing.T) {
	if _, err := Encrypt(make([]byte, 16), []byte("x")); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("err = %v, want ErrInvalidKey", err)
	}
}

// --- Round-trip (requires Decrypt to be implemented) ---

func TestRoundTrip_AllLabels(t *testing.T) {
	master := bytes.Repeat([]byte{0x99}, MasterKeyLen)
	labels := []Label{
		LabelBody, LabelSubject, LabelHeaders, LabelAddrs,
		LabelDraft, LabelContact, LabelFTSToken, LabelMeta,
	}
	plaintexts := [][]byte{
		nil,
		{},
		[]byte("a"),
		[]byte("hello, world"),
		bytes.Repeat([]byte("durian "), 100),
	}

	for _, label := range labels {
		key, err := DeriveSubKey(master, label)
		if err != nil {
			t.Fatalf("derive %s: %v", label, err)
		}
		for _, pt := range plaintexts {
			ct, err := Encrypt(key, pt)
			if err != nil {
				t.Fatalf("[%s] Encrypt(%dB): %v", label, len(pt), err)
			}
			got, err := Decrypt(key, ct)
			if err != nil {
				t.Fatalf("[%s] Decrypt(%dB): %v", label, len(pt), err)
			}
			if !bytes.Equal(got, pt) {
				t.Errorf("[%s] round-trip mismatch:\n got=%q\nwant=%q", label, got, pt)
			}
		}
	}
}

func TestDecrypt_RejectsShortCiphertext(t *testing.T) {
	key := bytes.Repeat([]byte{0x33}, KeyLen)
	// Anything shorter than version+nonce+tag must be rejected before any AEAD work.
	for _, n := range []int{0, 1, NonceLen, envelopeOverhead - 1} {
		_, err := Decrypt(key, make([]byte, n))
		if !errors.Is(err, ErrShortCiphertext) {
			t.Errorf("len %d: err = %v, want ErrShortCiphertext", n, err)
		}
	}
}

func TestDecrypt_RejectsUnknownVersion(t *testing.T) {
	key := bytes.Repeat([]byte{0x44}, KeyLen)
	ct, err := Encrypt(key, []byte("payload"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	ct[0] = 0xff // corrupt the version byte
	_, err = Decrypt(key, ct)
	if !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("err = %v, want ErrUnknownVersion", err)
	}
}

func TestDecrypt_DetectsTampering(t *testing.T) {
	key := bytes.Repeat([]byte{0x55}, KeyLen)
	ct, err := Encrypt(key, []byte("authentic"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip a bit somewhere in the ciphertext body (past version+nonce).
	ct[1+NonceLen] ^= 0x01
	_, err = Decrypt(key, ct)
	if err == nil {
		t.Fatal("Decrypt accepted tampered ciphertext")
	}
	// Must not be mistaken for a structural error.
	if errors.Is(err, ErrShortCiphertext) || errors.Is(err, ErrUnknownVersion) {
		t.Errorf("tamper detection leaked as structural error: %v", err)
	}
	// AEAD failure message is stable enough to assert on loosely.
	if !strings.Contains(err.Error(), "authentication failed") &&
		!strings.Contains(err.Error(), "message authentication failed") {
		t.Logf("note: tamper error = %v (not a hard failure, just FYI)", err)
	}
}

func TestDecrypt_RejectsBadKey(t *testing.T) {
	if _, err := Decrypt(make([]byte, 16), make([]byte, envelopeOverhead)); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("err = %v, want ErrInvalidKey", err)
	}
}

// --- Keyring ---

func TestNewKeyring_DerivesAllSubKeys(t *testing.T) {
	master := bytes.Repeat([]byte{0x77}, MasterKeyLen)
	kr, err := NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	// Every sub-key must (a) be the right length and (b) match a direct
	// DeriveSubKey call bit-for-bit — both paths produce the same on-disk
	// ciphertexts, any drift here corrupts the entire DB.
	cases := []struct {
		name  string
		got   []byte
		label Label
	}{
		{"Subject", kr.Subject, LabelSubject},
		{"Body", kr.Body, LabelBody},
		{"Addrs", kr.Addrs, LabelAddrs},
		{"Headers", kr.Headers, LabelHeaders},
		{"Draft", kr.Draft, LabelDraft},
		{"Meta", kr.Meta, LabelMeta},
		{"Contact", kr.Contact, LabelContact},
		{"FTSToken", kr.FTSToken, LabelFTSToken},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if len(c.got) != KeyLen {
				t.Errorf("length = %d, want %d", len(c.got), KeyLen)
			}
			want, err := DeriveSubKey(master, c.label)
			if err != nil {
				t.Fatalf("DeriveSubKey: %v", err)
			}
			if !bytes.Equal(c.got, want) {
				t.Errorf("diverged from DeriveSubKey(%s)", c.label)
			}
		})
	}
	// Pairwise distinctness — guards against future "oops, copy-paste".
	all := [][]byte{kr.Subject, kr.Body, kr.Addrs, kr.Headers, kr.Draft, kr.Meta, kr.Contact, kr.FTSToken}
	var pairs [][2][]byte
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			pairs = append(pairs, [2][]byte{all[i], all[j]})
		}
	}
	for i, p := range pairs {
		if bytes.Equal(p[0], p[1]) {
			t.Errorf("pair %d: sub-keys collided", i)
		}
	}
}

// TestKeyring_CachedAEADMatchesFreshPath proves that ciphertext produced
// by the cached-AEAD per-sub-key methods round-trips through the
// package-level Decrypt(key, ct) path and vice versa. If the two paths
// ever diverge on envelope shape, every ciphertext written by one path
// becomes unreadable by the other — same risk class as the HKDF tripwire
// above.
func TestKeyring_CachedAEADMatchesFreshPath(t *testing.T) {
	master := bytes.Repeat([]byte{0xa5}, MasterKeyLen)
	kr, err := NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	plain := []byte("paths must agree on the wire format")

	cases := []struct {
		name    string
		key     []byte
		seal    func([]byte) ([]byte, error)
		open    func([]byte) ([]byte, error)
	}{
		{"Subject", kr.Subject, kr.EncryptSubject, kr.DecryptSubject},
		{"Body", kr.Body, kr.EncryptBody, kr.DecryptBody},
		{"Addrs", kr.Addrs, kr.EncryptAddrs, kr.DecryptAddrs},
		{"Headers", kr.Headers, kr.EncryptHeaders, kr.DecryptHeaders},
		{"Draft", kr.Draft, kr.EncryptDraft, kr.DecryptDraft},
		{"Meta", kr.Meta, kr.EncryptMeta, kr.DecryptMeta},
		{"Contact", kr.Contact, kr.EncryptContact, kr.DecryptContact},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// cached seal → fresh open
			ct, err := c.seal(plain)
			if err != nil {
				t.Fatalf("cached seal: %v", err)
			}
			got, err := Decrypt(c.key, ct)
			if err != nil {
				t.Fatalf("fresh open of cached ct: %v", err)
			}
			if !bytes.Equal(got, plain) {
				t.Errorf("fresh-open mismatch: got=%q want=%q", got, plain)
			}
			// fresh seal → cached open
			ct2, err := Encrypt(c.key, plain)
			if err != nil {
				t.Fatalf("fresh seal: %v", err)
			}
			got2, err := c.open(ct2)
			if err != nil {
				t.Fatalf("cached open of fresh ct: %v", err)
			}
			if !bytes.Equal(got2, plain) {
				t.Errorf("cached-open mismatch: got=%q want=%q", got2, plain)
			}
		})
	}
}

// TestKeyring_DecryptCached_RejectsStructuralErrors mirrors the
// package-level guards (short / unknown-version) for the cached path so
// callers see the same error shape regardless of which entry they took.
func TestKeyring_DecryptCached_RejectsStructuralErrors(t *testing.T) {
	kr, err := NewKeyring(bytes.Repeat([]byte{0x66}, MasterKeyLen))
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	for _, n := range []int{0, 1, NonceLen, envelopeOverhead - 1} {
		if _, err := kr.DecryptSubject(make([]byte, n)); !errors.Is(err, ErrShortCiphertext) {
			t.Errorf("len %d: err = %v, want ErrShortCiphertext", n, err)
		}
	}
	bad := make([]byte, envelopeOverhead)
	bad[0] = 0xff
	if _, err := kr.DecryptSubject(bad); !errors.Is(err, ErrUnknownVersion) {
		t.Errorf("bad version: err = %v, want ErrUnknownVersion", err)
	}
}

// BenchmarkDecrypt_FreshVsCached compares the package-level Decrypt path
// (which re-runs aes.NewCipher + cipher.NewGCM per call) against the
// Keyring-cached path. ADR-0001 audit #254.1 acceptance: ≥2× speedup.
//
// Run via:  go test -run=^$ -bench=Decrypt_FreshVsCached -benchmem ./...
func BenchmarkDecrypt_FreshVsCached(b *testing.B) {
	master := bytes.Repeat([]byte{0xc3}, MasterKeyLen)
	kr, err := NewKeyring(master)
	if err != nil {
		b.Fatalf("NewKeyring: %v", err)
	}
	plain := bytes.Repeat([]byte("durian-body "), 80) // ~1 KB
	ct, err := kr.EncryptBody(plain)
	if err != nil {
		b.Fatalf("seal: %v", err)
	}

	b.Run("fresh", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := Decrypt(kr.Body, ct); err != nil {
				b.Fatalf("decrypt: %v", err)
			}
		}
	})
	b.Run("cached", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := kr.DecryptBody(ct); err != nil {
				b.Fatalf("decrypt: %v", err)
			}
		}
	})
}

func TestNewKeyring_RejectsBadMasterLen(t *testing.T) {
	if _, err := NewKeyring(make([]byte, 16)); !errors.Is(err, ErrInvalidKey) {
		t.Errorf("err = %v, want ErrInvalidKey wrapped", err)
	}
}

func TestKeyring_Wipe(t *testing.T) {
	kr, err := NewKeyring(bytes.Repeat([]byte{0x33}, MasterKeyLen))
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	kr.Wipe()
	if kr.Subject != nil {
		t.Errorf("Subject not nilled after Wipe: %x", kr.Subject)
	}
	// Wipe must be safe on nil receiver — defensive for shutdown paths.
	var nilKr *Keyring
	nilKr.Wipe()
}
