package hustrun

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// LoadReferenceSensor returns the sensor text of a previously captured file.
// When expectedSHA256 is set the file must match it, so a captured blob cannot
// silently change between runs.
func LoadReferenceSensor(path, sportID, expectedSHA256 string) (string, bool) {
	candidates := make([]string, 0, 3)
	if path != "" {
		candidates = append(candidates, path)
	}
	if sportID != "" {
		directory := filepath.Dir(path)
		if path == "" {
			directory = "."
		}
		candidates = append(candidates,
			filepath.Join(directory, "sensor_"+sportID+".txt"),
			filepath.Join(directory, "sensor_"+shortID(sportID)+".txt"),
		)
	}
	for _, candidate := range candidates {
		raw, err := os.ReadFile(candidate)
		if err != nil {
			continue
		}
		if expectedSHA256 != "" {
			sum := sha256.Sum256(raw)
			if !strings.EqualFold(hex.EncodeToString(sum[:]), strings.TrimSpace(expectedSHA256)) {
				continue
			}
		}
		text := strings.TrimSpace(string(raw))
		var values []float64
		if err := json.Unmarshal([]byte(text), &values); err != nil || len(values) == 0 {
			continue
		}
		return text, true
	}
	return "", false
}

func shortID(value string) string {
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

// PrepareSensorZipUpload renders the sensor text the way the client stores it
// before the multipart upload: the JSON array closed, gzip level 6, then base64
// with 76-character lines.
func PrepareSensorZipUpload(sensorJSON, sportID string) (string, []byte, error) {
	text := PrepareSensorFileUpload(sensorJSON)
	compressed, err := gzipPayload([]byte(text))
	if err != nil {
		return "", nil, err
	}
	return sportID + ".txt", base64Lines(compressed), nil
}

// PrepareSensorFileUpload normalizes the sensor text: trimmed and closed.
func PrepareSensorFileUpload(sensorJSON string) string {
	text := strings.TrimSpace(sensorJSON)
	if !strings.HasSuffix(text, "]") {
		text += "]"
	}
	return text
}

// gzipPayload compresses with the parameters Android's GZIPOutputStream uses:
// level 6, no name, zero mtime, OS byte 255.
func gzipPayload(data []byte) ([]byte, error) {
	var buffer bytes.Buffer
	writer, err := gzip.NewWriterLevel(&buffer, 6)
	if err != nil {
		return nil, err
	}
	writer.Header.ModTime = time.Time{}
	writer.Header.OS = 255
	if _, err := writer.Write(data); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

// base64Lines encodes like Python's base64.encodebytes: 76-character lines with
// a trailing newline.
func base64Lines(data []byte) []byte {
	encoded := base64.StdEncoding.EncodeToString(data)
	var out bytes.Buffer
	for len(encoded) > 76 {
		out.WriteString(encoded[:76])
		out.WriteByte('\n')
		encoded = encoded[76:]
	}
	if encoded != "" {
		out.WriteString(encoded)
		out.WriteByte('\n')
	}
	return out.Bytes()
}

// AnalyzeSensorTrace summarizes a sensor series, which is useful for comparing
// a generated trace with a captured one.
func AnalyzeSensorTrace(text string) (map[string]float64, error) {
	var values []float64
	if err := json.Unmarshal([]byte(strings.TrimSpace(text)), &values); err != nil {
		return nil, fmt.Errorf("%w: sensor trace is not a JSON array: %v", ErrInvalidRecord, err)
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("%w: sensor trace is empty", ErrInvalidRecord)
	}
	minimum, maximum := values[0], values[0]
	var sum float64
	for _, value := range values {
		sum += value
		minimum = min(minimum, value)
		maximum = max(maximum, value)
	}
	mean := sum / float64(len(values))
	var variance float64
	for _, value := range values {
		variance += (value - mean) * (value - mean)
	}
	variance /= float64(len(values))
	return map[string]float64{
		"count":  float64(len(values)),
		"mean":   mean,
		"std":    math.Sqrt(variance),
		"min":    minimum,
		"max":    maximum,
		"median": median(values),
	}, nil
}

func median(values []float64) float64 {
	sorted := append([]float64{}, values...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
			sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
		}
	}
	return sorted[len(sorted)/2]
}
