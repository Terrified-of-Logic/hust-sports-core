package hustrun

import (
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestParseSimpleYAML(t *testing.T) {
	content := `
# leading comment
paths:
  header_xml: ".credentials/HEADER.xml"   # trailing comment
  userinfo_xml: '.credentials/USERINFO.xml'
upload:
  aes_encoding: "base64"
  skip_sensor: true
device:
  is_root: false
  v1_seed: ""
  user_agent: "hzkjapp/1.0.9(Linux;Android 15)"
top_level: value
`
	config := YAMLConfig{Top: map[string]string{}, Sections: map[string]map[string]string{}}
	if err := parseSimpleYAML(content, &config); err != nil {
		t.Fatalf("parseSimpleYAML: %v", err)
	}
	if got := config.String("paths", "header_xml", ""); got != ".credentials/HEADER.xml" {
		t.Errorf("header_xml = %q", got)
	}
	if got := config.String("paths", "userinfo_xml", ""); got != ".credentials/USERINFO.xml" {
		t.Errorf("userinfo_xml = %q", got)
	}
	if got := config.String("upload", "aes_encoding", ""); got != "base64" {
		t.Errorf("aes_encoding = %q", got)
	}
	if !config.Bool("upload", "skip_sensor", false) {
		t.Error("skip_sensor was not parsed as true")
	}
	if config.Bool("device", "is_root", true) {
		t.Error("is_root was not parsed as false")
	}
	if got := config.String("device", "user_agent", ""); !strings.HasPrefix(got, "hzkjapp/1.0.9") {
		t.Errorf("user_agent = %q", got)
	}
	if got := config.String("", "top_level", ""); got != "value" {
		t.Errorf("top level value = %q", got)
	}
	if config.String("device", "v1_seed", "fallback") != "fallback" {
		t.Error("empty values must fall back")
	}
}

func TestParseSimpleYAMLRejectsGarbage(t *testing.T) {
	config := YAMLConfig{Top: map[string]string{}, Sections: map[string]map[string]string{}}
	if err := parseSimpleYAML("this line has no colon", &config); err == nil {
		t.Error("garbage line accepted")
	}
}

func TestReadSPXML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "HEADER.xml")
	writeFile(t, path, `<?xml version="1.0" encoding="utf-8" standalone="yes"?>
<map>
    <string name="TOKEN">abc.def.ghi</string>
    <string name="SECRET">c2VjcmV0</string>
    <string name="LOGIN_OUT_TIME">2026-07-01 00:00:00</string>
</map>`)
	values, err := ReadSPXML(path)
	if err != nil {
		t.Fatalf("ReadSPXML: %v", err)
	}
	if values["TOKEN"] != "abc.def.ghi" || values["SECRET"] != "c2VjcmV0" {
		t.Fatalf("values = %v", values)
	}
	if _, err := ReadSPXML(filepath.Join(dir, "missing.xml")); err != nil {
		t.Fatalf("missing file must not be an error: %v", err)
	}
}

func TestLoadAuthConfigMergesYAMLAndXML(t *testing.T) {
	dir := t.TempDir()
	secret := newTestSecret(t)
	writeFile(t, filepath.Join(dir, "HEADER.xml"), `<?xml version="1.0" encoding="utf-8"?>
<map>
    <string name="TOKEN">jwt.claims.signature</string>
    <string name="SECRET">`+secret+`</string>
    <string name="SIGN">old-sign</string>
    <string name="CGAPP_TIME_KEY">old-time</string>
    <string name="WEB_VIEW_TIME">old-view</string>
    <string name="LOGIN_OUT_TIME">2026-07-01 00:00:00</string>
</map>`)
	writeFile(t, filepath.Join(dir, "USERINFO.xml"), `<?xml version="1.0" encoding="utf-8"?>
<map>
    <string name="USERXH">U000000000</string>
    <string name="USERNAME">测试学生</string>
    <string name="USER_ROLES">1</string>
</map>`)
	writeFile(t, filepath.Join(dir, "SCHOOL.xml"), `<?xml version="1.0" encoding="utf-8"?>
<map>
    <string name="SERVERURL">https://example.invalid/cgapp-server/</string>
</map>`)
	writeFile(t, filepath.Join(dir, "config.yaml"), `paths:
  header_xml: "HEADER.xml"
  userinfo_xml: "USERINFO.xml"
  school_xml: "SCHOOL.xml"
upload:
  aes_encoding: "base64"
  token_header: "cgAuthorization"
device:
  imei: "00000000-0000-0000-0000-000000000000"
  user_agent: "hzkjapp/1.0.9(Linux;Android 15)"
  v1_seed: "seed"
  is_root: true
  phone_version: "PJE110,35,15|1.0.9"
`)

	auth, err := LoadAuthConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadAuthConfig: %v", err)
	}
	if auth.Token != "jwt.claims.signature" || auth.Secret != secret {
		t.Errorf("credentials = %q / %q", auth.Token, auth.Secret)
	}
	if auth.XH != "U000000000" || auth.Name != "测试学生" {
		t.Errorf("identity = %q / %q", auth.XH, auth.Name)
	}
	if auth.ServerURL != "https://example.invalid/cgapp-server" {
		t.Errorf("server url = %q", auth.ServerURL)
	}
	if auth.AESEncoding != "base64" || auth.TokenHeader != "cgAuthorization" {
		t.Errorf("upload config = %q / %q", auth.AESEncoding, auth.TokenHeader)
	}
	if !auth.IsRoot || auth.V1Seed != "seed" || auth.PhoneVersion != "PJE110,35,15|1.0.9" {
		t.Errorf("device config = %+v", auth)
	}
	if key, err := auth.AESKey(); err != nil || key != secret[:16] {
		t.Errorf("AESKey = %q, %v", key, err)
	}
	if missing := auth.MissingSessionFields(); len(missing) != 0 {
		t.Errorf("missing session fields = %v", missing)
	}
	if err := auth.HeadersConfigured(); err != nil {
		t.Errorf("HeadersConfigured: %v", err)
	}
}

func TestLoadAuthConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	auth, err := LoadAuthConfig(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatalf("LoadAuthConfig: %v", err)
	}
	if auth.ServerURL != DefaultServerURL {
		t.Errorf("server url = %q", auth.ServerURL)
	}
	if auth.AESEncoding != "base64" || auth.TokenHeader != "cgAuthorization" {
		t.Errorf("defaults = %q / %q", auth.AESEncoding, auth.TokenHeader)
	}
	if auth.PhoneVersion != DefaultPhoneVersion {
		t.Errorf("phone version = %q", auth.PhoneVersion)
	}
	if missing := auth.MissingSessionFields(); len(missing) != 2 {
		t.Errorf("missing fields = %v", missing)
	}
	if err := auth.HeadersConfigured(); !errors.Is(err, ErrConfig) {
		t.Errorf("HeadersConfigured error = %v", err)
	}
}

func TestSaveHeaderXMLWritesCredentialsAndKeepsOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "HEADER.xml")
	writeFile(t, path, `<?xml version="1.0" encoding="utf-8"?>
<map>
    <string name="TOKEN">old</string>
    <string name="EXTRA">keep-me</string>
</map>`)
	writeFile(t, path, `<?xml version="1.0" encoding="utf-8"?>
<map>
    <string name="TOKEN">old</string>
    <string name="SIGN">stale-sign</string>
    <string name="CGAPP_TIME_KEY">stale-time</string>
    <string name="EXTRA">keep-me</string>
</map>`)
	result := LoginResult{Success: true, Token: "new.token.value", Secret: "bmV3", LoginOutTime: "2026-08-01 00:00:00"}
	if err := SaveHeaderXML(path, result, true); err != nil {
		t.Fatalf("SaveHeaderXML: %v", err)
	}
	values, err := ReadSPXML(path)
	if err != nil {
		t.Fatalf("ReadSPXML: %v", err)
	}
	if values["TOKEN"] != "new.token.value" || values["SECRET"] != "bmV3" {
		t.Errorf("values = %v", values)
	}
	if values["LOGIN_OUT_TIME"] != "2026-08-01 00:00:00" {
		t.Errorf("login out time = %q", values["LOGIN_OUT_TIME"])
	}
	if values["EXTRA"] != "keep-me" {
		t.Errorf("unrelated key was dropped: %v", values)
	}
	if values["SIGN"] != "" || values["CGAPP_TIME_KEY"] != "" || values["WEB_VIEW_TIME"] != "" {
		t.Errorf("legacy fields bound to the previous SECRET must be cleared: %v", values)
	}
	if _, err := os.Stat(path + ".bak"); err != nil {
		t.Errorf("backup missing: %v", err)
	}
	if err := SaveHeaderXML(path, LoginResult{}, false); err == nil {
		t.Error("a failed login was persisted")
	}
}

func TestEnsureSPXMLAndWriteUserInfo(t *testing.T) {
	dir := t.TempDir()
	headerPath := filepath.Join(dir, "nested", "HEADER.xml")
	if err := EnsureSPXML(headerPath); err != nil {
		t.Fatalf("EnsureSPXML: %v", err)
	}
	if _, err := os.Stat(headerPath); err != nil {
		t.Fatalf("header file missing: %v", err)
	}

	userInfoPath := filepath.Join(dir, "USERINFO.xml")
	writeFile(t, userInfoPath, `<?xml version="1.0" encoding="utf-8"?>
<map>
    <string name="USER_ROLES">2</string>
</map>`)
	if err := WriteUserInfo(userInfoPath, "U000000000", "学生"); err != nil {
		t.Fatalf("WriteUserInfo: %v", err)
	}
	values, err := ReadSPXML(userInfoPath)
	if err != nil {
		t.Fatalf("ReadSPXML: %v", err)
	}
	if values["USERXH"] != "U000000000" || values["USERNAME"] != "学生" || values["USER_ROLES"] != "2" {
		t.Errorf("values = %v", values)
	}
}

func TestIsJWTExpired(t *testing.T) {
	now := time.Now()
	encode := func(payload string) string {
		return "header." + base64.RawURLEncoding.EncodeToString([]byte(payload)) + ".signature"
	}
	if isJWTExpired(encode(`{"exp":4102444800}`), now) {
		t.Error("a token valid until 2100 was reported expired")
	}
	if !isJWTExpired(encode(`{"exp":1000000000}`), now) {
		t.Error("an expired token was accepted")
	}
	if !isJWTExpired("garbage", now) {
		t.Error("an unparsable token was accepted")
	}
	if !isJWTExpired(encode(`not-json`), now) {
		t.Error("a token with a non-JSON payload was accepted")
	}
}
