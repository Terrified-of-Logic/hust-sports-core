package hustrun

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeAPI is a local stand-in for the business server. It verifies the signed
// headers of every request, decrypts the payloads and records the call order.
type fakeAPI struct {
	t *testing.T

	secret string
	aesKey string
	token  string
	key    []byte

	serverTimeMS int64
	sportID      string

	mu             sync.Mutex
	calls          []string
	payload        map[string]any
	precheck       map[string]any
	sensorUploaded bool
	sensorFilename string
	sensorBytes    int
	verified       int
	prejudgmentOK  bool

	savesportsValid string
	prejudgmentFail bool
	sensorFileText  string
}

func newFakeAPI(t *testing.T, secret string, serverTimeMS int64) *fakeAPI {
	t.Helper()
	key, err := DeriveSigningKey(secret)
	if err != nil {
		t.Fatalf("DeriveSigningKey: %v", err)
	}
	return &fakeAPI{
		t:               t,
		secret:          secret,
		aesKey:          secret[:16],
		token:           testToken(t),
		key:             key,
		serverTimeMS:    serverTimeMS,
		sportID:         "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee",
		savesportsValid: "1",
	}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 32<<20))
	if err != nil {
		f.t.Errorf("read body: %v", err)
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	f.mu.Lock()
	f.calls = append(f.calls, r.URL.Path)
	f.mu.Unlock()

	if err := f.verify(r, body); err != nil {
		f.t.Errorf("%s: %v", r.URL.Path, err)
		http.Error(w, "verification failed", http.StatusUnauthorized)
		return
	}

	switch r.URL.Path {
	case pathServerTime:
		writeJSON(w, map[string]any{"code": "0", "data": f.serverTimeMS, "message": ""})
	case pathInSportTime:
		writeJSON(w, map[string]any{"code": "0", "data": "1"})
	case pathSportsID:
		writeJSON(w, map[string]any{"code": "0", "data": f.sportID})
	case pathPrejudgment:
		payload, err := f.decryptForm(body, "jsonsports")
		if err != nil {
			f.t.Errorf("prejudgment payload: %v", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if _, exists := payload["coordinate"]; exists {
			f.t.Error("prejudgment carried the coordinate array")
		}
		f.mu.Lock()
		f.precheck = payload
		f.prejudgmentOK = true
		f.mu.Unlock()
		if f.prejudgmentFail {
			writeJSON(w, map[string]any{"code": "0", "data": []any{
				map[string]any{"isvalid": "0", "content": "距离不符合要求"},
			}})
			return
		}
		writeJSON(w, map[string]any{"code": "0", "data": []any{
			map[string]any{"isvalid": "1", "content": "距离符合要求"},
			map[string]any{"isvalid": "1", "content": "配速符合要求"},
		}})
	case pathSaveSports:
		payload, err := f.decryptForm(body, "jsonsports")
		if err != nil {
			f.t.Errorf("savesports payload: %v", err)
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.payload = payload
		f.mu.Unlock()
		reason := ""
		if f.savesportsValid != "1" {
			reason = "4"
		}
		writeJSON(w, map[string]any{"code": "0", "data": map[string]any{
			"sportId":       payload["sportId"],
			"isValid":       f.savesportsValid,
			"inValidReason": reason,
			"checkStatus":   "0",
			"tip":           "提交成功，待审核",
		}})
	case pathSensorZip:
		f.mu.Lock()
		f.sensorUploaded = true
		f.sensorFilename = r.URL.Query().Get("sensorZipFile")
		f.sensorBytes = len(body)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"code": "0", "data": "ok"})
	case pathSensorFile:
		values, err := url.ParseQuery(string(body))
		if err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		raw, err := base64.StdEncoding.DecodeString(values.Get("sensorFile"))
		if err != nil {
			http.Error(w, "bad sensor payload", http.StatusBadRequest)
			return
		}
		plain, err := DecryptECB(raw, f.aesKey)
		if err != nil {
			http.Error(w, "bad sensor cipher", http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.sensorFileText = string(plain)
		f.mu.Unlock()
		writeJSON(w, map[string]any{"code": "0", "data": "ok"})
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeAPI) decryptForm(body []byte, field string) (map[string]any, error) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return nil, err
	}
	encrypted := values.Get(field)
	if encrypted == "" {
		return nil, fmt.Errorf("missing %s field", field)
	}
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil {
		return nil, fmt.Errorf("base64: %w", err)
	}
	plain, err := DecryptECB(raw, f.aesKey)
	if err != nil {
		return nil, fmt.Errorf("decrypt: %w", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(plain, &payload); err != nil {
		return nil, fmt.Errorf("json: %w", err)
	}
	return payload, nil
}

// verify checks the interceptor headers and recomputes the signature from the
// request the server actually received.
func (f *fakeAPI) verify(r *http.Request, body []byte) error {
	if got := r.Header.Get("cgAuthorization"); got != f.token {
		return fmt.Errorf("authorization header = %q", got)
	}
	if r.Header.Get("imei") == "" {
		return fmt.Errorf("imei header missing")
	}
	if userAgent := r.Header.Get("User-Agent"); !strings.HasPrefix(userAgent, "hzkjapp/1.0.9") {
		return fmt.Errorf("user agent = %q", userAgent)
	}
	v1 := r.Header.Get("v1")
	if len(v1) != 32 {
		return fmt.Errorf("v1 = %q", v1)
	}
	clientHeader := r.Header.Get("client")
	raw, err := base64.StdEncoding.DecodeString(clientHeader)
	if err != nil {
		return fmt.Errorf("client header is not base64: %w", err)
	}
	beacon, err := DecryptECB(raw, f.aesKey)
	if err != nil {
		return fmt.Errorf("client header decrypt: %w", err)
	}
	var beaconFields map[string]string
	if err := json.Unmarshal(beacon, &beaconFields); err != nil {
		return fmt.Errorf("client header is not JSON: %w", err)
	}
	for _, key := range []string{"heap", "root", "t"} {
		if _, exists := beaconFields[key]; !exists {
			return fmt.Errorf("client header missing %s", key)
		}
	}

	target, err := f.signingTarget(r, body)
	if err != nil {
		return err
	}
	timestamp := r.Header.Get("timestamp")
	signature := r.Header.Get("sign")
	if timestamp == "" || signature == "" {
		return fmt.Errorf("signature headers missing")
	}
	now := time.Now().UnixMilli()
	for delta := int64(-10000); delta <= 10000; delta++ {
		ms := now + delta
		if strconv.FormatUint(Nonce(ms), 10) != timestamp {
			continue
		}
		expected, buildErr := BuildSignature(f.key, []byte(target), ms)
		if buildErr != nil {
			return buildErr
		}
		if expected != signature {
			return fmt.Errorf("signature mismatch for %s: got %q want %q", target, signature, expected)
		}
		f.mu.Lock()
		f.verified++
		f.mu.Unlock()
		return nil
	}
	return fmt.Errorf("timestamp %q does not match any native nonce near now", timestamp)
}

func (f *fakeAPI) signingTarget(r *http.Request, body []byte) (string, error) {
	switch r.URL.Path {
	case pathPrejudgment, pathSaveSports, pathSensorFile:
		values, err := url.ParseQuery(string(body))
		if err != nil {
			return "", err
		}
		keys := make([]string, 0, len(values))
		for key := range values {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool { return compareJavaString(keys[i], keys[j]) < 0 })
		parts := make([]string, 0, len(keys))
		for _, key := range keys {
			parts = append(parts, key+"="+values.Get(key))
		}
		return r.URL.Path + "?" + strings.Join(parts, "&"), nil
	default:
		return r.URL.RequestURI(), nil
	}
}

func (f *fakeAPI) snapshot() ([]string, map[string]any, map[string]any, bool, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string{}, f.calls...), f.payload, f.precheck, f.sensorUploaded, f.sensorFilename, f.verified
}

func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	encoded, _ := json.Marshal(payload)
	_, _ = w.Write(encoded)
}

func testToken(t *testing.T) string {
	t.Helper()
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":4102444800,"xh":"U000000000"}`))
	return "header." + payload + ".signature"
}

// writeAuthFixture creates a config.yaml plus the SharedPreferences files the
// loader expects.
func writeAuthFixture(t *testing.T, secret, serverURL string) string {
	t.Helper()
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "HEADER.xml"), `<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<map>
    <string name="TOKEN">`+testToken(t)+`</string>
    <string name="SECRET">`+secret+`</string>
    <string name="LOGIN_OUT_TIME">2026-12-01 00:00:00</string>
</map>`)
	writeFile(t, filepath.Join(dir, "USERINFO.xml"), `<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<map>
    <string name="USERXH">U000000000</string>
    <string name="USERNAME">测试学生</string>
    <string name="USER_ROLES">1</string>
</map>`)
	writeFile(t, filepath.Join(dir, "SCHOOL.xml"), `<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<map>
    <string name="SERVERURL">`+serverURL+`</string>
</map>`)
	writeFile(t, filepath.Join(dir, "config.yaml"), `paths:
  header_xml: "HEADER.xml"
  userinfo_xml: "USERINFO.xml"
  school_xml: "SCHOOL.xml"
upload:
  aes_encoding: "base64"
  token_header: "cgAuthorization"
  skip_sensor: false
device:
  imei: "00000000-0000-0000-0000-000000000000"
  user_agent: "hzkjapp/1.0.9(Linux;Android 15;OnePlus/TP1A.220905.001)"
  phone_version: "PJE110,35,15|1.0.9"
`)
	return filepath.Join(dir, "config.yaml")
}

func fixedServerTime() int64 {
	return time.Date(2026, 10, 25, 10, 0, 0, 0, ServerLocation).UnixMilli()
}

func TestUploadRouteFastFlow(t *testing.T) {
	secret := newTestSecret(t)
	api := newFakeAPI(t, secret, fixedServerTime())
	server := httptest.NewServer(api)
	defer server.Close()

	configPath := writeAuthFixture(t, secret, server.URL)
	seed := int64(9)
	options := &UploadOptions{
		RoutePath:       "testdata/route_track.json",
		TargetDistanceM: 120,
		Seed:            &seed,
		Fast:            true,
	}
	var lines []string
	flow := UploadRoute(context.Background(), configPath, options, func(message string) {
		lines = append(lines, message)
	})
	if !flow.Result.Success {
		t.Fatalf("upload failed: %s / %v", flow.Result.RawResponse, flow.Notes)
	}
	if flow.Result.IsValid != "1" {
		t.Errorf("isValid = %q", flow.Result.IsValid)
	}
	if !flow.Result.SensorZipOK {
		t.Errorf("sensor zip was not uploaded: %v", flow.Notes)
	}
	if flow.ServerSportID != api.sportID {
		t.Errorf("sport id = %q, want %q", flow.ServerSportID, api.sportID)
	}
	if flow.SportIDIssuedAtMS == nil {
		t.Fatal("issue time missing")
	}
	if flow.Record == nil {
		t.Fatal("record missing from the report")
	}
	// Fast mode moves the whole timeline before the claim.
	if flow.Record.StartDateLong >= *flow.SportIDIssuedAtMS {
		t.Errorf("record start %d is not before the claim %d", flow.Record.StartDateLong, *flow.SportIDIssuedAtMS)
	}
	if flow.Record.EndDateLong > fixedServerTime() {
		t.Errorf("record ends after the server clock: %d", flow.Record.EndDateLong)
	}
	if len(lines) == 0 {
		t.Error("no progress output")
	}

	calls, payload, precheck, sensorUploaded, sensorFilename, verified := api.snapshot()
	if payload == nil {
		t.Fatal("no savesports payload received")
	}
	if payload["sportId"] != api.sportID {
		t.Errorf("payload sportId = %v", payload["sportId"])
	}
	if precheck == nil {
		t.Error("prejudgment was never called")
	}
	if !sensorUploaded || sensorFilename != api.sportID+".txt" {
		t.Errorf("sensor upload = %t / %q", sensorUploaded, sensorFilename)
	}
	if verified < 5 {
		t.Errorf("only %d requests carried a verifiable signature", verified)
	}

	expected := []string{
		pathServerTime, pathInSportTime, pathSportsID,
		pathPrejudgment, pathSaveSports, pathSensorZip,
	}
	position := 0
	for _, path := range expected {
		found := false
		for ; position < len(calls); position++ {
			if calls[position] == path {
				found = true
				position++
				break
			}
		}
		if !found {
			t.Fatalf("call %s missing from %v", path, calls)
		}
	}

	coordinate, ok := payload["coordinate"].([]any)
	if !ok || len(coordinate) < 20 {
		t.Fatalf("coordinate array = %v", payload["coordinate"])
	}
	last := coordinate[len(coordinate)-1].(map[string]any)
	if last["da"].(float64) < 100 {
		t.Errorf("final da = %v", last["da"])
	}
}

func TestPrepareThenSubmitSession(t *testing.T) {
	secret := newTestSecret(t)
	now := time.Now().UnixMilli()
	api := newFakeAPI(t, secret, now)
	server := httptest.NewServer(api)
	defer server.Close()

	configPath := writeAuthFixture(t, secret, server.URL)
	sessionDir := t.TempDir()
	originalDir := SessionDir
	SessionDir = sessionDir
	defer func() { SessionDir = originalDir }()

	seed := int64(7)
	options := &UploadOptions{
		RoutePath:       "testdata/route_track.json",
		TargetDistanceM: 120,
		Seed:            &seed,
	}
	prepared := PrepareRoute(context.Background(), configPath, options, nil)
	if prepared.SessionPath == "" {
		t.Fatalf("prepare failed: %s %v", prepared.Result.RawResponse, prepared.Notes)
	}
	if prepared.Record == nil {
		t.Fatal("prepare returned no record")
	}
	if _, _, payload, _, _, _ := api.snapshot(); payload != nil {
		t.Error("prepare submitted the record")
	}

	record, session, err := LoadSession(prepared.SessionPath)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if session.SportID != api.sportID {
		t.Errorf("session sport id = %q", session.SportID)
	}
	if record.StartDateLong <= *prepared.SportIDIssuedAtMS {
		t.Errorf("record start %d is not after the claim %d", record.StartDateLong, *prepared.SportIDIssuedAtMS)
	}
	if len(record.Points) < 10 {
		t.Errorf("points = %d", len(record.Points))
	}

	// A record that has not finished yet must be refused without --wait.
	blocked := SubmitSession(context.Background(), configPath, prepared.SessionPath, SubmitOptions{NoWait: true}, nil)
	if blocked.Result.Success {
		t.Error("submit accepted a record that has not finished")
	}
	if !containsString(blocked.Notes, "not_finished") {
		t.Errorf("notes = %v", blocked.Notes)
	}

	// Now hand it a record that already ended relative to the server clock.
	endedAt := fixedServerTime()
	ended := generateRecordAt(t, endedAt-300000, 120, 11)
	if _, err := SaveSession(filepath.Join(sessionDir, "ended.json"), "aaaaaaaa-bbbb-4ccc-8ddd-ffffffffffff",
		endedAt-310000, ended, options); err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	api.mu.Lock()
	api.serverTimeMS = endedAt
	api.mu.Unlock()

	submitted := SubmitSession(context.Background(), configPath, filepath.Join(sessionDir, "ended.json"),
		SubmitOptions{NoWait: true}, nil)
	if !submitted.Result.Success {
		t.Fatalf("submit failed: %s %v", submitted.Result.RawResponse, submitted.Notes)
	}
	if submitted.ServerSportID != "aaaaaaaa-bbbb-4ccc-8ddd-ffffffffffff" {
		t.Errorf("submit reused a different sport id: %q", submitted.ServerSportID)
	}
	_, payload, _, _, _, _ := api.snapshot()
	if payload == nil {
		t.Fatal("no payload submitted")
	}
	if payload["sportId"] != "aaaaaaaa-bbbb-4ccc-8ddd-ffffffffffff" {
		t.Errorf("payload sportId = %v", payload["sportId"])
	}
	if payload["xh"] != "U000000000" {
		t.Errorf("payload xh = %v, want the current login", payload["xh"])
	}
}

func generateRecordAt(t *testing.T, startMS int64, distance float64, seed int64) SportRecord {
	t.Helper()
	route, err := LoadRoute("testdata/route_track.json")
	if err != nil {
		t.Fatalf("LoadRoute: %v", err)
	}
	record, err := GenerateRouteRecord(route, GenerateOptions{
		StartMS:         startMS,
		SportID:         "aaaaaaaa-bbbb-4ccc-8ddd-ffffffffffff",
		XH:              "U000000000",
		Name:            "测试学生",
		TargetDistanceM: distance,
		Seed:            &seed,
	})
	if err != nil {
		t.Fatalf("GenerateRouteRecord: %v", err)
	}
	return record
}

func TestSubmitReportsInvalidVerdict(t *testing.T) {
	secret := newTestSecret(t)
	api := newFakeAPI(t, secret, fixedServerTime())
	api.savesportsValid = "0"
	server := httptest.NewServer(api)
	defer server.Close()

	configPath := writeAuthFixture(t, secret, server.URL)
	sessionDir := t.TempDir()
	originalDir := SessionDir
	SessionDir = sessionDir
	defer func() { SessionDir = originalDir }()

	record := generateRecordAt(t, fixedServerTime()-300000, 120, 21)
	seed := int64(21)
	path, err := SaveSession(filepath.Join(sessionDir, "invalid.json"), record.SportID,
		record.StartDateLong-5000, record, &UploadOptions{
			RoutePath:       "testdata/route_track.json",
			TargetDistanceM: 120,
			Seed:            &seed,
		})
	if err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	flow := SubmitSession(context.Background(), configPath, path,
		SubmitOptions{NoWait: true, SkipSensor: true}, nil)
	if flow.Result.Success != true {
		t.Fatalf("request failed instead of reporting a verdict: %s", flow.Result.RawResponse)
	}
	if flow.Result.IsValid != "0" || flow.Result.InValidReason != "4" {
		t.Errorf("verdict = %q reason = %q", flow.Result.IsValid, flow.Result.InValidReason)
	}
	if flow.Result.SensorZipOK {
		t.Error("sensor data must not be uploaded for an invalid verdict")
	}
}

func TestSubmitAbortsWhenPrejudgmentFails(t *testing.T) {
	secret := newTestSecret(t)
	api := newFakeAPI(t, secret, fixedServerTime())
	api.prejudgmentFail = true
	server := httptest.NewServer(api)
	defer server.Close()

	configPath := writeAuthFixture(t, secret, server.URL)
	sessionDir := t.TempDir()
	originalDir := SessionDir
	SessionDir = sessionDir
	defer func() { SessionDir = originalDir }()

	record := generateRecordAt(t, fixedServerTime()-300000, 120, 22)
	path, err := SaveSession(filepath.Join(sessionDir, "precheck.json"), record.SportID,
		record.StartDateLong-5000, record, &UploadOptions{RoutePath: "testdata/route_track.json"})
	if err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	flow := SubmitSession(context.Background(), configPath, path, SubmitOptions{NoWait: true}, nil)
	if flow.Result.Success {
		t.Error("submit continued after a failed prejudgment")
	}
	if !strings.Contains(flow.Result.RawResponse, "距离不符合要求") {
		t.Errorf("message = %q", flow.Result.RawResponse)
	}
	calls, _, _, _, _, _ := api.snapshot()
	for _, call := range calls {
		if call == pathSaveSports {
			t.Error("savesports must not be called after a failed prejudgment")
		}
	}
}

func TestUploadSensorFileUsesEncryptedForm(t *testing.T) {
	secret := newTestSecret(t)
	api := newFakeAPI(t, secret, fixedServerTime())
	server := httptest.NewServer(api)
	defer server.Close()

	configPath := writeAuthFixture(t, secret, server.URL)
	auth, err := LoadAuthConfig(configPath)
	if err != nil {
		t.Fatalf("LoadAuthConfig: %v", err)
	}
	client, err := NewClient(auth, NativeSigner{}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if !client.UploadSensorFile(context.Background(), "sport-id", "[9.8,9.81]") {
		t.Fatal("sensor file upload failed")
	}
	api.mu.Lock()
	text := api.sensorFileText
	api.mu.Unlock()
	if text != "[9.8,9.81]" {
		t.Errorf("decrypted sensor text = %q", text)
	}
}

func TestDryRunStaysOffline(t *testing.T) {
	secret := newTestSecret(t)
	api := newFakeAPI(t, secret, fixedServerTime())
	server := httptest.NewServer(api)
	defer server.Close()

	configPath := writeAuthFixture(t, secret, server.URL)
	outPath := filepath.Join(t.TempDir(), "payload.json")
	seed := int64(3)
	options := &UploadOptions{
		RoutePath:       "testdata/route_track.json",
		TargetDistanceM: 600,
		Seed:            &seed,
		DryRun:          true,
		OutputPayload:   outPath,
	}
	flow := UploadRoute(context.Background(), configPath, options, nil)
	if flow.Record == nil {
		t.Fatalf("dry run produced no record: %s", flow.Result.RawResponse)
	}
	if calls, _, _, _, _, _ := api.snapshot(); len(calls) != 0 {
		t.Errorf("dry run hit the network: %v", calls)
	}
	raw, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("payload dump missing: %v", err)
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		t.Fatalf("payload dump is not JSON: %v", err)
	}
	if payload["sportId"] == "" {
		t.Error("dumped payload has no sport id")
	}
}

func TestCheckSessionReportsServerTime(t *testing.T) {
	secret := newTestSecret(t)
	api := newFakeAPI(t, secret, fixedServerTime())
	server := httptest.NewServer(api)
	defer server.Close()
	configPath := writeAuthFixture(t, secret, server.URL)

	report := CheckSession(context.Background(), configPath, nil)
	if !report.OK {
		t.Fatalf("check failed: %s", report.Message)
	}
	if report.ServerTime == nil || *report.ServerTime != fixedServerTime() {
		t.Errorf("server time = %v", report.ServerTime)
	}
	if report.InSportTime == nil || !*report.InSportTime {
		t.Errorf("in sport time = %v", report.InSportTime)
	}
	if !report.TokenPresent || !report.SecretPresent || report.TokenExpired {
		t.Errorf("credentials = %+v", report)
	}
}

func TestSessionRoundTripAndLatest(t *testing.T) {
	sessionDir := t.TempDir()
	originalDir := SessionDir
	SessionDir = sessionDir
	defer func() { SessionDir = originalDir }()

	record := generateRecordAt(t, fixedServerTime()-600000, 1200, 12)
	seed := int64(12)
	options := &UploadOptions{
		RoutePath:       "testdata/route_track.json",
		TargetDistanceM: 1200,
		Seed:            &seed,
		SportType:       "3",
		RouteID:         "7",
		RouteName:       "跑步-男生",
	}
	path, err := SaveSession(filepath.Join(sessionDir, "session.json"), record.SportID,
		record.StartDateLong-5000, record, options)
	if err != nil {
		t.Fatalf("SaveSession: %v", err)
	}
	loaded, session, err := LoadSession(path)
	if err != nil {
		t.Fatalf("LoadSession: %v", err)
	}
	if session.SportID != record.SportID || session.TargetDistanceM != 1200 {
		t.Errorf("session = %+v", session)
	}
	if loaded.StartDateLong != record.StartDateLong || loaded.EndDateLong != record.EndDateLong {
		t.Errorf("timestamps changed on round trip")
	}
	if len(loaded.Points) != len(record.Points) {
		t.Fatalf("points = %d, want %d", len(loaded.Points), len(record.Points))
	}
	if loaded.Points[0].T != record.Points[0].T || loaded.Points[len(loaded.Points)-1].DA != record.Points[len(record.Points)-1].DA {
		t.Error("point payload changed on round trip")
	}
	if len(loaded.MinuteSpeed) != len(record.MinuteSpeed) {
		t.Error("minute speed rows changed on round trip")
	}

	latest, err := LatestSession()
	if err != nil {
		t.Fatalf("LatestSession: %v", err)
	}
	if latest != path {
		t.Errorf("latest = %q, want %q", latest, path)
	}

	brokenPath := filepath.Join(sessionDir, "broken.json")
	writeFile(t, brokenPath, `{"version": 99}`)
	if _, _, err := LoadSession(brokenPath); err == nil {
		t.Error("a mismatched session version was accepted")
	}
}

func TestBuildHeadersCarriesSignatureAndFingerprint(t *testing.T) {
	secret := newTestSecret(t)
	auth := &AuthConfig{
		Token:        "jwt.claims.signature",
		Secret:       secret,
		XH:           "U000000000",
		ServerURL:    "https://example.invalid/cgapp-server",
		IMEI:         "00000000-0000-0000-0000-000000000000",
		UserAgent:    "hzkjapp/1.0.9",
		V1Seed:       "seed",
		PhoneVersion: "PJE110,35,15|1.0.9",
	}
	client, err := NewClient(auth, NativeSigner{}, nil)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	headers, err := client.BuildHeaders(pathServerTime, pathServerTime)
	if err != nil {
		t.Fatalf("BuildHeaders: %v", err)
	}
	if len(headers["sign"][0]) != 69 {
		t.Errorf("sign = %q", headers["sign"][0])
	}
	if headers["timestamp"][0] == "" {
		t.Error("timestamp missing")
	}
	if headers["Content-Type"][0] != "application/json;charset=UTF-8" {
		t.Errorf("content type = %q", headers["Content-Type"][0])
	}
	if headers["Accept-Encoding"][0] != "gzip" || headers["Connection"][0] != "Keep-Alive" {
		t.Error("wire-level headers missing")
	}

	var form FormValues
	form.Add("jsonsports", "cipher")
	formHeaders, err := client.BuildFormHeaders(pathSaveSports, form)
	if err != nil {
		t.Fatalf("BuildFormHeaders: %v", err)
	}
	if formHeaders["Content-Type"][0] != "application/x-www-form-urlencoded" {
		t.Errorf("form content type = %q", formHeaders["Content-Type"][0])
	}
	if formHeaders["sign"][0] == headers["sign"][0] {
		t.Error("the form signature must cover the fields")
	}
}

func TestBuildFormSigningTargetSortsLikeJava(t *testing.T) {
	var form FormValues
	form.Add("b", "2")
	form.Add("a", "1")
	form.Add("A", "0")
	target := BuildFormSigningTarget("/api/l/v7/savesports", form)
	if target != "/api/l/v7/savesports?A=0&a=1&b=2" {
		t.Errorf("target = %q", target)
	}
	if got := BuildFormSigningTarget("/api/l/v7/savesports", FormValues{}); got != "/api/l/v7/savesports" {
		t.Errorf("empty target = %q", got)
	}
}

func TestNewClientRejectsIncompleteAuth(t *testing.T) {
	if _, err := NewClient(nil, NativeSigner{}, nil); err == nil {
		t.Error("nil auth accepted")
	}
	auth := &AuthConfig{Token: "t", Secret: "0123456789abcdef", ServerURL: "https://example.invalid"}
	if _, err := NewClient(auth, NativeSigner{}, nil); err == nil {
		t.Error("missing device fingerprint accepted")
	}
	auth.IMEI = "imei"
	auth.UserAgent = "ua"
	auth.TokenHeader = "wrong"
	if _, err := NewClient(auth, NativeSigner{}, nil); err == nil {
		t.Error("wrong token header accepted")
	}
	auth.TokenHeader = "cgAuthorization"
	if _, err := NewClient(auth, nil, nil); err == nil {
		t.Error("missing signer accepted")
	}
	if _, err := NewClient(auth, NativeSigner{}, nil); err != nil {
		t.Errorf("valid auth rejected: %v", err)
	}
}

func TestEncodeQueryPreservesOrder(t *testing.T) {
	pairs := []QueryPair{{Key: "sportId", Value: "id"}, {Key: "sensorZipFile", Value: "id.txt"}}
	if got := EncodeQuery(pairs); got != "sportId=id&sensorZipFile=id.txt" {
		t.Errorf("query = %q", got)
	}
	if got := BuildURLSigningTarget(pathSensorZip, pairs); got != pathSensorZip+"?sportId=id&sensorZipFile=id.txt" {
		t.Errorf("target = %q", got)
	}
	if got := BuildURLSigningTarget(pathSportsID, nil); got != pathSportsID {
		t.Errorf("target = %q", got)
	}
}

func TestUploadRouteReportsMissingCredentials(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeFile(t, configPath, "paths:\n  header_xml: \"missing.xml\"\n")
	flow := UploadRoute(context.Background(), configPath, &UploadOptions{
		RoutePath:       "testdata/route_track.json",
		TargetDistanceM: 120,
	}, nil)
	if flow.Result.Success {
		t.Error("upload without credentials succeeded")
	}
	if !strings.Contains(flow.Result.RawResponse, "凭证") {
		t.Errorf("message = %q", flow.Result.RawResponse)
	}
}

func TestSubmitSessionWithoutSession(t *testing.T) {
	originalDir := SessionDir
	SessionDir = t.TempDir()
	defer func() { SessionDir = originalDir }()
	flow := SubmitSession(context.Background(), "config.yaml", "", SubmitOptions{NoWait: true}, nil)
	if flow.Result.Success {
		t.Error("submit without a session succeeded")
	}
	if !strings.Contains(flow.Result.RawResponse, "prepare") {
		t.Errorf("message = %q", flow.Result.RawResponse)
	}
}
