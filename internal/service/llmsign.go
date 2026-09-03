package service

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strconv"
	"time"
)

// RequestSigner adds HMAC authentication headers to outbound LLM requests,
// as required by trusted gateways such as MindCache Try. A nil
// *RequestSigner is valid and performs no signing, which keeps user-supplied
// (BYOK) endpoints on their original behaviour.
type RequestSigner struct {
	salt     string
	deviceID string
	now      func() time.Time
}

// NewRequestSigner returns a signer for the given pre-shared salt, or nil
// when salt is empty (signing disabled). deviceID identifies this
// installation for the gateway's rate limiting.
func NewRequestSigner(salt, deviceID string) *RequestSigner {
	if salt == "" {
		return nil
	}
	return &RequestSigner{salt: salt, deviceID: deviceID, now: time.Now}
}

// SignPayload computes the canonical HMAC-SHA256 signature (lowercase hex).
// The payload format must stay byte-identical across the gateway and every
// client implementation:
//
//	METHOD "\n" PATH "\n" TIMESTAMP "\n" NONCE "\n" DEVICE_ID "\n" SHA256_HEX(BODY)
func SignPayload(salt, method, path, timestamp, nonce, deviceID string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	payload := method + "\n" + path + "\n" + timestamp + "\n" + nonce + "\n" + deviceID + "\n" + hex.EncodeToString(bodyHash[:])
	mac := hmac.New(sha256.New, []byte(salt))
	mac.Write([]byte(payload))
	return hex.EncodeToString(mac.Sum(nil))
}

// Transport wraps next with a signing RoundTripper. A nil receiver returns
// next unchanged; a nil next falls back to http.DefaultTransport.
func (s *RequestSigner) Transport(next http.RoundTripper) http.RoundTripper {
	if s == nil {
		return next
	}
	if next == nil {
		next = http.DefaultTransport
	}
	return &signingTransport{signer: s, next: next}
}

type signingTransport struct {
	signer *RequestSigner
	next   http.RoundTripper
}

func (t *signingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		_ = req.Body.Close()
		body = b
	}

	timestamp := strconv.FormatInt(t.signer.now().Unix(), 10)
	nonce, err := randomNonce()
	if err != nil {
		return nil, err
	}

	req.Header.Set("X-Timestamp", timestamp)
	req.Header.Set("X-Nonce", nonce)
	if t.signer.deviceID != "" {
		req.Header.Set("X-Device-ID", t.signer.deviceID)
	}
	req.Header.Set("X-Signature", SignPayload(
		t.signer.salt, req.Method, req.URL.Path, timestamp, nonce, t.signer.deviceID, body,
	))

	if body != nil {
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.GetBody = func() (io.ReadCloser, error) {
			return io.NopCloser(bytes.NewReader(body)), nil
		}
	}
	return t.next.RoundTrip(req)
}

func randomNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
