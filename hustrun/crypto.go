package hustrun

import (
	"bytes"
	"crypto/aes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// EncryptECB encrypts plaintext with AES/ECB/PKCS#7 using the first 16 bytes of
// key, mirroring the client's BackAES(str, key, 0).
func EncryptECB(plaintext, key string) ([]byte, error) {
	keyBytes := []byte(key)
	if len(keyBytes) < 16 {
		return nil, fmt.Errorf("%w: AES key must provide 16 bytes", ErrConfig)
	}
	block, err := aes.NewCipher(keyBytes[:16])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	padded := pkcs7Pad([]byte(plaintext), aes.BlockSize)
	out := make([]byte, len(padded))
	for offset := 0; offset < len(padded); offset += aes.BlockSize {
		block.Encrypt(out[offset:offset+aes.BlockSize], padded[offset:offset+aes.BlockSize])
	}
	return out, nil
}

// DecryptECB reverses EncryptECB; it is exported because the tests and the
// protocol tools need to inspect payloads.
func DecryptECB(ciphertext []byte, key string) ([]byte, error) {
	keyBytes := []byte(key)
	if len(keyBytes) < 16 {
		return nil, fmt.Errorf("%w: AES key must provide 16 bytes", ErrConfig)
	}
	if len(ciphertext) == 0 || len(ciphertext)%aes.BlockSize != 0 {
		return nil, fmt.Errorf("%w: ciphertext is not block aligned", ErrProtocol)
	}
	block, err := aes.NewCipher(keyBytes[:16])
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	out := make([]byte, len(ciphertext))
	for offset := 0; offset < len(ciphertext); offset += aes.BlockSize {
		block.Decrypt(out[offset:offset+aes.BlockSize], ciphertext[offset:offset+aes.BlockSize])
	}
	return pkcs7Unpad(out, aes.BlockSize)
}

// EncryptForUpload AES-encrypts a JSON string and encodes it as configured
// ("base64" or "hex"), which is what the jsonsports form field carries.
func EncryptForUpload(jsonText, key, encoding string) (string, error) {
	encrypted, err := EncryptECB(jsonText, key)
	if err != nil {
		return "", err
	}
	switch strings.ToLower(strings.TrimSpace(encoding)) {
	case "", "base64":
		return base64.StdEncoding.EncodeToString(encrypted), nil
	case "hex":
		return hex.EncodeToString(encrypted), nil
	default:
		return "", fmt.Errorf("%w: unknown AES output encoding %q", ErrConfig, encoding)
	}
}

// EncryptClientHeader builds the anti-cheat "client" header: the compact JSON
// beacon {heap, root, t} under the same AES key.
func EncryptClientHeader(key string, isRoot bool, encoding string, now time.Time) (string, error) {
	root := "0"
	if isRoot {
		root = "1"
	}
	payload := `{"heap":"0","root":"` + root + `","t":"` +
		strconv.FormatInt(now.UnixMilli(), 10) + `"}`
	return EncryptForUpload(payload, key, encoding)
}

// EncryptSensorData encrypts the sensor text for the sensorFile endpoint.
func EncryptSensorData(sensorText, key, encoding string) (string, error) {
	return EncryptForUpload(sensorText, key, encoding)
}

// V1Header derives the "v1" header: MD5 of the configured seed, or of the
// current millisecond clock when no seed is configured.
func V1Header(seed string, now time.Time) string {
	material := seed
	if strings.TrimSpace(material) == "" {
		material = strconv.FormatInt(now.UnixMilli(), 10)
	}
	sum := md5.Sum([]byte(material))
	return hex.EncodeToString(sum[:])
}

func pkcs7Pad(data []byte, blockSize int) []byte {
	padding := blockSize - len(data)%blockSize
	return append(append([]byte{}, data...), bytes.Repeat([]byte{byte(padding)}, padding)...)
}

func pkcs7Unpad(data []byte, blockSize int) ([]byte, error) {
	if len(data) == 0 || len(data)%blockSize != 0 {
		return nil, fmt.Errorf("%w: invalid padded length", ErrProtocol)
	}
	padding := int(data[len(data)-1])
	if padding == 0 || padding > blockSize || padding > len(data) {
		return nil, fmt.Errorf("%w: invalid PKCS#7 padding", ErrProtocol)
	}
	for _, value := range data[len(data)-padding:] {
		if int(value) != padding {
			return nil, fmt.Errorf("%w: invalid PKCS#7 padding", ErrProtocol)
		}
	}
	return data[:len(data)-padding], nil
}
