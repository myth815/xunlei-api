package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/myth815/xunlei-api/docs"
	"github.com/myth815/xunlei-api/internal/api"
	"github.com/myth815/xunlei-api/internal/store"
	"github.com/myth815/xunlei-api/internal/xunlei"
)

var version = "dev"
var commit = "unknown"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "version", "--version":
			_ = json.NewEncoder(os.Stdout).Encode(map[string]string{"version": version, "commit": commit})
			return
		case "keygen":
			var key [32]byte
			if _, err := rand.Read(key[:]); err != nil {
				os.Exit(1)
			}
			fmt.Println(base64.RawURLEncoding.EncodeToString(key[:]))
			return
		case "healthcheck":
			if err := healthcheck(); err != nil {
				fmt.Fprintln(os.Stderr, "Health check failed")
				os.Exit(1)
			}
			return
		case "serve":
		default:
			fmt.Fprintln(os.Stderr, "Usage: xunlei-api [serve|version|keygen|healthcheck]")
			os.Exit(2)
		}
	}
	if err := run(); err != nil {
		slog.Error("service stopped", "error", err.Error())
		os.Exit(1)
	}
}

func env(name, defaultValue string) string {
	if v, ok := os.LookupEnv(name); ok {
		return v
	}
	return defaultValue
}
func secret(name string) (string, error) {
	value, path := os.Getenv(name), os.Getenv(name+"_FILE")
	if value != "" && path != "" {
		return "", fmt.Errorf("set only %s or %s_FILE", name, name)
	}
	if path != "" {
		f, e := os.Open(path)
		if e != nil {
			return "", fmt.Errorf("cannot read %s_FILE", name)
		}
		defer f.Close()
		b, e := io.ReadAll(io.LimitReader(f, 4097))
		if e != nil || len(b) > 4096 {
			return "", fmt.Errorf("invalid %s_FILE", name)
		}
		value = strings.TrimRight(string(b), "\r\n")
	}
	return value, nil
}
func duration(name string, defaultValue time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return defaultValue, nil
	}
	d, e := time.ParseDuration(v)
	if e != nil || d < time.Second || d > 30*time.Minute {
		return 0, fmt.Errorf("%s must be a duration between 1s and 30m", name)
	}
	return d, nil
}

func run() error {
	key, err := secret("API_KEY")
	if err != nil {
		return err
	}
	password, err := secret("XUNLEI_PASSWORD")
	if err != nil {
		return err
	}
	timeout, err := duration("UPSTREAM_TIMEOUT", 30*time.Second)
	if err != nil {
		return err
	}
	operationTimeout, err := duration("OPERATION_TIMEOUT", 2*time.Minute)
	if err != nil {
		return err
	}
	addr := env("LISTEN_ADDR", ":8080")
	cert, keyFile := os.Getenv("TLS_CERT_FILE"), os.Getenv("TLS_KEY_FILE")
	if (cert == "") != (keyFile == "") {
		return errors.New("TLS_CERT_FILE and TLS_KEY_FILE must be set together")
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = timeout
	backend, err := xunlei.New(xunlei.Config{BaseURL: env("XUNLEI_BASE_URL", "http://xunlei:2345"), Username: os.Getenv("XUNLEI_USERNAME"), Password: password, HTTPClient: &http.Client{Timeout: timeout, Transport: transport}})
	if err != nil {
		return err
	}
	st, err := store.Open(env("DATA_DIR", "/data"))
	if err != nil {
		return err
	}
	defer st.Close()
	origins := []string{}
	for _, o := range strings.Split(os.Getenv("CORS_ALLOWED_ORIGINS"), ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	app, err := api.New(api.Config{
		APIKey:                 key,
		AllowedOrigins:         origins,
		RequestTimeout:         timeout + 15*time.Second,
		OperationTimeout:       operationTimeout,
		OpenAPI:                docs.OpenAPI,
		DefaultDestinationPath: os.Getenv("DEFAULT_DESTINATION_PATH"),
	}, backend, st)
	if err != nil {
		return err
	}
	defer app.Close()
	app.Start()
	server := &http.Server{Addr: addr, Handler: app.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: timeout + 15*time.Second, WriteTimeout: 2*timeout + 45*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10, TLSConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() {
		if cert != "" {
			done <- server.ListenAndServeTLS(cert, keyFile)
		} else {
			done <- server.ListenAndServe()
		}
	}()
	slog.Info("xunlei-api started", "version", version, "listen", addr, "tls", cert != "")
	select {
	case err = <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("HTTP listener failed; check the listen address and TLS files")
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 2*timeout+20*time.Second)
		defer cancel()
		if err = server.Shutdown(shutdown); err != nil {
			_ = server.Close()
			return errors.New("HTTP shutdown timed out")
		}
		return nil
	}
}

func healthcheck() error {
	host, port, err := net.SplitHostPort(env("LISTEN_ADDR", ":8080"))
	if err != nil {
		return err
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	scheme := "http"
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	if certPath := os.Getenv("TLS_CERT_FILE"); certPath != "" {
		data, e := os.ReadFile(certPath)
		if e != nil {
			return e
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(data) {
			return errors.New("invalid TLS certificate")
		}
		block, _ := pem.Decode(data)
		if block == nil {
			return errors.New("invalid TLS certificate")
		}
		cert, e := x509.ParseCertificate(block.Bytes)
		if e != nil {
			return e
		}
		name := host
		if len(cert.DNSNames) > 0 {
			name = cert.DNSNames[0]
		} else if len(cert.IPAddresses) > 0 {
			name = cert.IPAddresses[0].String()
		}
		transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: name}
		scheme = "https"
	}
	client := &http.Client{Timeout: 4 * time.Second, Transport: transport}
	resp, err := client.Get(scheme + "://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return errors.New("unhealthy")
	}
	return nil
}
