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

func TestNewKeyring_DerivesSubjectSubKey(t *testing.T) {
	master := bytes.Repeat([]byte{0x77}, MasterKeyLen)
	kr, err := NewKeyring(master)
	if err != nil {
		t.Fatalf("NewKeyring: %v", err)
	}
	if len(kr.Subject) != KeyLen {
		t.Errorf("Subject sub-key length = %d, want %d", len(kr.Subject), KeyLen)
	}
	// Must match a direct DeriveSubKey call — they have to be bit-identical
	// because both paths produce the same on-disk ciphertexts.
	want, err := DeriveSubKey(master, LabelSubject)
	if err != nil {
		t.Fatalf("DeriveSubKey: %v", err)
	}
	if !bytes.Equal(kr.Subject, want) {
		t.Errorf("Keyring.Subject diverged from DeriveSubKey(LabelSubject)")
	}
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
