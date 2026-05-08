package extraction

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fakeSidecar mocks the extraction sidecar's HTTP API. /v1/extract
// returns whatever response the test configures via the responses map.
func fakeSidecar(t *testing.T, status int, body any, capture *http.Header) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/readyz":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"tikaReady":true}`))
		case "/v1/extract":
			if capture != nil {
				*capture = r.Header.Clone()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewClient_UnsetEnv(t *testing.T) {
	t.Setenv(EnvBaseURL, "")
	if NewClient() != nil {
		t.Errorf("expected nil client when EXTRACTION_URL unset")
	}
}

func TestExtract_NilClient_ReturnsErrUnavailable(t *testing.T) {
	var c *Client
	_, err := c.Extract(context.Background(), []byte("x"), "application/pdf")
	if !errors.Is(err, ErrUnavailable) {
		t.Errorf("expected ErrUnavailable, got %v", err)
	}
}

func TestExtract_HappyPath(t *testing.T) {
	var captured http.Header
	srv := fakeSidecar(t, http.StatusOK, map[string]any{
		"text": "hello",
		"tool": "tika",
		"metadata": map[string]any{
			"originalContentType": "application/pdf",
			"filename":            "x.pdf",
		},
	}, &captured)
	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	res, err := c.Extract(context.Background(), []byte("body"),
		"application/pdf", WithFilename("x.pdf"), WithLanguages("eng"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if res.Text != "hello" {
		t.Errorf("text: got %q", res.Text)
	}
	if res.Tool != "tika" {
		t.Errorf("tool: got %q", res.Tool)
	}
	if captured.Get("Content-Type") != "application/pdf" {
		t.Errorf("Content-Type forwarded: got %q", captured.Get("Content-Type"))
	}
	if captured.Get("X-Filename") != "x.pdf" {
		t.Errorf("X-Filename: got %q", captured.Get("X-Filename"))
	}
	if captured.Get("X-Languages") != "eng" {
		t.Errorf("X-Languages: got %q", captured.Get("X-Languages"))
	}
}

func TestExtract_ServerErrorParsesEnvelope(t *testing.T) {
	srv := fakeSidecar(t, http.StatusUnsupportedMediaType, map[string]any{
		"error": "no extractor for this MIME",
		"code":  "unsupported",
	}, nil)
	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	_, err := c.Extract(context.Background(), []byte("x"), "application/x-bogus")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "415") || !strings.Contains(err.Error(), "code=unsupported") {
		t.Errorf("error formatting: %q", err.Error())
	}
}

func TestExtract_TransportError(t *testing.T) {
	// Closed server → connection refused on Extract.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {}))
	srv.Close()
	c := &Client{BaseURL: srv.URL, HTTPClient: &http.Client{Timeout: 200 * time.Millisecond}}
	_, err := c.Extract(context.Background(), []byte("x"), "application/pdf")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "transport") {
		t.Errorf("error should mention transport: %v", err)
	}
}

func TestReady(t *testing.T) {
	srv := fakeSidecar(t, http.StatusOK, nil, nil)
	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	if !c.Ready(context.Background()) {
		t.Errorf("expected Ready=true")
	}
	var nilC *Client
	if nilC.Ready(context.Background()) {
		t.Errorf("nil client Ready should be false")
	}
}

// TestExtract_EntriesAndIterInner — verify entries[] decodes correctly
// and IterInner skips the container.
func TestExtract_EntriesAndIterInner(t *testing.T) {
	srv := fakeSidecar(t, http.StatusOK, map[string]any{
		"text": "all-content",
		"tool": "tika",
		"entries": []map[string]any{
			{"depth": 0, "filename": "archive.zip", "path": "", "contentType": "application/zip", "text": ""},
			{"depth": 1, "filename": "a.pdf", "path": "/a.pdf", "contentType": "application/pdf", "text": "alpha"},
			{"depth": 1, "filename": "b.txt", "path": "/b.txt", "contentType": "text/plain", "text": "beta"},
		},
	}, nil)
	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	res, err := c.Extract(context.Background(), []byte("zip"), "application/zip", WithFilename("archive.zip"))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(res.Entries) != 3 {
		t.Errorf("entries: got %d want 3", len(res.Entries))
	}
	inner := res.IterInner()
	if len(inner) != 2 {
		t.Fatalf("inner: got %d want 2 (container should be filtered)", len(inner))
	}
	if inner[0].Filename != "a.pdf" || inner[0].Depth != 1 {
		t.Errorf("inner[0]: %+v", inner[0])
	}
	// IterInner on nil receiver should be safe.
	var nilR *Result
	if len(nilR.IterInner()) != 0 {
		t.Errorf("nil receiver should yield empty slice")
	}
}

// Confirm that the body the client sends actually matches what arrives
// at the sidecar.
func TestExtract_BodyRoundTrip(t *testing.T) {
	got := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		_ = json.NewEncoder(w).Encode(map[string]any{"text": "ok", "tool": "tika"})
	}))
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, HTTPClient: srv.Client()}
	want := "the quick brown fox"
	_, err := c.Extract(context.Background(), []byte(want), "text/plain")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != want {
		t.Errorf("server got %q, want %q", got, want)
	}
}
