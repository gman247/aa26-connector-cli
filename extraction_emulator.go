// In-process mock of the extraction sidecar. Spawned by `aa26-connector
// test` when the manifest declares `spec.capabilities.sidecars: [extraction]`,
// so worker code that calls /v1/extract gets a non-empty response and
// the author can verify wiring without bundling real Tika+Tesseract.
//
// Behavior:
//   * Non-image MIMEs return a synthetic "EXTRACTED:<filename>" string
//     (or first 32 bytes of the body when no filename hint is sent).
//   * image/* returns 415 — OCR fidelity needs a real Tesseract; tests
//     for that should run against a cluster pod.
//   * /readyz always returns 200 — the mock skips JVM warmup.
//
// Port matches the production extraction sidecar's default
// (`127.0.0.1:8087`) so connector code that hardcodes the URL works
// against the harness AND in cluster without modification.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const extractionEmulatorAddr = "127.0.0.1:8087"

type extractionEmulator struct {
	mu    sync.Mutex
	calls int
}

func newExtractionEmulator() *extractionEmulator { return &extractionEmulator{} }

// callCount returns the number of /v1/extract requests served. Useful
// for fixture assertions like "this op should have invoked the
// extraction sidecar at least once."
func (e *extractionEmulator) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *extractionEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		_, _ = w.Write([]byte("ok\n"))
	case "/readyz":
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"tikaReady":true,"tesseractPath":"(mock)"}`))
	case "/v1/extract":
		e.handleExtract(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (e *extractionEmulator) handleExtract(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	contentType := r.Header.Get("Content-Type")
	if contentType == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"Content-Type header is required","code":"missing-content-type"}`))
		return
	}
	mime := strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))

	// Cap at 50 MiB so a runaway body in a fixture doesn't OOM the harness.
	body, err := io.ReadAll(io.LimitReader(r.Body, 50<<20))
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": err.Error()})
		return
	}

	e.mu.Lock()
	e.calls++
	e.mu.Unlock()

	if strings.HasPrefix(mime, "image/") {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnsupportedMediaType)
		_, _ = w.Write([]byte(`{"error":"OCR is not modeled by the local emulator; test against a real cluster pod","code":"unsupported"}`))
		return
	}

	filename := r.Header.Get("X-Filename")
	preview := string(body)
	if len(preview) > 32 {
		preview = preview[:32]
	}
	text := "EXTRACTED:" + filename
	if filename == "" {
		text = "EXTRACTED:" + preview
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"text": text,
		"tool": "tika",
		"metadata": map[string]any{
			"originalContentType": mime,
			"filename":            filename,
			"emulator":            true,
		},
	})
}

// listenExtractionEmulator binds the mock to the production port. Same
// failure-mode wrapping as listenEmulator so the operator gets a
// targeted error message when something else holds 8087.
func listenExtractionEmulator() (net.Listener, error) {
	l, err := net.Listen("tcp", extractionEmulatorAddr)
	if err != nil {
		return nil, fmt.Errorf("extraction emulator bind %s: %w (another aa26-connector test running, "+
			"or a real extraction sidecar on this host?)", extractionEmulatorAddr, err)
	}
	return l, nil
}
