package hustrun

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strings"
)

// LoginResult carries the credential bundle a CAS login produced.
type LoginResult struct {
	Success bool

	Token        string
	Secret       string
	LoginOutTime string
	Sign         string
	Timestamp    string
	WebViewTime  string

	XH   string
	Name string

	Error string
	Trace []string
}

// LoginOptions configures Login.
type LoginOptions struct {
	ServerURL string
	Username  string
	Password  string

	// Captcha downloads the login captcha and returns the code the user typed.
	// A nil callback fails the login when the page demands one.
	Captcha func(image []byte, suffix string) (string, error)
	// DynamicCode handles the二次 verification prompt (phoneCode).
	DynamicCode func(prompt string) (string, error)

	// UserAgent overrides the browser user agent used for the CAS flow.
	UserAgent string
	// HTTPClient overrides the client; cookies are added automatically.
	HTTPClient *http.Client
}

const casUserAgent = "Mozilla/5.0 (Linux; Android 14; Mobile) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/120.0.0.0 Mobile Safari/537.36"

// Login authenticates against the HUST CAS page and returns the App token
// bundle. The flow is: appLogin redirect -> /cas/rsa public key -> RSA
// PKCS#1 v1.5 encrypted ul/pl -> form POST -> getUserName callback.
func Login(ctx context.Context, options LoginOptions) (LoginResult, error) {
	result := LoginResult{}
	if strings.TrimSpace(options.Username) == "" || strings.TrimSpace(options.Password) == "" {
		return result, fmt.Errorf("%w: username and password are required", ErrConfig)
	}
	client, err := casHTTPClient(options)
	if err != nil {
		return result, err
	}
	serverURL := strings.TrimRight(options.ServerURL, "/")

	loginPage, err := httpGet(ctx, client, serverURL+"/cas/appLogin", map[string]string{
		"Accept":          "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
		"Accept-Language": "zh-CN,zh;q=0.9",
	})
	if err != nil {
		result.Error = "无法打开 CAS 登录页: " + err.Error()
		result.Trace = append(result.Trace, err.Error())
		return result, nil
	}
	result.Trace = append(result.Trace, loginPage.trace...)
	if loginPage.status != http.StatusOK {
		result.Error = fmt.Sprintf("CAS 登录页返回 HTTP %d", loginPage.status)
		return result, nil
	}
	finalHost := ""
	if parsed, parseErr := url.Parse(loginPage.url); parseErr == nil {
		finalHost = parsed.Hostname()
	}
	if !strings.EqualFold(finalHost, "pass.hust.edu.cn") {
		result.Error = "CAS 重定向目标异常: " + loginPage.url
		return result, nil
	}

	page := parseCASPage(loginPage.body)
	if page.Action == "" {
		result.Error = "CAS 页面中未找到 loginForm；页面结构可能已经更新"
		return result, nil
	}

	publicKey, err := fetchRSAPublicKey(ctx, client, loginPage.url)
	if err != nil {
		result.Error = "CAS RSA 初始化失败: " + err.Error()
		return result, nil
	}
	encryptedUser, err := rsaEncryptBase64(strings.TrimSpace(options.Username), publicKey)
	if err != nil {
		result.Error = "CAS RSA 初始化失败: " + err.Error()
		return result, nil
	}
	encryptedPassword, err := rsaEncryptBase64(strings.TrimSpace(options.Password), publicKey)
	if err != nil {
		result.Error = "CAS RSA 初始化失败: " + err.Error()
		return result, nil
	}

	captchaCode := ""
	if page.CaptchaURL != "" {
		if options.Captcha == nil {
			result.Error = "CAS 当前要求图形验证码，但调用方没有提供验证码输入"
			return result, nil
		}
		image, suffix, captchaErr := fetchCaptcha(ctx, client, loginPage.url, page.CaptchaURL)
		if captchaErr != nil {
			result.Error = "CAS 验证码下载失败: " + captchaErr.Error()
			return result, nil
		}
		code, codeErr := options.Captcha(image, suffix)
		captchaCode = strings.TrimSpace(code)
		if codeErr != nil {
			result.Error = "CAS 验证码输入失败: " + codeErr.Error()
			return result, nil
		}
		if captchaCode == "" {
			result.Error = "未输入验证码"
			return result, nil
		}
	}

	formURL := resolveReference(loginPage.url, page.Action)
	form := page.formValues()
	form.Set("ul", encryptedUser)
	form.Set("pl", encryptedPassword)
	form.Set("ua", browserUAJSON(options.resolvedUserAgent()))
	form.Set("visitorId", visitorID(options.resolvedUserAgent()))
	form.Set("code", captchaCode)
	if form.Get("_eventId") == "" {
		form.Set("_eventId", "submit")
	}
	response, err := httpPostForm(ctx, client, formURL, form, loginPage.url)
	if err != nil {
		result.Error = "CAS 表单提交失败: " + err.Error()
		return result, nil
	}
	result.Trace = append(result.Trace, response.trace...)

	second := parseCASPage(response.body)
	if isDynamicCodeChallenge(second.ErrorText) {
		if options.DynamicCode == nil {
			result.Error = "CAS: " + second.ErrorText
			return result, nil
		}
		code, codeErr := options.DynamicCode(second.ErrorText)
		code = strings.TrimSpace(code)
		if codeErr != nil {
			result.Error = "CAS 动态验证码输入失败: " + codeErr.Error()
			return result, nil
		}
		if code == "" {
			result.Error = "未输入动态验证码"
			return result, nil
		}
		if second.Action == "" {
			result.Error = "CAS 动态验证码页面缺少 loginForm"
			return result, nil
		}
		secondKey, keyErr := fetchRSAPublicKey(ctx, client, response.url)
		if keyErr != nil {
			result.Error = "CAS 动态验证码提交失败: " + keyErr.Error()
			return result, nil
		}
		encryptedUser, err = rsaEncryptBase64(strings.TrimSpace(options.Username), secondKey)
		if err != nil {
			result.Error = "CAS 动态验证码提交失败: " + err.Error()
			return result, nil
		}
		encryptedPassword, err = rsaEncryptBase64(strings.TrimSpace(options.Password), secondKey)
		if err != nil {
			result.Error = "CAS 动态验证码提交失败: " + err.Error()
			return result, nil
		}
		secondForm := second.formValues()
		secondForm.Set("ul", encryptedUser)
		secondForm.Set("pl", encryptedPassword)
		secondForm.Set("ua", browserUAJSON(options.resolvedUserAgent()))
		secondForm.Set("visitorId", visitorID(options.resolvedUserAgent()))
		secondForm.Set("code", "")
		secondForm.Set("phoneCode", code)
		if secondForm.Get("_eventId") == "" {
			secondForm.Set("_eventId", "submit")
		}
		response, err = httpPostForm(ctx, client, resolveReference(response.url, second.Action), secondForm, response.url)
		if err != nil {
			result.Error = "CAS 动态验证码提交失败: " + err.Error()
			return result, nil
		}
		result.Trace = append(result.Trace, response.trace...)
	}

	parsed := parseLoginResponse(response)
	parsed.Trace = result.Trace
	return parsed, nil
}

func (o LoginOptions) resolvedUserAgent() string {
	if o.UserAgent != "" {
		return o.UserAgent
	}
	return casUserAgent
}

type httpDocument struct {
	status  int
	url     string
	body    string
	headers http.Header
	trace   []string
}

func casHTTPClient(options LoginOptions) (*http.Client, error) {
	if options.HTTPClient != nil {
		if options.HTTPClient.Jar == nil {
			jar, err := cookiejar.New(nil)
			if err != nil {
				return nil, fmt.Errorf("%w: %v", ErrConfig, err)
			}
			options.HTTPClient.Jar = jar
		}
		return options.HTTPClient, nil
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	client := &http.Client{Jar: jar}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return client, nil
}

func httpGet(ctx context.Context, client *http.Client, target string, headers map[string]string) (httpDocument, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return httpDocument{}, err
	}
	request.Header.Set("User-Agent", casUserAgent)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response, err := client.Do(request)
	if err != nil {
		return httpDocument{}, err
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body, 4<<20)
	if err != nil {
		return httpDocument{}, err
	}
	document := httpDocument{
		status:  response.StatusCode,
		url:     response.Request.URL.String(),
		body:    string(body),
		headers: response.Header,
	}
	document.trace = append(document.trace, fmt.Sprintf("%d %s", response.StatusCode, document.url))
	return document, nil
}

func httpPostForm(ctx context.Context, client *http.Client, target string, form url.Values, referer string) (httpDocument, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, target, strings.NewReader(form.Encode()))
	if err != nil {
		return httpDocument{}, err
	}
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.Header.Set("User-Agent", casUserAgent)
	if referer != "" {
		request.Header.Set("Referer", referer)
		if parsed, parseErr := url.Parse(referer); parseErr == nil {
			request.Header.Set("Origin", parsed.Scheme+"://"+parsed.Host)
		}
	}
	response, err := client.Do(request)
	if err != nil {
		return httpDocument{}, err
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body, 4<<20)
	if err != nil {
		return httpDocument{}, err
	}
	document := httpDocument{
		status:  response.StatusCode,
		url:     response.Request.URL.String(),
		body:    string(body),
		headers: response.Header,
	}
	document.trace = append(document.trace, fmt.Sprintf("%d %s", response.StatusCode, document.url))
	return document, nil
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	limited := &io.LimitedReader{R: reader, N: limit + 1}
	data, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return data[:limit], nil
	}
	return data, nil
}

func resolveReference(base, reference string) string {
	baseURL, err := url.Parse(base)
	if err != nil {
		return reference
	}
	referenceURL, err := url.Parse(reference)
	if err != nil {
		return reference
	}
	return baseURL.ResolveReference(referenceURL).String()
}

func fetchRSAPublicKey(ctx context.Context, client *http.Client, loginURL string) (string, error) {
	rsaURL := resolveReference(loginURL, "rsa")
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, rsaURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", "application/json, text/javascript, */*; q=0.01")
	request.Header.Set("Referer", loginURL)
	request.Header.Set("X-Requested-With", "XMLHttpRequest")
	request.Header.Set("User-Agent", casUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body, 1<<20)
	if err != nil {
		return "", err
	}
	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", response.StatusCode)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("RSA 响应不是 JSON: %v", err)
	}
	key, _ := payload["publicKey"].(string)
	if strings.TrimSpace(key) == "" {
		return "", fmt.Errorf("响应中缺少 publicKey")
	}
	return key, nil
}

func fetchCaptcha(ctx context.Context, client *http.Client, loginURL, captchaPath string) ([]byte, string, error) {
	captchaURL := resolveReference(loginURL, captchaPath)
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, captchaURL, nil)
	if err != nil {
		return nil, "", err
	}
	request.Header.Set("Accept", "image/avif,image/webp,image/apng,image/*,*/*;q=0.8")
	request.Header.Set("Referer", loginURL)
	request.Header.Set("User-Agent", casUserAgent)
	response, err := client.Do(request)
	if err != nil {
		return nil, "", err
	}
	defer response.Body.Close()
	body, err := readLimited(response.Body, 2<<20)
	if err != nil {
		return nil, "", err
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if !strings.HasPrefix(contentType, "image/") || len(body) == 0 {
		return nil, "", fmt.Errorf("验证码响应格式异常: %s", contentType)
	}
	suffix := ".jpg"
	switch strings.TrimSpace(strings.Split(contentType, ";")[0]) {
	case "image/png":
		suffix = ".png"
	case "image/gif":
		suffix = ".gif"
	case "image/webp":
		suffix = ".webp"
	}
	switch {
	case strings.HasPrefix(string(body), "GIF87a"), strings.HasPrefix(string(body), "GIF89a"):
		suffix = ".gif"
	case strings.HasPrefix(string(body), "\x89PNG\r\n\x1a\n"):
		suffix = ".png"
	}
	return body, suffix, nil
}

// rsaEncryptBase64 encrypts value with the CAS public key (PKCS#1 v1.5) and
// returns base64. The key may be PEM or bare base64 DER.
func rsaEncryptBase64(value, publicKey string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("用户名和密码不能为空")
	}
	key, err := parseRSAPublicKey(publicKey)
	if err != nil {
		return "", err
	}
	encrypted, err := rsa.EncryptPKCS1v15(rand.Reader, key, []byte(value))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(encrypted), nil
}

func parseRSAPublicKey(material string) (*rsa.PublicKey, error) {
	trimmed := strings.TrimSpace(material)
	raw := []byte(trimmed)
	if block, _ := pem.Decode(raw); block != nil {
		raw = block.Bytes
	} else if !strings.Contains(trimmed, "BEGIN") {
		cleaned := strings.Map(func(char rune) rune {
			if char == '\n' || char == '\r' || char == ' ' || char == '\t' {
				return -1
			}
			return char
		}, trimmed)
		decoded, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return nil, fmt.Errorf("CAS 返回了无法解析的 RSA 公钥: %v", err)
		}
		raw = decoded
	}
	if parsed, err := x509.ParsePKIXPublicKey(raw); err == nil {
		if key, ok := parsed.(*rsa.PublicKey); ok {
			return key, nil
		}
	}
	if key, err := x509.ParsePKCS1PublicKey(raw); err == nil {
		return key, nil
	}
	if parsed, err := x509.ParsePKCS8PrivateKey(raw); err == nil {
		if key, ok := parsed.(*rsa.PrivateKey); ok {
			return &key.PublicKey, nil
		}
	}
	return nil, fmt.Errorf("CAS 返回了无法解析的 RSA 公钥")
}

func browserUAJSON(userAgent string) string {
	payload := struct {
		UA      string `json:"ua"`
		Browser struct {
			Name  string `json:"name"`
			Major string `json:"major"`
		} `json:"browser"`
		Device struct {
			Type string `json:"type"`
		} `json:"device"`
		OS struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"os"`
	}{}
	payload.UA = userAgent
	payload.Browser.Name = "Chrome"
	payload.Browser.Major = "120"
	payload.Device.Type = "mobile"
	payload.OS.Name = "Android"
	payload.OS.Version = "14"
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func visitorID(userAgent string) string {
	sum := sha256.Sum256([]byte(userAgent))
	return fmt.Sprintf("%x", sum)[:32]
}

func isDynamicCodeChallenge(message string) bool {
	return message != "" && strings.Contains(message, "动态验证码")
}

var loginCallbackPattern = regexp.MustCompile(`getUserName\s*\(\s*["']([^"']*)["']\s*,\s*["']([^"']+)["']`)

func parseLoginResponse(response httpDocument) LoginResult {
	if strings.Contains(strings.ToLower(response.headers.Get("Content-Type")), "json") {
		return parseJSONLoginResponse(response)
	}
	if match := loginCallbackPattern.FindStringSubmatch(response.body); match != nil {
		result := parseTokenBundle(match[2])
		result.XH = match[1]
		return result
	}
	page := parseCASPage(response.body)
	if page.ErrorText != "" {
		return LoginResult{Error: "CAS: " + page.ErrorText}
	}
	if parsed, err := url.Parse(response.url); err == nil {
		if strings.EqualFold(parsed.Hostname(), "pass.hust.edu.cn") {
			return LoginResult{Error: "CAS 未返回登录凭证；可能需要验证码、二次认证，或账号信息不正确"}
		}
	}
	return LoginResult{Error: fmt.Sprintf("App 登录回调格式未知: HTTP %d, URL=%s", response.status, response.url)}
}

func parseJSONLoginResponse(response httpDocument) LoginResult {
	var body map[string]any
	if err := json.Unmarshal([]byte(response.body), &body); err != nil {
		return LoginResult{Error: "登录接口返回了无效 JSON"}
	}
	// The login endpoint may answer without a code field; only a present and
	// non-2xx code is a failure here.
	if _, hasCode := body["code"]; hasCode {
		ok, code, message := apiStatus(body)
		if !ok {
			if message == "" {
				message = "登录失败"
			}
			return LoginResult{Error: fmt.Sprintf("登录接口 code=%s: %s", code, message)}
		}
	}
	data, _ := body["data"].(map[string]any)
	if data == nil {
		data = body
	}
	bundle, _ := data["token"].(string)
	if bundle == "" {
		if value, ok := body["token"].(string); ok {
			bundle = value
		}
	}
	result := parseTokenBundle(bundle)
	if info, ok := data["info"].(map[string]any); ok {
		if xh, ok := info["xh"].(string); ok {
			result.XH = xh
		} else if uid, ok := info["uid"].(string); ok {
			result.XH = uid
		}
		if name, ok := info["xm"].(string); ok {
			result.Name = name
		} else if name, ok := info["name"].(string); ok {
			result.Name = name
		}
	}
	return result
}

// parseTokenBundle splits the getUserName callback value:
// JWT.SECRET.LOGIN_OUT_TIME[...]. The App needs at least five segments.
func parseTokenBundle(bundle string) LoginResult {
	parts := strings.Split(bundle, ".")
	if len(parts) < 5 {
		return LoginResult{Error: fmt.Sprintf("登录凭证格式异常：收到 %d 段，App 需要至少 5 段", len(parts))}
	}
	return LoginResult{
		Success:      true,
		Token:        strings.Join(parts[:3], "."),
		Secret:       parts[3],
		LoginOutTime: parts[4],
	}
}

// casPage is the subset of the CAS login document this client needs.
type casPage struct {
	Action     string
	Inputs     map[string]string
	CaptchaURL string
	ErrorText  string
}

func (p casPage) formValues() url.Values {
	form := url.Values{}
	for name, value := range p.Inputs {
		form.Set(name, value)
	}
	// The browser disables the visible un/pd fields and submits only the
	// encrypted ul/pl pair.
	for _, name := range []string{"un", "pd", "username", "password", "rsa"} {
		form.Del(name)
	}
	return form
}

// parseCASPage extracts the login form, its hidden inputs, the captcha image and
// the visible error text without pulling in a full HTML parser.
func parseCASPage(document string) casPage {
	page := casPage{Inputs: map[string]string{}}
	var (
		inLoginForm bool
		errorStack  []string
		errorParts  []string
	)
	appendText := func(text string) {
		if len(errorStack) == 0 {
			return
		}
		trimmed := strings.TrimSpace(text)
		if trimmed != "" {
			errorParts = append(errorParts, trimmed)
		}
	}
	position := 0
	for position < len(document) {
		next := strings.IndexByte(document[position:], '<')
		if next < 0 {
			appendText(document[position:])
			break
		}
		appendText(document[position : position+next])
		position += next
		if strings.HasPrefix(document[position:], "<!--") {
			end := strings.Index(document[position+4:], "-->")
			if end < 0 {
				break
			}
			position += 4 + end + 3
			continue
		}
		if strings.HasPrefix(document[position:], "<!") || strings.HasPrefix(document[position:], "<?") {
			end := strings.IndexByte(document[position:], '>')
			if end < 0 {
				break
			}
			position += end + 1
			continue
		}
		end := findTagEnd(document, position)
		if end < 0 {
			break
		}
		raw := document[position+1 : end]
		position = end + 1

		tag := parseHTMLTag(raw)
		if tag.name == "" {
			continue
		}
		if tag.closing {
			if len(errorStack) > 0 && errorStack[len(errorStack)-1] == tag.name {
				errorStack = errorStack[:len(errorStack)-1]
			}
			if tag.name == "form" {
				inLoginForm = false
			}
			continue
		}
		switch tag.name {
		case "script", "style", "textarea":
			if !tag.selfClosing {
				closing := "</" + tag.name
				offset := strings.Index(strings.ToLower(document[position:]), closing)
				if offset < 0 {
					position = len(document)
				} else {
					position += offset
				}
			}
		case "form":
			isLoginForm := tag.attrs["id"] == "loginForm"
			if isLoginForm || page.Action == "" {
				page.Action = tag.attrs["action"]
				inLoginForm = true
			}
		case "input":
			if inLoginForm {
				if name := tag.attrs["name"]; name != "" {
					page.Inputs[name] = tag.attrs["value"]
				}
			}
		case "img":
			if tag.attrs["id"] == "codeImage" {
				page.CaptchaURL = tag.attrs["src"]
			}
		}
		if !tag.selfClosing && !isVoidHTMLTag(tag.name) {
			identifier := tag.attrs["id"]
			classes := strings.Fields(tag.attrs["class"])
			if identifier == "errormsg" || identifier == "errormsghide" || containsString(classes, "error-msg-box") {
				errorStack = append(errorStack, tag.name)
			} else if len(errorStack) > 0 {
				errorStack = append(errorStack, tag.name)
			}
		}
	}
	page.ErrorText = strings.Join(errorParts, " ")
	return page
}

func isVoidHTMLTag(name string) bool {
	switch name {
	case "br", "img", "input", "hr", "meta", "link", "area", "base", "col", "embed", "source", "track", "wbr":
		return true
	}
	return false
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type htmlTag struct {
	name        string
	closing     bool
	selfClosing bool
	attrs       map[string]string
}

// findTagEnd returns the index of the '>' that closes the tag starting at
// position, skipping quoted attribute values.
func findTagEnd(document string, position int) int {
	var quote byte
	for index := position + 1; index < len(document); index++ {
		char := document[index]
		switch {
		case quote != 0:
			if char == quote {
				quote = 0
			}
		case char == '"' || char == '\'':
			quote = char
		case char == '>':
			return index
		}
	}
	return -1
}

func parseHTMLTag(raw string) htmlTag {
	tag := htmlTag{attrs: map[string]string{}}
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(trimmed, "/") {
		tag.closing = true
		trimmed = strings.TrimSpace(trimmed[1:])
	}
	if strings.HasSuffix(trimmed, "/") {
		tag.selfClosing = true
		trimmed = strings.TrimSpace(trimmed[:len(trimmed)-1])
	}
	nameEnd := strings.IndexAny(trimmed, " \t\r\n")
	if nameEnd < 0 {
		tag.name = strings.ToLower(trimmed)
		return tag
	}
	tag.name = strings.ToLower(trimmed[:nameEnd])
	rest := trimmed[nameEnd:]
	for {
		rest = strings.TrimLeft(rest, " \t\r\n")
		if rest == "" {
			break
		}
		equals := strings.IndexByte(rest, '=')
		if equals < 0 {
			break
		}
		name := strings.ToLower(strings.TrimSpace(rest[:equals]))
		rest = strings.TrimLeft(rest[equals+1:], " \t\r\n")
		var value string
		if len(rest) > 0 && (rest[0] == '"' || rest[0] == '\'') {
			closing := strings.IndexByte(rest[1:], rest[0])
			if closing < 0 {
				value = rest[1:]
				rest = ""
			} else {
				value = rest[1 : 1+closing]
				rest = rest[2+closing:]
			}
		} else {
			valueEnd := strings.IndexAny(rest, " \t\r\n")
			if valueEnd < 0 {
				value = rest
				rest = ""
			} else {
				value = rest[:valueEnd]
				rest = rest[valueEnd:]
			}
		}
		if name != "" {
			tag.attrs[name] = value
		}
	}
	return tag
}
