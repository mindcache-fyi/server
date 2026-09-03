package service

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// TestSignPayload_GoldenVector pins the canonical signature scheme. The
// expected value is produced independently by Node's crypto module and is
// shared with the gateway's client_auth_example.js, so any drift between the
// Go signer, the TypeScript gateway, and the JS client fails here.
func TestSignPayload_GoldenVector(t *testing.T) {
	got := SignPayload(
		"test-shared-salt",
		"POST",
		"/v1/chat/completions",
		"1750000000",
		"abcdef0123456789abcdef0123456789",
		"device-0001",
		[]byte(`{"model":"foo/bar:free","stream":true}`),
	)
	want := "cd8c9712c912921ab66298c044a5472c93bf7ec7d6168f893a983191e749b068"
	if got != want {
		t.Fatalf("SignPayload = %q, want %q", got, want)
	}
}

func TestNewRequestSigner_DisabledWithoutSalt(t *testing.T) {
	if signer := NewRequestSigner("", "device-0001"); signer != nil {
		t.Fatal("NewRequestSigner with empty salt must return nil")
	}
	var signer *RequestSigner
	if got := signer.Transport(http.DefaultTransport); got != http.DefaultTransport {
		t.Fatal("nil signer Transport must pass the wrapped transport through")
	}
}

func TestSigningTransport_SignsRequestAndPreservesBody(t *testing.T) {
	type captured struct {
		method   string
		path     string
		body     string
		ts       string
		nonce    string
		deviceID string
		sig      string
	}
	var got captured

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		got = captured{
			method:   r.Method,
			path:     r.URL.Path,
			body:     string(body),
			ts:       r.Header.Get("X-Timestamp"),
			nonce:    r.Header.Get("X-Nonce"),
			deviceID: r.Header.Get("X-Device-ID"),
			sig:      r.Header.Get("X-Signature"),
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	signer := NewRequestSigner("test-shared-salt", "device-0001")
	client := &http.Client{Transport: signer.Transport(nil)}

	reqBody := `{"model":"foo/bar:free","messages":[]}`
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	if got.body != reqBody {
		t.Fatalf("body = %q, want %q", got.body, reqBody)
	}
	if got.deviceID != "device-0001" {
		t.Fatalf("device id = %q, want %q", got.deviceID, "device-0001")
	}
	if !regexp.MustCompile(`^\d+$`).MatchString(got.ts) {
		t.Fatalf("timestamp = %q, want unix seconds", got.ts)
	}
	if !regexp.MustCompile(`^[a-f0-9]{32}$`).MatchString(got.nonce) {
		t.Fatalf("nonce = %q, want 32 hex chars", got.nonce)
	}
	wantSig := SignPayload("test-shared-salt", got.method, got.path, got.ts, got.nonce, got.deviceID, []byte(got.body))
	if got.sig != wantSig {
		t.Fatalf("signature = %q, want %q", got.sig, wantSig)
	}
}

func TestSigningTransport_EmptyBodySignsEmptyHash(t *testing.T) {
	var sig, ts, nonce string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sig = r.Header.Get("X-Signature")
		ts = r.Header.Get("X-Timestamp")
		nonce = r.Header.Get("X-Nonce")
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	signer := NewRequestSigner("test-shared-salt", "")
	client := &http.Client{Transport: signer.Transport(nil)}

	resp, err := client.Get(srv.URL + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()

	want := SignPayload("test-shared-salt", http.MethodGet, "/v1/models", ts, nonce, "", nil)
	if sig != want {
		t.Fatalf("signature = %q, want %q", sig, want)
	}
}
