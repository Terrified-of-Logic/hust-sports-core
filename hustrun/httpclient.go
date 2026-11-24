package hustrun

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf16"
)

// Business endpoints of the App 1.0.9 protocol.
const (
	pathServerTime  = "/api/l/v6/currentTimeMillis"
	pathInSportTime = "/api/l/v6.1/sporttype/inSportTime"
	pathSportsID    = "/api/l/v7/sportsId"
	pathPrejudgment = "/api/l/v6.1/prejudgment"
	pathSaveSports  = "/api/l/v7/savesports"
	pathSensorZip   = "/api/l/v7/upload/sensorZipFile"
	pathSensorFile  = "/api/l/v7/upload/sensorFile"

	defaultRequestTimeout = 10 * time.Second
	uploadRequestTimeout  = 30 * time.Second
	maxResponseBytes      = 1 << 20
)

// Client issues the business requests of the upload chain.
type Client struct {
	Auth   *AuthConfig
	Signer Signer
	HTTP   *http.Client
	// BaseURL overrides Auth.ServerURL (tests point it at a local server).
	BaseURL string
	// Now overrides the clock; nil means time.Now.
	Now func() time.Time
}

// NewClient validates the credentials and device fingerprint and returns a
// client. A nil HTTP client uses Go's default transport.
func NewClient(auth *AuthConfig, signer Signer, httpClient *http.Client) (*Client, error) {
	if auth == nil {
		return nil, fmt.Errorf("%w: auth config is nil", ErrConfig)
	}
	if err := auth.HeadersConfigured(); err != nil {
		return nil, err
	}
	if signer == nil {
		return nil, fmt.Errorf("%w: a signer is required for business requests", ErrSigner)
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: uploadRequestTimeout}
	}
	return &Client{Auth: auth, Signer: signer, HTTP: httpClient}, nil
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) baseURL() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return strings.TrimRight(c.Auth.ServerURL, "/")
}

// QueryPair is one ordered query parameter. The order is preserved so the URL
// and the signing target stay byte-identical to what the client signed.
type QueryPair struct {
	Key   string
	Value string
}

// EncodeQuery renders pairs the way Go and Python's urlencode do for these
// ASCII-only values.
func EncodeQuery(pairs []QueryPair) string {
	if len(pairs) == 0 {
		return ""
	}
	encoded := make([]string, len(pairs))
	for i, pair := range pairs {
		encoded[i] = urlQueryEscape(pair.Key) + "=" + urlQueryEscape(pair.Value)
	}
	return strings.Join(encoded, "&")
}

func urlQueryEscape(value string) string {
	// QueryEscape escapes space as '+', which matches the client's form
	// encoding and Python's quote_plus.
	return url.QueryEscape(value)
}

// BuildHeaders renders the interceptor chain's headers for a request. The
// signature covers signingTarget, which for JSON requests is the path and for
// form requests is path plus the sorted form fields.
func (c *Client) BuildHeaders(path, signingTarget string) (http.Header, error) {
	if err := c.Auth.HeadersConfigured(); err != nil {
		return nil, err
	}
	if strings.TrimSpace(signingTarget) == "" {
		signingTarget = path
	}
	timestamp, sign, err := c.Signer.Sign(c.Auth.Secret, signingTarget)
	if err != nil {
		return nil, err
	}
	aesKey, err := c.Auth.AESKey()
	if err != nil {
		return nil, err
	}
	clientHeader, err := EncryptClientHeader(aesKey, c.Auth.IsRoot, "base64", c.now())
	if err != nil {
		return nil, err
	}
	headers := http.Header{}
	headers["Content-Type"] = []string{"application/json;charset=UTF-8"}
	headers["imei"] = []string{c.Auth.IMEI}
	headers["timestamp"] = []string{timestamp}
	headers["sign"] = []string{sign}
	headers["User-Agent"] = []string{c.Auth.UserAgent}
	// Wire-level headers observed in the official App's traffic.
	headers["Accept-Encoding"] = []string{"gzip"}
	headers["Connection"] = []string{"Keep-Alive"}
	headers["client"] = []string{clientHeader}
	headers["v1"] = []string{V1Header(c.Auth.V1Seed, c.now())}
	headers["cgAuthorization"] = []string{c.Auth.Token}
	return headers, nil
}

// BuildFormHeaders renders headers for an application/x-www-form-urlencoded
// body, signing the normalized field set.
func (c *Client) BuildFormHeaders(path string, form FormValues) (http.Header, error) {
	headers, err := c.BuildHeaders(path, BuildFormSigningTarget(path, form))
	if err != nil {
		return nil, err
	}
	headers["Content-Type"] = []string{"application/x-www-form-urlencoded"}
	return headers, nil
}

// FormValues is an ordered form body. Order matters only for the request body;
// the signature sorts the fields itself.
type FormValues struct {
	pairs []QueryPair
}

// Add appends a field.
func (f *FormValues) Add(key, value string) {
	f.pairs = append(f.pairs, QueryPair{Key: key, Value: value})
}

// Get returns the first value for a key.
func (f FormValues) Get(key string) string {
	for _, pair := range f.pairs {
		if pair.Key == key {
			return pair.Value
		}
	}
	return ""
}

// Encode renders the body.
func (f FormValues) Encode() string { return EncodeQuery(f.pairs) }

// Pairs exposes the ordered fields.
func (f FormValues) Pairs() []QueryPair { return f.pairs }

// BuildFormSigningTarget reproduces the official Common(path, TreeMap) target:
// the fields sorted the way Java's TreeMap compares strings (UTF-16 code unit
// order), concatenated as name=value with '&'.
func BuildFormSigningTarget(path string, form FormValues) string {
	if len(form.pairs) == 0 {
		return path
	}
	normalized := make([]QueryPair, 0, len(form.pairs))
	for _, pair := range form.pairs {
		normalized = append(normalized, pair)
	}
	sort.SliceStable(normalized, func(i, j int) bool {
		return compareJavaString(normalized[i].Key, normalized[j].Key) < 0
	})
	var builder strings.Builder
	builder.WriteString(path)
	builder.WriteByte('?')
	for index, pair := range normalized {
		if index > 0 {
			builder.WriteByte('&')
		}
		builder.WriteString(pair.Key)
		builder.WriteByte('=')
		builder.WriteString(pair.Value)
	}
	return builder.String()
}

// compareJavaString orders two strings by UTF-16 code unit, matching Java's
// String.compareTo for ASCII and supplementary-plane input alike.
func compareJavaString(a, b string) int {
	left := utf16.Encode([]rune(a))
	right := utf16.Encode([]rune(b))
	for index := 0; index < len(left) && index < len(right); index++ {
		if left[index] != right[index] {
			if left[index] < right[index] {
				return -1
			}
			return 1
		}
	}
	switch {
	case len(left) < len(right):
		return -1
	case len(left) > len(right):
		return 1
	}
	return 0
}

// BuildURLSigningTarget renders "/path?query" for a signed GET.
func BuildURLSigningTarget(path string, pairs []QueryPair) string {
	encoded := EncodeQuery(pairs)
	if encoded == "" {
		return path
	}
	return path + "?" + encoded
}

type apiResponse struct {
	status int
	header http.Header
	body   []byte
}

func (r apiResponse) text() string { return string(r.body) }

func (c *Client) do(ctx context.Context, method, target string, headers http.Header, body io.Reader, timeout time.Duration) (apiResponse, error) {
	if timeout <= 0 {
		timeout = defaultRequestTimeout
	}
	requestContext := ctx
	var cancel context.CancelFunc
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		requestContext, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	request, err := http.NewRequestWithContext(requestContext, method, target, body)
	if err != nil {
		return apiResponse{}, err
	}
	if headers != nil {
		request.Header = headers
	}
	response, err := c.HTTP.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return apiResponse{}, ctx.Err()
		}
		return apiResponse{}, err
	}
	defer response.Body.Close()

	var reader io.Reader = &io.LimitedReader{R: response.Body, N: maxResponseBytes}
	if strings.EqualFold(response.Header.Get("Content-Encoding"), "gzip") {
		gzipReader, gzipErr := gzip.NewReader(reader)
		if gzipErr == nil {
			defer gzipReader.Close()
			reader = gzipReader
		}
	}
	payload, err := io.ReadAll(reader)
	if err != nil {
		return apiResponse{}, err
	}
	return apiResponse{status: response.StatusCode, header: response.Header, body: payload}, nil
}

// ProbeServerTime asks the server for its clock.
func (c *Client) ProbeServerTime(ctx context.Context) ServerProbe {
	headers, err := c.BuildHeaders(pathServerTime, pathServerTime)
	if err != nil {
		return ServerProbe{Message: err.Error()}
	}
	response, err := c.do(ctx, http.MethodGet, c.baseURL()+pathServerTime, headers, nil, defaultRequestTimeout)
	if err != nil {
		return ServerProbe{Message: "网络请求失败: " + err.Error()}
	}
	probe := ServerProbe{HTTPStatus: response.status}
	if response.status != http.StatusOK {
		probe.Message = httpErrorMessage(response)
		return probe
	}
	var body map[string]any
	if err := json.Unmarshal(response.body, &body); err != nil {
		probe.Message = "服务器返回 HTTP 200，但响应不是 JSON"
		return probe
	}
	ok, code, message := apiStatus(body)
	probe.APICode = code
	probe.Message = message
	if !ok {
		return probe
	}
	var value any = body["data"]
	if nested, isMap := value.(map[string]any); isMap {
		if current, exists := nested["currentTimeMillis"]; exists {
			value = current
		} else if current, exists := nested["time"]; exists {
			value = current
		}
	}
	serverTime, parseErr := jsonInt64(value)
	if parseErr != nil {
		probe.Message = "currentTimeMillis 响应缺少有效毫秒时间戳"
		return probe
	}
	probe.ServerTime = serverTime
	return probe
}

// RequestInSportTime asks the server whether the exercise window is open.
func (c *Client) RequestInSportTime(ctx context.Context, sportTypeID string) InSportTimeResult {
	if sportTypeID == "" {
		sportTypeID = "7"
	}
	pairs := []QueryPair{{Key: "sportTypeId", Value: sportTypeID}}
	target := BuildURLSigningTarget(pathInSportTime, pairs)
	headers, err := c.BuildHeaders(pathInSportTime, target)
	if err != nil {
		return InSportTimeResult{Message: err.Error()}
	}
	response, err := c.do(ctx, http.MethodGet, c.baseURL()+target, headers, nil, defaultRequestTimeout)
	if err != nil {
		return InSportTimeResult{Message: "网络请求失败: " + err.Error()}
	}
	result := InSportTimeResult{HTTPStatus: response.status}
	if response.status != http.StatusOK {
		result.Message = httpErrorMessage(response)
		return result
	}
	var body map[string]any
	if err := json.Unmarshal(response.body, &body); err != nil {
		result.Message = "inSportTime 返回 HTTP 200，但响应不是 JSON"
		return result
	}
	ok, code, message := apiStatus(body)
	result.APICode = code
	result.Message = message
	if !ok {
		return result
	}
	text := strings.TrimSpace(fmt.Sprint(body["data"]))
	if text != "0" && text != "1" {
		result.Message = "inSportTime 返回了无法识别的值: " + truncate(text, 20)
		return result
	}
	allowed := text == "1"
	result.Allowed = &allowed
	return result
}

// RequestSportsID claims the server-issued identifier that precedes a run.
func (c *Client) RequestSportsID(ctx context.Context) SportsIDResult {
	headers, err := c.BuildHeaders(pathSportsID, pathSportsID)
	if err != nil {
		return SportsIDResult{Message: err.Error()}
	}
	response, err := c.do(ctx, http.MethodGet, c.baseURL()+pathSportsID, headers, nil, defaultRequestTimeout)
	if err != nil {
		return SportsIDResult{Message: "网络请求失败: " + err.Error()}
	}
	result := SportsIDResult{HTTPStatus: response.status}
	if response.status != http.StatusOK {
		result.Message = httpErrorMessage(response)
		return result
	}
	var body map[string]any
	if err := json.Unmarshal(response.body, &body); err != nil {
		result.Message = "sportsId 返回 HTTP 200，但响应不是 JSON"
		return result
	}
	ok, code, message := apiStatus(body)
	result.APICode = code
	result.Message = message
	if !ok {
		return result
	}
	value, _ := body["data"].(string)
	if strings.TrimSpace(value) == "" {
		result.Message = "sportsId 响应缺少有效 ID"
		return result
	}
	result.SportID = strings.TrimSpace(value)
	return result
}

// RequestPrejudgment runs the end-of-run distance / duration / window check.
func (c *Client) RequestPrejudgment(ctx context.Context, encryptedPayload string) PrejudgmentResult {
	var form FormValues
	form.Add("jsonsports", encryptedPayload)
	headers, err := c.BuildFormHeaders(pathPrejudgment, form)
	if err != nil {
		return PrejudgmentResult{Message: err.Error()}
	}
	response, err := c.do(ctx, http.MethodPost, c.baseURL()+pathPrejudgment, headers,
		strings.NewReader(form.Encode()), uploadRequestTimeout)
	if err != nil {
		return PrejudgmentResult{Message: "网络请求失败: " + err.Error()}
	}
	result := PrejudgmentResult{HTTPStatus: response.status}
	if response.status != http.StatusOK {
		result.Message = httpErrorMessage(response)
		return result
	}
	var body map[string]any
	if err := json.Unmarshal(response.body, &body); err != nil {
		result.Message = "prejudgment 返回 HTTP 200，但响应不是 JSON"
		return result
	}
	ok, code, message := apiStatus(body)
	result.APICode = code
	result.Message = message
	if !ok {
		return result
	}
	items, isList := body["data"].([]any)
	if !isList {
		result.Message = "prejudgment 响应缺少检查列表"
		return result
	}
	checks := make([]PrejudgmentCheck, 0, len(items))
	for _, item := range items {
		entry, isMap := item.(map[string]any)
		if !isMap {
			result.Message = "prejudgment 检查项格式异常"
			return result
		}
		if _, exists := entry["isvalid"]; !exists {
			result.Message = "prejudgment 检查项格式异常"
			return result
		}
		checks = append(checks, PrejudgmentCheck{
			IsValid: fmt.Sprint(entry["isvalid"]),
			Content: fmt.Sprint(entry["content"]),
		})
	}
	result.Checks = checks
	result.Success = true
	return result
}

// UploadSavesports submits the main record.
func (c *Client) UploadSavesports(ctx context.Context, encryptedPayload string) UploadResult {
	var form FormValues
	form.Add("jsonsports", encryptedPayload)
	headers, err := c.BuildFormHeaders(pathSaveSports, form)
	if err != nil {
		return UploadResult{RawResponse: err.Error()}
	}
	response, err := c.do(ctx, http.MethodPost, c.baseURL()+pathSaveSports, headers,
		strings.NewReader(form.Encode()), uploadRequestTimeout)
	if err != nil {
		return UploadResult{RawResponse: err.Error()}
	}
	result := UploadResult{RawResponse: response.text(), HTTPStatus: response.status}
	if response.status != http.StatusOK {
		result.RawResponse = httpErrorMessage(response)
		return result
	}
	var body map[string]any
	if err := json.Unmarshal(response.body, &body); err != nil {
		result.RawResponse = "服务器返回 HTTP 200，但响应不是 JSON"
		return result
	}
	ok, code, message := apiStatus(body)
	result.APICode = code
	result.APIMessage = message
	if !ok {
		result.RawResponse = fmt.Sprintf("业务请求失败 code=%s: %s", code, message)
		return result
	}
	apiData, _ := body["data"].(map[string]any)
	if apiData == nil {
		result.RawResponse = "savesports 响应缺少 data 对象"
		return result
	}
	if _, hasSportID := apiData["sportId"]; !hasSportID {
		result.RawResponse = "savesports 响应缺少 sportId/isValid，不能判定上传成功"
		return result
	}
	if _, hasValid := apiData["isValid"]; !hasValid {
		result.RawResponse = "savesports 响应缺少 sportId/isValid，不能判定上传成功"
		return result
	}
	result.IsValid = fmt.Sprint(apiData["isValid"])
	reason := apiData["inValidReason"]
	if reason == nil || fmt.Sprint(reason) == "" {
		reason = apiData["isValidReason"]
	}
	result.InValidReason = fmt.Sprint(reason)
	result.CheckStatus = fmt.Sprint(apiData["checkStatus"])
	result.Tip = fmt.Sprint(apiData["tip"])
	result.SportID = fmt.Sprint(apiData["sportId"])
	result.Success = true
	return result
}

// UploadSensorZip uploads the gzip+base64 sensor blob as multipart data.
func (c *Client) UploadSensorZip(ctx context.Context, sportID string, zipData []byte, filename string) bool {
	if filename == "" {
		filename = sportID + ".txt"
	}
	if strings.ContainsAny(filename, "\r\n\"") {
		return false
	}
	pairs := []QueryPair{
		{Key: "sportId", Value: sportID},
		{Key: "sensorZipFile", Value: filename},
	}
	target := BuildURLSigningTarget(pathSensorZip, pairs)
	headers, err := c.BuildHeaders(pathSensorZip, target)
	if err != nil {
		return false
	}
	boundary, err := newBoundary()
	if err != nil {
		return false
	}
	var body bytes.Buffer
	body.WriteString("--" + boundary + "\r\n")
	body.WriteString(`Content-Disposition: form-data; name="` + filename + `"; filename="` + filename + `"` + "\r\n")
	body.WriteString("Content-Type: application/octet-stream\r\n")
	body.WriteString("Content-Length: " + strconv.Itoa(len(zipData)) + "\r\n")
	body.WriteString("\r\n")
	body.Write(zipData)
	body.WriteString("\r\n--" + boundary + "--\r\n")
	headers["Content-Type"] = []string{"multipart/form-data; boundary=" + boundary}

	response, err := c.do(ctx, http.MethodPost, c.baseURL()+target, headers, &body, uploadRequestTimeout)
	if err != nil {
		return false
	}
	return apiResponseSucceeded(response)
}

// UploadSensorFile uploads the sensor text through the AES-encrypted form
// endpoint.
func (c *Client) UploadSensorFile(ctx context.Context, sportID, sensorText string) bool {
	if err := c.Auth.HeadersConfigured(); err != nil {
		return false
	}
	aesKey, err := c.Auth.AESKey()
	if err != nil {
		return false
	}
	encrypted, err := EncryptSensorData(sensorText, aesKey, c.Auth.AESEncoding)
	if err != nil {
		return false
	}
	var form FormValues
	form.Add("sportId", sportID)
	form.Add("sensorFile", encrypted)
	headers, err := c.BuildFormHeaders(pathSensorFile, form)
	if err != nil {
		return false
	}
	response, err := c.do(ctx, http.MethodPost, c.baseURL()+pathSensorFile, headers,
		strings.NewReader(form.Encode()), uploadRequestTimeout)
	if err != nil {
		return false
	}
	return apiResponseSucceeded(response)
}

func newBoundary() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	raw[6] = raw[6]&0x0f | 0x40
	raw[8] = raw[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", raw[0:4], raw[4:6], raw[6:8], raw[8:10], raw[10:16]), nil
}

func apiResponseSucceeded(response apiResponse) bool {
	if response.status != http.StatusOK {
		return false
	}
	var body map[string]any
	if err := json.Unmarshal(response.body, &body); err != nil {
		return false
	}
	ok, _, _ := apiStatus(body)
	return ok
}

// apiStatus applies the client's business-status rules to a decoded body.
func apiStatus(body map[string]any) (bool, string, string) {
	if body == nil {
		return false, "", "响应类型异常"
	}
	code, hasCode := body["code"]
	message := ""
	if value, ok := body["message"]; ok && value != nil {
		message = fmt.Sprint(value)
	} else if value, ok := body["msg"]; ok && value != nil {
		message = fmt.Sprint(value)
	}
	if success, ok := body["success"].(bool); ok && !success {
		codeText := ""
		if hasCode && code != nil {
			codeText = fmt.Sprint(code)
		}
		if message == "" {
			message = "success=false"
		}
		return false, codeText, message
	}
	if !hasCode || code == nil {
		if success, ok := body["success"].(bool); !ok || !success {
			if message == "" {
				message = "响应缺少业务状态 code"
			}
			return false, "", message
		}
		return true, "", message
	}
	codeText := fmt.Sprint(code)
	switch codeText {
	case "0", "200":
		return true, codeText, message
	default:
		if message == "" {
			message = "未知业务错误"
		}
		return false, codeText, message
	}
}

var htmlTitlePattern = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)

func httpErrorMessage(response apiResponse) string {
	contentType := response.header.Get("Content-Type")
	summary := strings.TrimSpace(strings.ReplaceAll(strings.ReplaceAll(truncate(response.text(), 300), "\r", " "), "\n", " "))
	if strings.Contains(strings.ToLower(summary), "<html") {
		title := ""
		if match := htmlTitlePattern.FindStringSubmatch(response.text()); match != nil {
			title = strings.TrimSpace(stripTags(match[1]))
		}
		if title == "" {
			title = "HTML 拦截页"
		}
		summary = title
	}
	return fmt.Sprintf("HTTP %d (%s): %s", response.status, contentType, summary)
}

func stripTags(value string) string {
	var builder strings.Builder
	depth := 0
	for _, char := range value {
		switch char {
		case '<':
			depth++
		case '>':
			if depth > 0 {
				depth--
			}
		default:
			if depth == 0 {
				builder.WriteRune(char)
			}
		}
	}
	return builder.String()
}

func truncate(value string, limit int) string {
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

func jsonInt64(value any) (int64, error) {
	switch typed := value.(type) {
	case float64:
		return int64(typed), nil
	case string:
		return strconv.ParseInt(strings.TrimSpace(typed), 10, 64)
	case json.Number:
		return typed.Int64()
	}
	return 0, fmt.Errorf("not a number")
}
