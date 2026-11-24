package hustrun

import (
	"crypto/aes"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

func TestEncryptECBRoundTrip(t *testing.T) {
	const key = "0123456789abcdef"
	const plaintext = `{"sportId":"00000000-0000-4000-8000-000000000000","dllc":"2.15"}`

	ciphertext, err := EncryptECB(plaintext, key)
	if err != nil {
		t.Fatalf("EncryptECB: %v", err)
	}
	if len(ciphertext)%aes.BlockSize != 0 {
		t.Fatalf("ciphertext length %d is not block aligned", len(ciphertext))
	}
	decrypted, err := DecryptECB(ciphertext, key)
	if err != nil {
		t.Fatalf("DecryptECB: %v", err)
	}
	if string(decrypted) != plaintext {
		t.Fatalf("round trip = %q, want %q", decrypted, plaintext)
	}
}

// TestEncryptECBLayout pins the mode and padding: ECB over a PKCS#7-padded
// block is deterministic, so the ciphertext must equal a direct block
// encryption of the padded plaintext.
func TestEncryptECBLayout(t *testing.T) {
	const key = "0123456789abcdef"
	const plaintext = "hustrun"

	ciphertext, err := EncryptECB(plaintext, key)
	if err != nil {
		t.Fatalf("EncryptECB: %v", err)
	}
	block, err := aes.NewCipher([]byte(key))
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	padded := make([]byte, 16)
	copy(padded, plaintext)
	for index := len(plaintext); index < 16; index++ {
		padded[index] = byte(16 - len(plaintext))
	}
	expected := make([]byte, 16)
	block.Encrypt(expected, padded)
	if hex.EncodeToString(ciphertext) != hex.EncodeToString(expected) {
		t.Fatalf("ciphertext %x != expected %x", ciphertext, expected)
	}
}

func TestEncryptForUploadEncodings(t *testing.T) {
	const key = "0123456789abcdef"
	const plaintext = `{"a":1}`

	encoded, err := EncryptForUpload(plaintext, key, "base64")
	if err != nil {
		t.Fatalf("base64: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if decrypted, err := DecryptECB(raw, key); err != nil || string(decrypted) != plaintext {
		t.Fatalf("base64 round trip failed: %q %v", decrypted, err)
	}

	hexText, err := EncryptForUpload(plaintext, key, "hex")
	if err != nil {
		t.Fatalf("hex: %v", err)
	}
	raw, err = hex.DecodeString(hexText)
	if err != nil {
		t.Fatalf("hex decode: %v", err)
	}
	if decrypted, err := DecryptECB(raw, key); err != nil || string(decrypted) != plaintext {
		t.Fatalf("hex round trip failed: %q %v", decrypted, err)
	}

	if _, err := EncryptForUpload(plaintext, key, "rot13"); err == nil {
		t.Fatal("unknown encoding accepted")
	}
}

func TestEncryptClientHeaderBeacon(t *testing.T) {
	const key = "0123456789abcdef"
	fixed := time.UnixMilli(1792980518965)

	encoded, err := EncryptClientHeader(key, false, "base64", fixed)
	if err != nil {
		t.Fatalf("EncryptClientHeader: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	decrypted, err := DecryptECB(raw, key)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	const want = `{"heap":"0","root":"0","t":"1792980518965"}`
	if string(decrypted) != want {
		t.Fatalf("client beacon = %q, want %q", decrypted, want)
	}

	rooted, err := EncryptClientHeader(key, true, "base64", fixed)
	if err != nil {
		t.Fatalf("EncryptClientHeader(root): %v", err)
	}
	raw, _ = base64.StdEncoding.DecodeString(rooted)
	decrypted, _ = DecryptECB(raw, key)
	if !strings.Contains(string(decrypted), `"root":"1"`) {
		t.Fatalf("root flag missing: %q", decrypted)
	}
}

func TestV1Header(t *testing.T) {
	fixed := time.UnixMilli(1792980518965)
	first := V1Header("abc", fixed)
	if len(first) != 32 {
		t.Fatalf("v1 = %q, want a 32-character digest", first)
	}
	if first != V1Header("abc", fixed) {
		t.Fatal("v1 is not stable for a fixed seed")
	}
	if first == V1Header("abd", fixed) {
		t.Fatal("v1 ignores the seed")
	}
	if V1Header("", fixed) == V1Header("", fixed.Add(time.Millisecond)) {
		t.Fatal("v1 does not follow the clock when the seed is empty")
	}
}

func TestEncryptSensorData(t *testing.T) {
	const key = "0123456789abcdef"
	encoded, err := EncryptSensorData("[9.8,9.9]", key, "base64")
	if err != nil {
		t.Fatalf("EncryptSensorData: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	decrypted, err := DecryptECB(raw, key)
	if err != nil || string(decrypted) != "[9.8,9.9]" {
		t.Fatalf("sensor round trip = %q (%v)", decrypted, err)
	}
}

func TestDecryptECBRejectsBadPadding(t *testing.T) {
	// A zero block decrypts to a padding byte of zero, which is invalid.
	ciphertext := make([]byte, 16)
	if _, err := DecryptECB(ciphertext, "0123456789abcdef"); err == nil {
		t.Fatal("invalid padding was accepted")
	}
	if _, err := DecryptECB([]byte("short"), "0123456789abcdef"); err == nil {
		t.Fatal("unaligned ciphertext was accepted")
	}
}
