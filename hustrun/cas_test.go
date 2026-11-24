package hustrun

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net/url"
	"strings"
	"testing"
)

const sampleCASPage = `<!DOCTYPE html>
<html>
<head><title>统一身份认证</title></head>
<body>
<script>var x = "<form id='fake'>";</script>
<div id="errormsghide" class="error-msg-box"><span>密码错误</span></div>
<form id="loginForm" action="/cas/login?service=app" method="post">
  <input type="hidden" name="lt" value="LT-1234-abc">
  <input type="hidden" name="execution" value="e1s1">
  <input type="hidden" name="_eventId" value="submit">
  <input type="text" name="un" value="">
  <input type="password" name="pd" value="">
  <img id="codeImage" src="/cas/captcha?ts=1" alt="captcha">
  <input type="submit" name="submit" value="登录">
</form>
</body>
</html>`

func TestParseCASPage(t *testing.T) {
	page := parseCASPage(sampleCASPage)
	if page.Action != "/cas/login?service=app" {
		t.Errorf("action = %q", page.Action)
	}
	if page.Inputs["lt"] != "LT-1234-abc" || page.Inputs["execution"] != "e1s1" {
		t.Errorf("inputs = %v", page.Inputs)
	}
	if page.Inputs["_eventId"] != "submit" {
		t.Errorf("_eventId = %q", page.Inputs["_eventId"])
	}
	if page.CaptchaURL != "/cas/captcha?ts=1" {
		t.Errorf("captcha = %q", page.CaptchaURL)
	}
	if page.ErrorText != "密码错误" {
		t.Errorf("error text = %q", page.ErrorText)
	}

	form := page.formValues()
	for _, name := range []string{"un", "pd", "username", "password", "rsa"} {
		if form.Get(name) != "" {
			t.Errorf("form still carries %s", name)
		}
	}
	if form.Get("lt") != "LT-1234-abc" {
		t.Errorf("hidden fields were dropped: %v", form)
	}
}

func TestParseCASPageWithoutErrorBlock(t *testing.T) {
	page := parseCASPage(strings.Replace(sampleCASPage,
		`<div id="errormsghide" class="error-msg-box"><span>密码错误</span></div>`, "", 1))
	if page.ErrorText != "" {
		t.Errorf("error text = %q, want empty", page.ErrorText)
	}
	// The script body must not be treated as markup.
	if page.Action != "/cas/login?service=app" {
		t.Errorf("action = %q", page.Action)
	}
}

func TestParseCASPageIgnoresFieldsOutsideTheLoginForm(t *testing.T) {
	document := `<html><body>
<form action="/first"><input name="a" value="1"></form>
<form id="loginForm" action="/cas/login"><input name="b" value="2"></form>
</body></html>`
	page := parseCASPage(document)
	if page.Action != "/cas/login" {
		t.Errorf("action = %q", page.Action)
	}
	if page.Inputs["b"] != "2" {
		t.Errorf("inputs = %v", page.Inputs)
	}
}

func TestParseTokenBundle(t *testing.T) {
	result := parseTokenBundle("aaa.bbb.ccc.c2VjcmV0.2026-07-01 00:00:00")
	if !result.Success {
		t.Fatalf("bundle rejected: %s", result.Error)
	}
	if result.Token != "aaa.bbb.ccc" {
		t.Errorf("token = %q", result.Token)
	}
	if result.Secret != "c2VjcmV0" {
		t.Errorf("secret = %q", result.Secret)
	}
	if result.LoginOutTime != "2026-07-01 00:00:00" {
		t.Errorf("logout time = %q", result.LoginOutTime)
	}

	short := parseTokenBundle("aaa.bbb.ccc")
	if short.Success || !strings.Contains(short.Error, "5 段") {
		t.Errorf("short bundle = %+v", short)
	}
}

func TestParseLoginResponseExtractsCallback(t *testing.T) {
	body := `<html><script>window.getUserName("U202400001", "aaa.bbb.ccc.c2VjcmV0.2026-07-01 00:00:00");</script></html>`
	document := httpDocument{
		status:  200,
		url:     "https://petzjk.hust.edu.cn/cgapp-server/cas/callback",
		body:    body,
		headers: map[string][]string{"Content-Type": {"text/html"}},
	}
	result := parseLoginResponse(document)
	if !result.Success {
		t.Fatalf("login response rejected: %s", result.Error)
	}
	if result.XH != "U202400001" {
		t.Errorf("xh = %q", result.XH)
	}
	if result.Secret != "c2VjcmV0" {
		t.Errorf("secret = %q", result.Secret)
	}
}

func TestParseJSONLoginResponseWithoutCode(t *testing.T) {
	document := httpDocument{
		status:  200,
		url:     "https://petzjk.hust.edu.cn/cgapp-server/login",
		body:    `{"data":{"token":"aaa.bbb.ccc.c2VjcmV0.2026-07-01 00:00:00","info":{"xh":"U1","xm":"学生"}}}`,
		headers: map[string][]string{"Content-Type": {"application/json"}},
	}
	result := parseLoginResponse(document)
	if !result.Success {
		t.Fatalf("login answer rejected: %s", result.Error)
	}
	if result.XH != "U1" || result.Name != "学生" {
		t.Errorf("identity = %q / %q", result.XH, result.Name)
	}
	if result.Secret != "c2VjcmV0" {
		t.Errorf("secret = %q", result.Secret)
	}

	failed := httpDocument{
		status:  200,
		url:     "https://petzjk.hust.edu.cn/cgapp-server/login",
		body:    `{"code":500,"message":"账号或密码错误"}`,
		headers: map[string][]string{"Content-Type": {"application/json"}},
	}
	if parsed := parseLoginResponse(failed); parsed.Success || !strings.Contains(parsed.Error, "账号或密码错误") {
		t.Errorf("failure answer = %+v", parsed)
	}
}

func TestRSAEncryptBase64AcceptsPEMAndDER(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey: %v", err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der}))
	forms := map[string]string{
		"pem":  pemText,
		"der":  base64.StdEncoding.EncodeToString(der),
		"wrap": wrapBase64(base64.StdEncoding.EncodeToString(der)),
	}
	for name, material := range forms {
		encoded, err := rsaEncryptBase64("hustrun", material)
		if err != nil {
			t.Fatalf("%s: rsaEncryptBase64: %v", name, err)
		}
		ciphertext, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil {
			t.Fatalf("%s: base64 decode: %v", name, err)
		}
		plaintext, err := rsa.DecryptPKCS1v15(rand.Reader, key, ciphertext)
		if err != nil {
			t.Fatalf("%s: DecryptPKCS1v15: %v", name, err)
		}
		if string(plaintext) != "hustrun" {
			t.Errorf("%s: plaintext = %q", name, plaintext)
		}
	}
	if _, err := rsaEncryptBase64("hustrun", "not-a-key"); err == nil {
		t.Error("garbage public key accepted")
	}
	if _, err := rsaEncryptBase64("", forms["pem"]); err == nil {
		t.Error("empty username accepted")
	}
}

func wrapBase64(value string) string {
	var builder strings.Builder
	for len(value) > 64 {
		builder.WriteString(value[:64])
		builder.WriteString("\n")
		value = value[64:]
	}
	builder.WriteString(value)
	return builder.String()
}

func TestBrowserUAJSONAndVisitorID(t *testing.T) {
	payload := browserUAJSON("UA/1.0")
	for _, fragment := range []string{`"ua":"UA/1.0"`, `"name":"Chrome"`, `"version":"14"`} {
		if !strings.Contains(payload, fragment) {
			t.Errorf("ua payload %q missing %s", payload, fragment)
		}
	}
	if len(visitorID("UA/1.0")) != 32 {
		t.Errorf("visitor id = %q", visitorID("UA/1.0"))
	}
	if visitorID("a") == visitorID("b") {
		t.Error("visitor id does not depend on the client fingerprint")
	}
}

func TestIsDynamicCodeChallenge(t *testing.T) {
	if !isDynamicCodeChallenge("验证码已发送到企业微信，请输入动态验证码") {
		t.Error("challenge not detected")
	}
	if isDynamicCodeChallenge("密码错误") {
		t.Error("plain error treated as a challenge")
	}
}

func TestLoginRejectsEmptyCredentials(t *testing.T) {
	if _, err := Login(context.Background(), LoginOptions{ServerURL: "https://example.invalid"}); err == nil {
		t.Error("login without credentials succeeded")
	}
}

func TestFormValuesEncodeAndSign(t *testing.T) {
	var form FormValues
	form.Add("jsonsports", "abc+def/==")
	encoded := form.Encode()
	decoded, err := url.QueryUnescape(encoded)
	if err != nil {
		t.Fatalf("QueryUnescape: %v", err)
	}
	if decoded != "jsonsports=abc+def/==" {
		t.Errorf("decoded form = %q", decoded)
	}
	target := BuildFormSigningTarget("/api/l/v7/savesports", form)
	if !strings.HasPrefix(target, "/api/l/v7/savesports?jsonsports=") {
		t.Errorf("target = %q", target)
	}
}
