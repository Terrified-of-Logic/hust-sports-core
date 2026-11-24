package hustrun

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrepareSensorZipUpload(t *testing.T) {
	sensor := `[9.8,9.9,10.1`
	filename, payload, err := PrepareSensorZipUpload(sensor, "00000000-0000-4000-8000-000000000001")
	if err != nil {
		t.Fatalf("PrepareSensorZipUpload: %v", err)
	}
	if filename != "00000000-0000-4000-8000-000000000001.txt" {
		t.Errorf("filename = %q", filename)
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(string(payload), "\n", ""))
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if len(decoded) < 2 || decoded[0] != 0x1f || decoded[1] != 0x8b {
		t.Fatalf("payload is not gzip data: %x", decoded[:2])
	}
	if decoded[3] != 0 || decoded[2] != 8 {
		t.Errorf("gzip header = % x", decoded[:4])
	}
	// The Android writer leaves mtime zero and OS=255.
	for index, expected := range map[int]byte{4: 0, 5: 0, 6: 0, 7: 0, 9: 255} {
		if decoded[index] != expected {
			t.Errorf("gzip header byte %d = %d, want %d", index, decoded[index], expected)
		}
	}

	reader, err := gzip.NewReader(bytes.NewReader(decoded))
	if err != nil {
		t.Fatalf("gzip.NewReader: %v", err)
	}
	text, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(text) != `[9.8,9.9,10.1]` {
		t.Errorf("decompressed = %q", text)
	}
	for _, line := range strings.Split(strings.TrimRight(string(payload), "\n"), "\n") {
		if len(line) > 76 {
			t.Errorf("base64 line longer than 76 characters: %d", len(line))
		}
	}
	if !strings.HasSuffix(string(payload), "\n") {
		t.Error("base64 payload must end with a newline")
	}
}

func TestPrepareSensorFileUpload(t *testing.T) {
	if got := PrepareSensorFileUpload("  [1,2]  "); got != "[1,2]" {
		t.Errorf("closed text = %q", got)
	}
	if got := PrepareSensorFileUpload("[1,2"); got != "[1,2]" {
		t.Errorf("open text = %q", got)
	}
}

func TestLoadReferenceSensorHonoursChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sensor.txt")
	content := "[9.8,9.81,9.79]"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	sum := sha256.Sum256([]byte(content))
	digest := hex.EncodeToString(sum[:])

	if text, ok := LoadReferenceSensor(path, "", digest); !ok || text != content {
		t.Errorf("matching checksum rejected: %q %t", text, ok)
	}
	if _, ok := LoadReferenceSensor(path, "", strings.Repeat("0", 64)); ok {
		t.Error("mismatched checksum accepted")
	}
	if _, ok := LoadReferenceSensor(filepath.Join(dir, "missing.txt"), "", ""); ok {
		t.Error("missing file accepted")
	}

	badPath := filepath.Join(dir, "bad.txt")
	if err := os.WriteFile(badPath, []byte("not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, ok := LoadReferenceSensor(badPath, "", ""); ok {
		t.Error("non-JSON sensor file accepted")
	}
}

func TestAnalyzeSensorTrace(t *testing.T) {
	stats, err := AnalyzeSensorTrace("[1,2,3,4,5]")
	if err != nil {
		t.Fatalf("AnalyzeSensorTrace: %v", err)
	}
	if stats["count"] != 5 || stats["mean"] != 3 || stats["min"] != 1 || stats["max"] != 5 || stats["median"] != 3 {
		t.Errorf("stats = %v", stats)
	}
	if _, err := AnalyzeSensorTrace("nope"); err == nil {
		t.Error("non-array accepted")
	}
	if _, err := AnalyzeSensorTrace("[]"); err == nil {
		t.Error("empty array accepted")
	}
}

func TestSensorTraceIsValidJSON(t *testing.T) {
	trace := MakeSensorTrace(SensorTraceOptions{DurationSeconds: 5, Seed: int64Ptr(9)})
	var values []float64
	if err := json.Unmarshal([]byte(trace), &values); err != nil {
		t.Fatalf("trace is not JSON: %v", err)
	}
	if len(values) != 50 {
		t.Errorf("values = %d, want 50", len(values))
	}
}
