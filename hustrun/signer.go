package hustrun

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"
)

// secretBlobKey is the AES-256 key that unwraps SECRET. It is a constant of the
// native client (recovered from its constant pool) and only ever decrypts the
// SECRET blob; it is not itself a credential.
var secretBlobKey = func() []byte {
	raw, err := hex.DecodeString("ad8dad2869c5e017303e5f48858db3129bf4d07a47d85beaaaa9d26f436d232a")
	if err != nil {
		panic("hustrun: invalid secret blob key: " + err.Error())
	}
	return raw
}()

// Signer produces the (timestamp, sign) header pair for one request.
type Signer interface {
	// Sign returns the native timestamp nonce and the 69-character signature
	// for the canonical signing target.
	Sign(secret, signingTarget string) (timestamp, sign string, err error)
}

// NativeSigner reproduces the official native signer in portable Go: no
// external process, no shared object, no JVM.
type NativeSigner struct {
	// Now overrides the clock in tests. A nil value uses time.Now.
	Now func() time.Time
}

// Sign returns the nonce and signature for the target.
func (s NativeSigner) Sign(secret, signingTarget string) (string, string, error) {
	key, err := DeriveSigningKey(secret)
	if err != nil {
		return "", "", err
	}
	now := time.Now
	if s.Now != nil {
		now = s.Now
	}
	ms := now().UnixMilli()
	signature, err := BuildSignature(key, []byte(signingTarget), ms)
	if err != nil {
		return "", "", err
	}
	return fmt.Sprint(Nonce(ms)), signature, nil
}

// DeriveSigningKey unwraps SECRET into the 32-byte key the native signer uses.
// SECRET is base64(4-byte big-endian total length | 16-byte IV | ciphertext) and
// the key is the AES-256-CBC decryption of the ciphertext.
func DeriveSigningKey(secret string) ([]byte, error) {
	secret = strings.TrimSpace(secret)
	if secret == "" {
		return nil, fmt.Errorf("%w: SECRET is empty", ErrSigner)
	}
	blob, err := base64.StdEncoding.DecodeString(secret)
	if err != nil {
		return nil, fmt.Errorf("%w: SECRET is not valid base64", ErrSigner)
	}
	if len(blob) < 52 {
		return nil, fmt.Errorf("%w: SECRET is too short for the native layout", ErrSigner)
	}
	declared := int(binary.BigEndian.Uint32(blob[:4]))
	end := len(blob)
	if declared > 20 && declared <= len(blob) {
		end = declared
	}
	ciphertext := blob[20:end]
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("%w: SECRET ciphertext is not block aligned", ErrSigner)
	}
	block, err := aes.NewCipher(secretBlobKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrSigner, err)
	}
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, blob[4:20]).CryptBlocks(plain, ciphertext)
	if len(plain) != 32 {
		return nil, fmt.Errorf("%w: SECRET does not decode to a 32-byte signing key", ErrSigner)
	}
	return plain, nil
}

// CanonicalizeSigningTarget reproduces the native normalization of a signing
// target: the path before the first '?' is kept verbatim, the query is split on
// '&', segments without '=' are dropped from the tail (and rejected in the
// middle), empty keys are skipped, values are form-decoded once, and the pairs
// are sorted by (key, value) bytes before concatenation.
func CanonicalizeSigningTarget(target []byte) ([]byte, error) {
	q := bytes.IndexByte(target, '?')
	if q < 0 {
		return target, nil
	}
	path := target[:q]
	query := target[q+1:]
	var segments [][]byte
	if len(query) > 0 {
		segments = bytes.Split(query, []byte("&"))
	}
	for len(segments) > 0 && bytes.IndexByte(segments[len(segments)-1], '=') < 0 {
		segments = segments[:len(segments)-1]
	}
	type pair struct{ key, value []byte }
	pairs := make([]pair, 0, len(segments))
	for _, segment := range segments {
		eq := bytes.IndexByte(segment, '=')
		if eq < 0 {
			return nil, fmt.Errorf("%w: signing target mixes a segment without '='", ErrSigner)
		}
		if eq == 0 {
			continue
		}
		pairs = append(pairs, pair{key: segment[:eq], value: percentDecode(segment[eq+1:])})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		if c := bytes.Compare(pairs[i].key, pairs[j].key); c != 0 {
			return c < 0
		}
		return bytes.Compare(pairs[i].value, pairs[j].value) < 0
	})
	out := make([]byte, 0, len(path)+len(query))
	out = append(out, path...)
	for _, item := range pairs {
		out = append(out, item.key...)
		out = append(out, item.value...)
	}
	return out, nil
}

// BuildSignature reproduces the native digest: SHA-256 over the key, the
// canonical target, the big-endian millisecond clock and the reversed key, with
// the fixed 1/9/9/s/t characters spliced into the hex digest.
func BuildSignature(key []byte, signingTarget []byte, ms int64) (string, error) {
	canonical, err := CanonicalizeSigningTarget(signingTarget)
	if err != nil {
		return "", err
	}
	reversed := make([]byte, len(key))
	for i := range key {
		reversed[len(key)-1-i] = key[i]
	}
	clock := make([]byte, 8)
	binary.BigEndian.PutUint64(clock, uint64(ms))

	hasher := sha256.New()
	hasher.Write(key)
	hasher.Write(canonical)
	hasher.Write(clock)
	hasher.Write(reversed)
	digest := hex.EncodeToString(hasher.Sum(nil))

	var out strings.Builder
	out.Grow(len(digest) + 5)
	out.WriteString(digest[:12])
	out.WriteByte('1')
	out.WriteString(digest[12:27])
	out.WriteByte('9')
	out.WriteString(digest[27:34])
	out.WriteByte('9')
	out.WriteString(digest[34:35])
	out.WriteByte('s')
	out.WriteString(digest[35:54])
	out.WriteByte('t')
	out.WriteString(digest[54:])
	return out.String(), nil
}

func percentDecode(data []byte) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); i++ {
		switch {
		case data[i] == '+':
			out = append(out, ' ')
		case data[i] == '%' && i+2 < len(data):
			high, okHigh := hexValue(data[i+1])
			low, okLow := hexValue(data[i+2])
			if !okHigh || !okLow {
				out = append(out, data[i])
				continue
			}
			out = append(out, high<<4|low)
			i += 2
		default:
			out = append(out, data[i])
		}
	}
	return out
}

func hexValue(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}
