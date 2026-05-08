// Package extraction is the Go client for the connector framework's
// extraction sidecar.
//
// The extraction sidecar (Tika + Tesseract) is attached to the connector
// Pod when the manifest declares `spec.capabilities.sidecars: [extraction]`.
// It exposes a single endpoint, POST /v1/extract, accepting raw file bytes
// and returning extracted text.
//
// Typical usage:
//
//	import "github.com/netwrix/connector-sdk-go/extraction"
//
//	c := extraction.NewClient()  // reads EXTRACTION_URL from env
//	res, err := c.Extract(ctx, fileBytes, "application/pdf",
//	    extraction.WithFilename("report.pdf"))
//	if err != nil {
//	    // ErrUnavailable: sidecar not attached (manifest didn't opt in)
//	    // ErrTransport / ErrServer: HTTP/transport failures
//	    log.Printf("extraction failed: %v", err)
//	}
//	finding["content"] = res.Text
package extraction

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

const (
	// EnvBaseURL is the env var the framework injects when the sidecar
	// is attached. Typically "http://127.0.0.1:8087".
	EnvBaseURL = "EXTRACTION_URL"

	// DefaultTimeout matches the sidecar's default EXTRACT_TIMEOUT_S
	// (300s = 5 min). Tika+OCR on a ~30 MB archive can take 1-3 min;
	// the previous 60s default tripped read-timeout on the govdocs1
	// corpus. Lower per-call only if you want to give up sooner.
	DefaultTimeout = 300 * time.Second
)

// ErrUnavailable indicates EXTRACTION_URL is unset — the manifest didn't
// opt into the extraction sidecar. Connectors that handle extraction
// themselves can branch on errors.Is(err, ErrUnavailable) cleanly.
var ErrUnavailable = errors.New("extraction sidecar not configured (EXTRACTION_URL unset)")

// Result is the parsed response from POST /v1/extract.
//
// Text is the concatenation of every entry's text — fine for
// "classify the whole archive" workflows. Entries[] enumerates each
// parsed item (the outer container plus any unwrapped archive members)
// for connectors that want per-entry findings.
type Result struct {
	Text     string         `json:"text"`
	Tool     string         `json:"tool"`
	Entries  []Entry        `json:"entries,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

// Entry is one item from inside an archive (or the lone item for a
// non-archive input). For a connector that wants to emit one finding
// per inner file, range over Result.Entries (or call Result.IterInner).
type Entry struct {
	Path        string         `json:"path"`        // e.g. "/payroll.pdf" or "/inner.zip/notes.docx"
	Filename    string         `json:"filename"`
	ContentType string         `json:"contentType"`
	Depth       int            `json:"depth"`       // 0 = container, 1 = direct child, 2 = grandchild
	Text        string         `json:"text"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

// IterInner returns Entries excluding the outer container (depth == 0).
// Convenience for the common "iterate every parsed file" loop without
// having to filter the container yourself. Slice is a fresh allocation;
// modifying it doesn't affect r.Entries.
func (r *Result) IterInner() []Entry {
	if r == nil {
		return nil
	}
	out := make([]Entry, 0, len(r.Entries))
	for _, e := range r.Entries {
		if e.Depth == 0 {
			continue
		}
		out = append(out, e)
	}
	return out
}

// Client is a thin wrapper around an http.Client that knows how to talk
// to the extraction sidecar.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient constructs a Client from EXTRACTION_URL. Use SetURL on the
// returned client to override the env-derived value (rarely needed).
// Returns nil if the env var is unset; callers should branch on that
// condition the way they would on errors.Is(err, ErrUnavailable).
func NewClient() *Client {
	url := strings.TrimSpace(os.Getenv(EnvBaseURL))
	if url == "" {
		return nil
	}
	return &Client{
		BaseURL:    strings.TrimRight(url, "/"),
		HTTPClient: &http.Client{Timeout: DefaultTimeout + 5*time.Second},
	}
}

// Option mutates the request before send. Use WithFilename / WithLanguages.
type Option func(*http.Request)

// WithFilename sets the X-Filename hint Tika uses for format detection
// when Content-Type is generic.
func WithFilename(name string) Option {
	return func(r *http.Request) { r.Header.Set("X-Filename", name) }
}

// WithLanguages sets the X-Languages hint for Tesseract OCR.
// Comma-separated language codes (e.g. "eng,spa").
func WithLanguages(langs string) Option {
	return func(r *http.Request) { r.Header.Set("X-Languages", langs) }
}

// Extract calls POST /v1/extract with the provided bytes. ctx controls
// cancellation and deadline; use context.WithTimeout to set a per-call
// timeout below DefaultTimeout if needed.
func (c *Client) Extract(ctx context.Context, data []byte, contentType string, opts ...Option) (*Result, error) {
	if c == nil {
		return nil, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/v1/extract", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)
	for _, opt := range opts {
		opt(req)
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("extraction transport: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		var errResp struct {
			Error string `json:"error"`
			Code  string `json:"code"`
		}
		_ = json.Unmarshal(body, &errResp)
		msg := errResp.Error
		if msg == "" {
			msg = strings.TrimSpace(string(body))
		}
		if errResp.Code != "" {
			msg = fmt.Sprintf("%s (code=%s)", msg, errResp.Code)
		}
		return nil, fmt.Errorf("extraction %d: %s", resp.StatusCode, msg)
	}
	var result Result
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// Ready returns true iff the sidecar's /readyz endpoint reports ready.
// Useful for startup probes that want to wait on Tika warmup before
// issuing the first scan request. Returns false on any transport error.
func (c *Client) Ready(ctx context.Context) bool {
	if c == nil {
		return false
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/readyz", nil)
	if err != nil {
		return false
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
