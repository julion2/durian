package agekey

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	plaintext := []byte("the cake is a lie")
	const passphrase = "correct-horse-battery-staple"

	ct, err := Encrypt(plaintext, passphrase)
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	got, err := Decrypt(ct, passphrase)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Errorf("round-trip mismatch:\n got=%q\nwant=%q", got, plaintext)
	}
}

func TestEncrypt_ProducesAsciiArmoredOutput(t *testing.T) {
	// Armored age starts with a fixed header line — easy to paste into a
	// password manager. If we ever switch to binary format, this guard
	// catches the format change so docs/restore tooling can be updated.
	ct, err := Encrypt([]byte("hello"), "pw")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if !strings.HasPrefix(string(ct), "-----BEGIN AGE ENCRYPTED FILE-----") {
		t.Errorf("ciphertext does not start with armor header:\n%s", string(ct[:64]))
	}
}

func TestDecrypt_RejectsWrongPassphrase(t *testing.T) {
	ct, err := Encrypt([]byte("payload"), "right-pw")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	_, err = Decrypt(ct, "wrong-pw")
	if err == nil {
		t.Fatal("Decrypt accepted wrong passphrase")
	}
}

func TestDecrypt_RejectsTampering(t *testing.T) {
	ct, err := Encrypt([]byte("payload"), "pw")
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	// Flip a byte deep inside the armored body, past the header.
	idx := len(ct) / 2
	ct[idx] = ct[idx] ^ 0x01
	_, err = Decrypt(ct, "pw")
	if err == nil {
		t.Fatal("Decrypt accepted tampered ciphertext")
	}
}

func TestEncrypt_RejectsEmptyPassphrase(t *testing.T) {
	if _, err := Encrypt([]byte("x"), ""); err == nil {
		t.Error("Encrypt with empty passphrase should error")
	}
}

func TestDecrypt_RejectsEmptyPassphrase(t *testing.T) {
	if _, err := Decrypt([]byte("..."), ""); err == nil {
		t.Error("Decrypt with empty passphrase should error")
	}
}
