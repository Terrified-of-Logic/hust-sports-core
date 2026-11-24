package hustrun

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"encoding/binary"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCanonicalizeSigningTarget(t *testing.T) {
	cases := []struct {
		target string
		want   string
	}{
		// No query: returned verbatim.
		{"/api/l/v6/currentTimeMillis", "/api/l/v6/currentTimeMillis"},
		// '?', '=' and '&' are dropped after (key, value) sorting.
		{"/wq?a=z&ab=1", "/wqazab1"},
		{"/wt?a=9&a0=1", "/wta9a01"},
		{"/wr?x=z&x=a", "/wrxaxz"},
		{"/wf?x=2&x=1&x=3", "/wfx1x2x3"},
		{"/wz?aB=1&aa=2", "/wzaB1aa2"},
		// Values are form-decoded once; keys and the path are not.
		{"/w0?x=a+b", "/w0xa b"},
		{"/w1?x=a%2Bb", "/w1xa+b"},
		{"/w2?x=%2520", "/w2x%20"},
		{"/w3?x=%zz", "/w3x%zz"},
		{"/wb?x=%E4%B8%AD", "/wbx中"},
		{"/u4?a=b%26c", "/u4ab&c"},
		{"/u0?a%20b=1", "/u0a%20b1"},
		{"/va%61?x=1", "/va%61x1"},
		// Empty keys are skipped; trailing segments without '=' are dropped.
		{"/w6?a=1&=2&b=3", "/w6a1b3"},
		{"/w7?=1&a=2", "/w7a2"},
		{"/wh?x=1&y", "/whx1"},
		{"/p3?a=1&", "/p3a1"},
		{"/p5?a", "/p5"},
		// Separators inside a value survive.
		{"/ve?x=1;y=2", "/vex1;y=2"},
	}
	for _, testCase := range cases {
		got, err := CanonicalizeSigningTarget([]byte(testCase.target))
		if err != nil {
			t.Fatalf("CanonicalizeSigningTarget(%q) failed: %v", testCase.target, err)
		}
		if string(got) != testCase.want {
			t.Errorf("CanonicalizeSigningTarget(%q) = %q, want %q", testCase.target, got, testCase.want)
		}
	}
}

func TestCanonicalizeRejectsMiddleSegmentWithoutEquals(t *testing.T) {
	for _, target := range []string{"/api?&&k=v", "/api?k=v&&x=1", "/t1?x&y=1", "/tf?&x=1", "/ta?x&&a=1"} {
		if _, err := CanonicalizeSigningTarget([]byte(target)); err == nil {
			t.Errorf("CanonicalizeSigningTarget(%q) accepted a malformed target", target)
		}
	}
}

func TestBuildSignature(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	const ms = 1792980518965

	plain, err := BuildSignature(key, []byte("/api/l/v6/currentTimeMillis"), ms)
	if err != nil {
		t.Fatalf("BuildSignature: %v", err)
	}
	const wantPlain = "16a848f1f11013f702adb5b95c579dc36ba39c" +
		"s39209ab3ef1e83c6dd4t50cc0a794a"
	if plain != wantPlain {
		t.Errorf("plain path signature = %q, want %q", plain, wantPlain)
	}

	sorted, err := BuildSignature(key, []byte("/wq?a=z&ab=1"), ms)
	if err != nil {
		t.Fatalf("BuildSignature: %v", err)
	}
	const wantSorted = "dcf081d013ce1f37fa883596a048969f7ab09a" +
		"s79a3b669c61178cfe50t3906c13155"
	if sorted != wantSorted {
		t.Errorf("sorted target signature = %q, want %q", sorted, wantSorted)
	}
}

func TestSignatureInsertionPositions(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")
	signature, err := BuildSignature(key, []byte("/api/l/v6/currentTimeMillis"), 1792980518965)
	if err != nil {
		t.Fatalf("BuildSignature: %v", err)
	}
	if len(signature) != 69 {
		t.Fatalf("signature length = %d, want 69", len(signature))
	}
	for _, expected := range []struct {
		index int
		char  byte
	}{{12, '1'}, {28, '9'}, {36, '9'}, {38, 's'}, {58, 't'}} {
		if signature[expected.index] != expected.char {
			t.Errorf("signature[%d] = %q, want %q", expected.index, signature[expected.index], expected.char)
		}
	}
}

// newTestSecret builds a SECRET blob in the native layout around a synthetic
// 32-byte signing key, so the signer can be exercised end to end.
func newTestSecret(t *testing.T) string {
	t.Helper()
	key := []byte("0123456789abcdef0123456789abcdef")
	iv := bytes.Repeat([]byte{0x5a}, 16)
	block, err := aes.NewCipher(secretBlobKey)
	if err != nil {
		t.Fatalf("aes.NewCipher: %v", err)
	}
	ciphertext := make([]byte, len(key))
	cipher.NewCBCEncrypter(block, iv).CryptBlocks(ciphertext, key)

	blob := make([]byte, 0, 20+len(ciphertext))
	var length [4]byte
	binary.BigEndian.PutUint32(length[:], uint32(20+len(ciphertext)))
	blob = append(blob, length[:]...)
	blob = append(blob, iv...)
	blob = append(blob, ciphertext...)
	return base64.StdEncoding.EncodeToString(blob)
}

func TestDeriveSigningKeyRoundTrip(t *testing.T) {
	secret := newTestSecret(t)
	key, err := DeriveSigningKey(secret)
	if err != nil {
		t.Fatalf("DeriveSigningKey: %v", err)
	}
	if string(key) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("derived key = %q", key)
	}
	if _, err := DeriveSigningKey("not-base64!!"); err == nil {
		t.Fatal("DeriveSigningKey accepted invalid base64")
	}
	if _, err := DeriveSigningKey(base64.StdEncoding.EncodeToString([]byte("short"))); err == nil {
		t.Fatal("DeriveSigningKey accepted a short blob")
	}
}

func TestNativeSignerSignsWithNonceTimestamp(t *testing.T) {
	secret := newTestSecret(t)
	key, err := DeriveSigningKey(secret)
	if err != nil {
		t.Fatalf("DeriveSigningKey: %v", err)
	}
	fixed := int64(1792980518965)
	signer := NativeSigner{Now: func() time.Time { return time.UnixMilli(fixed) }}

	timestamp, signature, err := signer.Sign(secret, "/api/l/v6/currentTimeMillis")
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	if want := strconv.FormatUint(Nonce(fixed), 10); timestamp != want {
		t.Errorf("timestamp = %q, want nonce %q", timestamp, want)
	}
	wantSignature, err := BuildSignature(key, []byte("/api/l/v6/currentTimeMillis"), fixed)
	if err != nil {
		t.Fatalf("BuildSignature: %v", err)
	}
	if signature != wantSignature {
		t.Errorf("signature = %q, want %q", signature, wantSignature)
	}
	if !strings.HasPrefix(signature, "16a848f1f110") {
		t.Errorf("unexpected signature prefix %q", signature)
	}
}
