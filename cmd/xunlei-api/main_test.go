package main

import (
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSecretFileAndConflict(t *testing.T) {
	t.Setenv("TEST_SECRET", "")
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte("a-secret-value\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_SECRET_FILE", path)
	got, err := secret("TEST_SECRET")
	if err != nil || got != "a-secret-value" {
		t.Fatalf("file secret: %q, %v", got, err)
	}
	t.Setenv("TEST_SECRET", "must-not-appear")
	if _, err = secret("TEST_SECRET"); err == nil || strings.Contains(err.Error(), "must-not-appear") {
		t.Fatal("conflicting sources must fail without leaking value")
	}
	t.Setenv("TEST_SECRET", "")
	if err := os.WriteFile(path, []byte(strings.Repeat("a", 4097)), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = secret("TEST_SECRET"); err == nil {
		t.Fatal("oversized secret accepted")
	}
}
func TestDurationBounds(t *testing.T) {
	for _, value := range []string{"1ms", "0s", "-1s", "31m", "invalid"} {
		t.Setenv("TEST_DURATION", value)
		if _, err := duration("TEST_DURATION", time.Second); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	t.Setenv("TEST_DURATION", "30s")
	if d, err := duration("TEST_DURATION", time.Second); err != nil || d != 30*time.Second {
		t.Fatalf("%v %v", d, err)
	}
}
func TestHealthcheckHTTPAndTLS(t *testing.T) {
	for _, secure := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "tls"}[secure], func(t *testing.T) {
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/healthz" || r.Header.Get("Authorization") != "" {
					t.Error("invalid health request")
				}
				w.WriteHeader(http.StatusOK)
			})
			var server *httptest.Server
			if secure {
				server = httptest.NewTLSServer(handler)
			} else {
				server = httptest.NewServer(handler)
			}
			defer server.Close()
			u, _ := url.Parse(server.URL)
			t.Setenv("LISTEN_ADDR", u.Host)
			t.Setenv("TLS_CERT_FILE", "")
			t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
			if secure {
				path := filepath.Join(t.TempDir(), "cert.pem")
				data := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
				if err := os.WriteFile(path, data, 0600); err != nil {
					t.Fatal(err)
				}
				t.Setenv("TLS_CERT_FILE", path)
			}
			if err := healthcheck(); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestHealthcheckRejectsFailure(t *testing.T) {
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer s.Close()
	u, _ := url.Parse(s.URL)
	t.Setenv("LISTEN_ADDR", u.Host)
	t.Setenv("TLS_CERT_FILE", "")
	if healthcheck() == nil {
		t.Fatal("unhealthy status accepted")
	}
}
