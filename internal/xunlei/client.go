package xunlei

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

const apiPrefix = "/webman/3rdparty/pan-xunlei-com/index.cgi"
const maxResponseBytes = 16 << 20
const maxTorrentBytes = 16 << 20

var tokenPattern = regexp.MustCompile(`(?s)(?:function\s+uiauth\s*\([^)]*\)|(?:window\.)?uiauth\s*=\s*function\s*\([^)]*\))\s*\{\s*return\s*["']([^"'\r\n]+)["']`)

type Client struct {
	base               *url.URL
	username, password string
	http               *http.Client
	mu                 sync.Mutex
	token              string
	authenticatedAt    time.Time
	device             Device
}

func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.BaseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, invalid("base URL must be an HTTP(S) origin or proxy path without credentials, query, or fragment")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	if !strings.HasSuffix(u.Path, "/index.cgi") {
		u.Path += apiPrefix
	}
	u.RawPath = ""
	h := http.Client{Timeout: 30 * time.Second}
	if cfg.HTTPClient != nil {
		h = *cfg.HTTPClient
		if h.Timeout <= 0 {
			h.Timeout = 30 * time.Second
		}
	}
	// Prevent credentials (including custom pan-auth headers) crossing origins.
	h.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 || req.URL.Scheme != u.Scheme || req.URL.Host != u.Host || (len(via) > 0 && via[0].Method != http.MethodGet && via[0].Method != http.MethodHead) {
			return http.ErrUseLastResponse
		}
		return nil
	}
	return &Client{base: u, username: cfg.Username, password: cfg.Password, http: &h}, nil
}

func invalid(message string) *Error { return &Error{Kind: "invalid", Message: message, Status: 400} }
func malformed(mutation bool) *Error {
	if mutation {
		return &Error{Kind: "unknown_outcome", Message: "upstream response could not be verified; do not repeat the mutation automatically", Status: 502}
	}
	return &Error{Kind: "upstream", Message: "upstream returned an unexpected response", Status: 502}
}

// networkReason returns a fixed vocabulary only. In particular url.Error.Error()
// contains the authenticated URL and must never be returned or logged.
func networkReason(err error) string {
	for i := 0; i < 8; i++ {
		wrapped, ok := err.(*url.Error)
		if !ok {
			break
		}
		err = wrapped.Err
	}
	if errors.Is(err, context.Canceled) {
		return "request_canceled"
	}
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return "timeout"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof"
	}
	if errors.Is(err, io.EOF) {
		return "eof"
	}
	if errors.Is(err, syscall.ECONNREFUSED) {
		return "connection_refused"
	}
	if errors.Is(err, syscall.ECONNRESET) {
		return "connection_reset"
	}
	if errors.Is(err, syscall.EPIPE) {
		return "broken_pipe"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return "dns_failure"
	}
	var unknownAuthority x509.UnknownAuthorityError
	var invalidCertificate x509.CertificateInvalidError
	var hostname x509.HostnameError
	if errors.As(err, &unknownAuthority) || errors.As(err, &invalidCertificate) || errors.As(err, &hostname) {
		return "tls_certificate"
	}
	var tlsRecord tls.RecordHeaderError
	if errors.As(err, &tlsRecord) {
		return "tls_handshake"
	}
	var protocol *http.ProtocolError
	if errors.As(err, &protocol) {
		return "protocol_error"
	}
	return "transport_error"
}
func networkFailure(mutation bool, err error) *Error {
	reason := networkReason(err)
	if mutation {
		return &Error{Kind: "unknown_outcome", Message: "upstream request did not return a reliable response (" + reason + "); the mutation may have been accepted", Status: 502}
	}
	return &Error{Kind: "unavailable", Message: "upstream connection failed (" + reason + ")", Status: 503}
}

func (c *Client) exchange(ctx context.Context, method, path, token string, q url.Values, body []byte, contentType string, mutation bool) ([]byte, error) {
	u := *c.base
	u.RawPath = c.base.EscapedPath() + path
	u.Path, _ = url.PathUnescape(u.RawPath)
	values := url.Values{}
	for k, v := range q {
		values[k] = append([]string(nil), v...)
	}
	if token != "" {
		values.Set("pan_auth", token)
		values.Set("device_space", "")
	}
	u.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, invalid("invalid upstream request")
	}
	if token != "" {
		req.Header.Set("pan-auth", token)
		req.Header.Set("Device-Space", "")
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if c.username != "" || c.password != "" {
		req.SetBasicAuth(c.username, c.password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, networkFailure(mutation, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return nil, malformed(mutation)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		kind, status, msg := "upstream", 502, "upstream rejected the request"
		switch resp.StatusCode {
		case 401, 403:
			kind, status, msg = "unauthorized", 502, "upstream authentication was rejected"
		case 404:
			kind, status, msg = "not_found", 404, "upstream resource was not found"
		case 400, 422:
			kind, status, msg = "invalid", 400, "upstream rejected the request parameters"
		default:
			if mutation && (resp.StatusCode >= 500 || resp.StatusCode == 408 || (resp.StatusCode >= 300 && resp.StatusCode < 400)) {
				kind, msg = "unknown_outcome", "upstream server failed; the mutation outcome is unknown"
			}
		}
		return nil, &Error{Kind: kind, Status: status, Message: msg}
	}
	return data, nil
}

func decodeResponse(data []byte, mutation bool) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil || obj == nil {
		return nil, malformed(mutation)
	}
	failure := ""
	for _, key := range []string{"error", "error_code", "code", "HttpStatus"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		v := scalar(raw)
		if v != "" && v != "0" && v != "null" && v != "false" && v != "200" && v != "OK" && v != "ok" {
			failure = v
			if raw[0] == '{' || raw[0] == '[' {
				failure = "upstream_business_error"
			}
			break
		}
	}
	if raw, ok := obj["success"]; ok && string(raw) == "false" {
		failure = "failed"
	}
	if failure != "" {
		kind, status, msg := "upstream", 502, "upstream reported a business error"
		lower := strings.ToLower(failure)
		if strings.Contains(lower, "unauth") || strings.Contains(lower, "token") || lower == "401" || lower == "403" {
			kind, msg = "unauthorized", "upstream authentication was rejected"
		}
		if lower == "not_found" || lower == "404" || lower == "file_not_found" || lower == "task_not_found" {
			kind, status, msg = "not_found", 404, "upstream resource was not found"
		}
		return nil, &Error{Kind: kind, Status: status, Message: msg}
	}
	return obj, nil
}

func (c *Client) authenticated(ctx context.Context) (string, Device, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Since(c.authenticatedAt) < 9*time.Minute {
		return c.token, c.device, nil
	}
	page, err := c.exchange(ctx, http.MethodGet, "/", "", nil, nil, "", false)
	if err != nil {
		return "", Device{}, err
	}
	match := tokenPattern.FindSubmatch(page)
	if len(match) != 2 {
		return "", Device{}, &Error{Kind: "unauthorized", Status: 502, Message: "could not discover the Xunlei web token; engine 3.21.0 or newer is required"}
	}
	token := html.UnescapeString(string(match[1]))
	if strings.ContainsAny(token, "\r\n") {
		return "", Device{}, malformed(false)
	}
	data, err := c.exchange(ctx, http.MethodPost, "/device/info/watch", token, nil, []byte("{}"), "application/json", false)
	if err != nil {
		return "", Device{}, err
	}
	obj, err := decodeResponse(data, false)
	if err != nil {
		return "", Device{}, err
	}
	device, err := decodeDevice(obj)
	if err != nil {
		return "", Device{}, err
	}
	// Launcher status is advisory: some wrapper versions expose only client_version.
	versionData, verr := c.exchange(ctx, http.MethodGet, "/launcher/status", token, nil, nil, "", false)
	if verr == nil {
		if ver, vErr := decodeResponse(versionData, false); vErr == nil && scalar(ver["running_version"]) != "" {
			device.Version = scalar(ver["running_version"])
		}
	}
	c.token, c.device, c.authenticatedAt = token, device, time.Now()
	return token, device, nil
}
func (c *Client) invalidate(token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == token {
		c.token = ""
		c.authenticatedAt = time.Time{}
	}
}

// call retries only read operations after an explicit authentication rejection.
// Resource resolution uses POST but is a read operation. Mutations never retry.
func (c *Client) call(ctx context.Context, method, path string, q url.Values, payload any, mutation bool) (map[string]json.RawMessage, error) {
	var body []byte
	if payload != nil {
		var err error
		body, err = json.Marshal(payload)
		if err != nil {
			return nil, invalid("request cannot be encoded")
		}
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, device, err := c.authenticated(ctx)
		if err != nil {
			return nil, err
		}
		bound := q.Get("space")
		if len(body) > 0 {
			var fields map[string]json.RawMessage
			if json.Unmarshal(body, &fields) == nil && scalar(fields["space"]) != "" {
				bound = scalar(fields["space"])
			}
		}
		if bound != "" && bound != device.ID {
			return nil, &Error{Kind: "unavailable", Status: 503, Message: "the upstream device changed; refresh device and resource information before retrying"}
		}
		data, err := c.exchange(ctx, method, path, token, q, body, "application/json", mutation)
		var obj map[string]json.RawMessage
		if err == nil {
			obj, err = decodeResponse(data, mutation)
		}
		var upstreamErr *Error
		if errors.As(err, &upstreamErr) && upstreamErr.Kind == "unauthorized" {
			c.invalidate(token)
			if !mutation && attempt == 0 {
				continue
			}
		}
		return obj, err
	}
	return nil, malformed(false)
}

func decodeDevice(obj map[string]json.RawMessage) (Device, error) {
	var user map[string]json.RawMessage
	_ = json.Unmarshal(obj["user"], &user)
	d := Device{ID: scalar(obj["target"]), Version: scalar(obj["client_version"]), Online: scalar(obj["is_connected"]) == "true", LoggedIn: scalar(user["id"]) != "" && scalar(user["id"]) != "0", Volumes: []Volume{}}
	if d.ID == "" || d.ID == "null" {
		return Device{}, &Error{Kind: "unavailable", Status: 503, Message: "Xunlei did not report a local device identifier"}
	}
	var downloads []map[string]json.RawMessage
	_ = json.Unmarshal(obj["downloads"], &downloads)
	for _, v := range downloads {
		d.Volumes = append(d.Volumes, Volume{Path: scalar(v["path"]), CapacityBytes: int64Value(v["limit"]), UsedBytes: int64Value(v["usage"])})
	}
	return d, nil
}
func (c *Client) Device(ctx context.Context) (Device, error) {
	obj, err := c.call(ctx, http.MethodPost, "/device/info/watch", nil, map[string]any{}, false)
	if err != nil {
		return Device{}, err
	}
	d, err := decodeDevice(obj)
	if err != nil {
		return Device{}, err
	}
	c.mu.Lock()
	if d.Version == "" {
		d.Version = c.device.Version
	}
	c.device = d
	c.mu.Unlock()
	return d, nil
}
