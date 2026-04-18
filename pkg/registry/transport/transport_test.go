package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestClient_DefaultsToSecureTransport(t *testing.T) {
	resetConfig(t)

	c := Client("example.com")
	if c == nil {
		t.Fatal("Client returned nil")
	}
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is not *http.Transport, got %T", c.Transport)
	}
	if tr.TLSClientConfig == nil || tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("default transport must verify TLS")
	}
	if tr.TLSClientConfig.MinVersion < tls.VersionTLS12 {
		t.Fatal("default transport must enforce at least TLS 1.2")
	}
}

func TestClient_InsecureHostSkipsVerification(t *testing.T) {
	resetConfig(t)

	if err := Configure([]string{"flaky.internal:5000"}, ""); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	c := Client("flaky.internal:5000")
	tr := c.Transport.(*http.Transport)
	if !tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("listed host must get InsecureSkipVerify transport")
	}
	if !IsInsecure("flaky.internal:5000") {
		t.Fatal("IsInsecure should report true for configured host")
	}
	if IsInsecure("other.host") {
		t.Fatal("IsInsecure must not bleed to unlisted hosts")
	}
}

func TestClient_SecureTransportRejectsSelfSigned(t *testing.T) {
	resetConfig(t)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := Client(srv.Listener.Addr().String())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	res, err := c.Do(req)
	if err == nil {
		_ = res.Body.Close()
		t.Fatal("expected TLS verification failure against self-signed httptest server")
	}
}

func TestClient_AcceptsRegisteredCABundle(t *testing.T) {
	resetConfig(t)

	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	defer srv.Close()

	// Serialize httptest's ephemeral cert as PEM so Configure can load it.
	cert := srv.Certificate()
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})

	tmp := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(tmp, pemBytes, 0o600); err != nil {
		t.Fatalf("write CA PEM: %v", err)
	}

	if err := Configure(nil, tmp); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	c := Client(srv.Listener.Addr().String())
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL, nil)
	res, err := c.Do(req)
	if err != nil {
		t.Fatalf("expected CA-bundle trust to accept the cert, got: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", res.StatusCode)
	}
}

func TestConfigure_RejectsMissingBundle(t *testing.T) {
	resetConfig(t)

	err := Configure(nil, filepath.Join(t.TempDir(), "does-not-exist.pem"))
	if err == nil {
		t.Fatal("expected error for missing CA bundle")
	}
}

func TestConfigure_RejectsEmptyBundle(t *testing.T) {
	resetConfig(t)

	tmp := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(tmp, []byte(""), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := Configure(nil, tmp); err == nil {
		t.Fatal("expected error for PEM bundle with no certs")
	}
}

func resetConfig(t *testing.T) {
	t.Helper()
	mu.Lock()
	cfg = &Config{InsecureRegistries: map[string]struct{}{}, RootCAs: x509.NewCertPool()}
	cfg = &Config{InsecureRegistries: map[string]struct{}{}}
	rebuildTransports()
	mu.Unlock()
}
