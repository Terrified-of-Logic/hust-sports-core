package hustrun

import (
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Default endpoints and formats used when the configuration is silent.
const (
	DefaultServerURL    = "https://petzjk.hust.edu.cn/cgapp-server"
	DefaultHeaderXML    = ".credentials/HEADER.xml"
	DefaultUserInfoXML  = ".credentials/USERINFO.xml"
	DefaultSchoolXML    = ".credentials/SCHOOL.xml"
	DefaultAESEncoding  = "base64"
	DefaultTokenHeader  = "cgAuthorization"
	DefaultPhoneVersion = "Android 14"
)

// YAMLConfig is the flat section/key view of config.yaml. Only the mapping
// subset this tool needs is supported: two levels, scalars, comments.
type YAMLConfig struct {
	Top      map[string]string
	Sections map[string]map[string]string
	// Dir is the directory of the configuration file, used to resolve the
	// relative credential paths inside it.
	Dir string
}

// Get returns a value from a section, falling back to a top-level key.
func (c YAMLConfig) Get(section, key string) (string, bool) {
	if entries, ok := c.Sections[section]; ok {
		if value, ok := entries[key]; ok {
			return value, true
		}
	}
	value, ok := c.Top[key]
	return value, ok
}

// String returns a trimmed value or the fallback.
func (c YAMLConfig) String(section, key, fallback string) string {
	if value, ok := c.Get(section, key); ok && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	return fallback
}

// Bool returns a boolean value or the fallback.
func (c YAMLConfig) Bool(section, key string, fallback bool) bool {
	value, ok := c.Get(section, key)
	if !ok {
		return fallback
	}
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "on", "1":
		return true
	case "false", "no", "off", "0":
		return false
	}
	return fallback
}

// Path resolves a configured path against the configuration file's directory.
func (c YAMLConfig) Path(section, key, fallback string) string {
	value := c.String(section, key, fallback)
	if filepath.IsAbs(value) {
		return value
	}
	return filepath.Join(c.Dir, value)
}

// LoadYAMLConfig reads the mapping subset of a YAML file. A missing file yields
// an empty configuration, which keeps the credential XML files usable alone.
func LoadYAMLConfig(path string) (YAMLConfig, error) {
	config := YAMLConfig{Top: map[string]string{}, Sections: map[string]map[string]string{}}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return config, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	config.Dir = filepath.Dir(absolute)
	raw, err := os.ReadFile(absolute)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return config, nil
		}
		return config, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if err := parseSimpleYAML(string(raw), &config); err != nil {
		return config, fmt.Errorf("%w: %s: %v", ErrConfig, path, err)
	}
	return config, nil
}

func parseSimpleYAML(text string, config *YAMLConfig) error {
	var section string
	for lineNumber, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, "\r")
		cleaned := stripYAMLComment(line)
		if strings.TrimSpace(cleaned) == "" {
			continue
		}
		indent := len(cleaned) - len(strings.TrimLeft(cleaned, " \t"))
		content := strings.TrimSpace(cleaned)
		key, value, found := strings.Cut(content, ":")
		if !found {
			return fmt.Errorf("line %d: expected 'key: value'", lineNumber+1)
		}
		key = strings.TrimSpace(strings.Trim(key, `"'`))
		value = strings.TrimSpace(value)
		if value == "" && indent == 0 {
			section = key
			if _, ok := config.Sections[section]; !ok {
				config.Sections[section] = map[string]string{}
			}
			continue
		}
		value = unquoteYAML(value)
		if indent == 0 {
			config.Top[key] = value
			continue
		}
		if section == "" {
			return fmt.Errorf("line %d: nested value without a section", lineNumber+1)
		}
		config.Sections[section][key] = value
	}
	return nil
}

func stripYAMLComment(line string) string {
	var quote rune
	for index, char := range line {
		switch {
		case quote != 0:
			if char == quote {
				quote = 0
			}
		case char == '"' || char == '\'':
			quote = char
		case char == '#':
			if index == 0 {
				return ""
			}
			if previous := rune(line[index-1]); previous == ' ' || previous == '\t' {
				return line[:index]
			}
		}
	}
	return line
}

func unquoteYAML(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') ||
			(value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

// ReadSPXML reads the Android SharedPreferences <string> entries of a file.
func ReadSPXML(path string) (map[string]string, error) {
	out := map[string]string{}
	raw, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, nil
		}
		return out, fmt.Errorf("%w: %v", ErrConfig, err)
	}
	var document struct {
		Strings []struct {
			Name  string `xml:"name,attr"`
			Value string `xml:",chardata"`
		} `xml:"string"`
	}
	if err := xml.Unmarshal(raw, &document); err != nil {
		return out, fmt.Errorf("%w: %s: %v", ErrConfig, path, err)
	}
	for _, entry := range document.Strings {
		out[entry.Name] = entry.Value
	}
	return out, nil
}

// LoadAuthConfig assembles the credential set from config.yaml plus the three
// SharedPreferences files the official app writes.
func LoadAuthConfig(configPath string) (*AuthConfig, error) {
	yaml, err := LoadYAMLConfig(configPath)
	if err != nil {
		return nil, err
	}
	header, err := ReadSPXML(yaml.Path("paths", "header_xml", DefaultHeaderXML))
	if err != nil {
		return nil, err
	}
	userInfo, err := ReadSPXML(yaml.Path("paths", "userinfo_xml", DefaultUserInfoXML))
	if err != nil {
		return nil, err
	}
	school, err := ReadSPXML(yaml.Path("paths", "school_xml", DefaultSchoolXML))
	if err != nil {
		return nil, err
	}

	serverURL := yaml.String("auth", "server_url", "")
	if serverURL == "" {
		serverURL = strings.TrimSpace(school["SERVERURL"])
	}
	if serverURL == "" {
		serverURL = DefaultServerURL
	}

	auth := &AuthConfig{
		Token:        yaml.String("auth", "token", header["TOKEN"]),
		Secret:       yaml.String("auth", "secret", header["SECRET"]),
		Sign:         yaml.String("auth", "sign", header["SIGN"]),
		Timestamp:    yaml.String("auth", "timestamp", header["CGAPP_TIME_KEY"]),
		WebViewTime:  yaml.String("auth", "web_view_time", header["WEB_VIEW_TIME"]),
		LoginOutTime: yaml.String("auth", "login_out_time", header["LOGIN_OUT_TIME"]),

		XH:   userInfo["USERXH"],
		Name: userInfo["USERNAME"],

		ServerURL: strings.TrimRight(serverURL, "/"),

		AESEncoding:  yaml.String("upload", "aes_encoding", DefaultAESEncoding),
		TokenHeader:  yaml.String("upload", "token_header", DefaultTokenHeader),
		SkipSensor:   yaml.Bool("upload", "skip_sensor", false),
		IMEI:         yaml.String("device", "imei", ""),
		OAID:         yaml.String("device", "oaid", ""),
		UserAgent:    yaml.String("device", "user_agent", ""),
		AppKey:       yaml.String("device", "app_key", ""),
		V1Seed:       yaml.String("device", "v1_seed", ""),
		IsRoot:       yaml.Bool("device", "is_root", false),
		PhoneVersion: yaml.String("device", "phone_version", DefaultPhoneVersion),
		SensorPath:   yaml.String("data", "sensor_path", ""),
		SensorSHA256: yaml.String("data", "sensor_sha256", ""),
	}
	return auth, nil
}

// SaveHeaderXML writes the credentials a successful CAS login produced into the
// SharedPreferences file, backing up the previous file when asked. Fields bound
// to an older SECRET are cleared instead of silently reused.
func SaveHeaderXML(path string, result LoginResult, backup bool) error {
	if !result.Success {
		return fmt.Errorf("%w: refusing to persist a failed login", ErrConfig)
	}
	existing, err := ReadSPXML(path)
	if err != nil {
		return err
	}
	if backup {
		if raw, readErr := os.ReadFile(path); readErr == nil {
			if writeErr := os.WriteFile(path+".bak", raw, 0o600); writeErr != nil {
				return fmt.Errorf("%w: backup: %v", ErrConfig, writeErr)
			}
		}
	}
	updates := map[string]string{
		"TOKEN":          result.Token,
		"SECRET":         result.Secret,
		"LOGIN_OUT_TIME": result.LoginOutTime,
		"SIGN":           result.Sign,
		"WEB_VIEW_TIME":  result.WebViewTime,
		"CGAPP_TIME_KEY": result.Timestamp,
	}
	order := []string{"TOKEN", "SECRET", "LOGIN_OUT_TIME", "SIGN", "WEB_VIEW_TIME", "CGAPP_TIME_KEY"}
	var builder strings.Builder
	builder.WriteString(xmlDeclaration)
	builder.WriteString("<map>\n")
	written := map[string]bool{}
	for _, name := range order {
		// Legacy fields (SIGN / WEB_VIEW_TIME / CGAPP_TIME_KEY) are bound to the
		// previous SECRET: clearing them is deliberate, not a loss.
		written[name] = true
		builder.WriteString(spEntry(name, updates[name]))
	}
	for name, value := range existing {
		if !written[name] {
			builder.WriteString(spEntry(name, value))
		}
	}
	builder.WriteString("</map>\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	if err := os.WriteFile(path, []byte(builder.String()), 0o600); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return nil
}

const xmlDeclaration = `<?xml version="1.0" encoding="utf-8" standalone="yes"?>` + "\n"

func spEntry(name, value string) string {
	var escaped strings.Builder
	if err := xml.EscapeText(&escaped, []byte(value)); err != nil {
		escaped.WriteString(value)
	}
	return `    <string name="` + name + `">` + escaped.String() + "</string>\n"
}

// EnsureSPXML creates an empty SharedPreferences file when it is missing.
func EnsureSPXML(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return os.WriteFile(path, []byte(xmlDeclaration+"<map>\n</map>\n"), 0o600)
}

// WriteUserInfo writes the student identity file, preserving an existing role
// list.
func WriteUserInfo(path, xh, name string) error {
	roles := "1"
	if existing, err := ReadSPXML(path); err == nil {
		if value, ok := existing["USER_ROLES"]; ok && value != "" {
			roles = value
		}
	}
	var builder strings.Builder
	builder.WriteString(xmlDeclaration)
	builder.WriteString("<map>\n")
	builder.WriteString(spEntry("USERXH", xh))
	builder.WriteString(spEntry("USERNAME", name))
	builder.WriteString(spEntry("USER_ROLES", roles))
	builder.WriteString("</map>\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("%w: %v", ErrConfig, err)
	}
	return os.WriteFile(path, []byte(builder.String()), 0o600)
}

// isJWTExpired reports whether the token is unusable: unparsable or past exp.
func isJWTExpired(token string, now time.Time) bool {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return true
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return true
	}
	var claims struct {
		Exp json.Number `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return true
	}
	expiry, err := strconv.ParseFloat(claims.Exp.String(), 64)
	if err != nil {
		return true
	}
	return float64(now.Unix()) > expiry
}
