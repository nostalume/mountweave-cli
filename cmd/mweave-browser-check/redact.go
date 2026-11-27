package main

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
)

const (
	maxDiagnosticBodyBytes = 16 << 10
	diagnosticRedacted     = "[redacted]"
)

var (
	diagnosticJWT            = regexp.MustCompile(`(?:^|[^a-zA-Z0-9_-])[a-zA-Z0-9_-]{8,}\.[a-zA-Z0-9_-]{8,}\.[a-zA-Z0-9_-]{8,}(?:$|[^a-zA-Z0-9_-])`)
	diagnosticCredentialText = regexp.MustCompile(`(?i)\b(?:bearer|basic)\s+[a-z0-9_+/.-]{4,}|\b(?:access[_-]?token|refresh[_-]?token|token|password|passwd|cookie|secret|signature|authorization|sig|x-amz-[a-z-]+)\s*[:=]`)
)

// diagnosticEndpoint keeps the route shape without including URL credentials.
func diagnosticEndpoint(raw string) string {
	if len(raw) > 8<<10 {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.Opaque != "" || u.Scheme != "https" && u.Scheme != "http" {
		return ""
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	parts := strings.Split(path, "/")
	redactNext := false
	for i, part := range parts {
		decoded, err := url.PathUnescape(part)
		key := diagnosticKey(decoded)
		if err != nil || redactNext && part != "" && !diagnosticRouteWord(key) || strings.ContainsAny(decoded, "?#@\\/%") ||
			diagnosticOpaqueValue(decoded) || diagnosticCredentialText.MatchString(decoded) {
			parts[i] = "_redacted_"
			redactNext = false
			continue
		}
		redactNext = diagnosticCredentialKey(key) || redactNext && part == ""
	}
	return u.Scheme + "://" + strings.ToLower(u.Host) + strings.Join(parts, "/")
}

func diagnosticRouteWord(key string) bool {
	switch key {
	case "auth", "token", "tokens", "refreshtoken", "accesstoken", "authorize", "refresh", "exchange", "callback", "verify", "login", "logout", "validate":
		return true
	default:
		return false
	}
}

// diagnosticJSON accepts only small structured bodies. It never falls back to
// recording text or the original bytes if parsing or projection fails.
func diagnosticJSON(raw []byte) json.RawMessage {
	if len(raw) > maxDiagnosticBodyBytes {
		return nil
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || raw[0] != '{' && raw[0] != '[' {
		return nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var data any
	if decoder.Decode(&data) != nil || len(bytes.TrimSpace(raw[decoder.InputOffset():])) != 0 {
		return nil
	}
	projected, err := json.Marshal(redactDiagnosticValue(data, 0))
	if err != nil || len(projected) > maxDiagnosticBodyBytes {
		return nil
	}
	return projected
}

func redactDiagnosticValue(value any, depth int) any {
	if depth >= 64 {
		return diagnosticRedacted
	}
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if diagnosticSensitiveText(key) || diagnosticOpaqueValue(key) {
				delete(value, key)
				continue
			}
			label := diagnosticKey(key)
			switch child.(type) {
			case nil, bool:
				continue
			}
			_, text := child.(string)
			_, number := child.(json.Number)
			if diagnosticCredentialKey(label) || text && diagnosticURLKey(label) || label == "code" && !number {
				value[key] = diagnosticRedacted
				continue
			}
			value[key] = redactDiagnosticValue(child, depth+1)
		}
	case []any:
		for i, child := range value {
			value[i] = redactDiagnosticValue(child, depth+1)
		}
	case string:
		if diagnosticSensitiveText(value) {
			return diagnosticRedacted
		}
	}
	return value
}

func diagnosticKey(key string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case 'A' <= r && r <= 'Z':
			return r + 'a' - 'A'
		case 'a' <= r && r <= 'z' || '0' <= r && r <= '9':
			return r
		default:
			return -1
		}
	}, key)
}

func diagnosticCredentialKey(key string) bool {
	switch key {
	case "auth", "authcode", "bduss", "dykey", "session", "sessionid", "sid", "jsessionid",
		"codeverifier", "csrf", "xsrf", "pin", "otp", "apikey", "key", "keypair", "privatekey", "pass",
		"sig", "sign", "signature", "credential", "ticket", "sastoken":
		return true
	}
	for _, component := range [...]string{"authorization", "credential", "token", "password", "passwd", "cookie", "secret", "signature"} {
		if strings.Contains(key, component) {
			return true
		}
	}
	return false
}

func diagnosticURLKey(key string) bool {
	switch key {
	case "href", "location", "downloadpath", "downloadaddress", "prefix":
		return true
	}
	return strings.HasSuffix(key, "url") || strings.HasSuffix(key, "uri") || strings.HasSuffix(key, "link")
}

func diagnosticSensitiveText(value string) bool {
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.Contains(lower, "://") || strings.HasPrefix(lower, "//") ||
		strings.HasPrefix(lower, "data:") || strings.HasPrefix(lower, "blob:") ||
		strings.Contains(lower, "-----begin ") || diagnosticJWT.MatchString(value) ||
		diagnosticCredentialText.MatchString(value) || strings.ContainsAny(lower, "?#") && strings.Contains(lower, "=")
}

func diagnosticOpaqueValue(value string) bool {
	if len(value) < 24 {
		return false
	}
	hex := true
	var lower, upper, digit bool
	for _, c := range value {
		switch {
		case 'a' <= c && c <= 'z':
			lower = true
			hex = hex && c <= 'f'
		case 'A' <= c && c <= 'Z':
			upper = true
			hex = hex && c <= 'F'
		case '0' <= c && c <= '9':
			digit = true
		case c == '-' || c == '_' || c == '+' || c == '=':
			hex = false
		default:
			return diagnosticJWT.MatchString(value)
		}
	}
	return hex || len(value) >= 32 && lower && upper && digit
}
