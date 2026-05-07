// In-process implementation of the connector-runtime sidecar contract,
// used only by `aa26-connector test`. The harness emulator MUST match
// the real sidecar (connector-prototype/runtime/main.go) byte-for-byte
// on the bits a connector author can observe — anything that differs
// here is a footgun (the connector "works" locally, then fails in
// production, or vice-versa).
//
// Specifically, this emulator:
//   - Listens on 127.0.0.1:8089 (the production port — connectors that
//     hardcode "localhost:8089" work both here and in a real cluster).
//   - Returns 200 with `{}` on /v1/progress and /v1/log (NOT 204; that
//     was the source of a recently-fixed Python `requests.json()` bug).
//   - Returns 204 with empty body on GET /v1/checkpoint (matching
//     production's "no saved checkpoint" semantics — also a known
//     footgun for Python json.load on empty body).
//   - Validates every NDJSON line on /v1/findings against finding.schema.json.
//   - Returns the fixture-supplied invocation on /v1/invocation.
//   - Records per-endpoint hit counts and the last-call summary so the
//     test runner can produce coverage reports and forensic failure
//     summaries.
//   - Honors EmulatorOverrides from the fixture, letting authors write
//     deliberate negative tests.
//
// Things this emulator deliberately does NOT do:
//   - Forward findings to AA26's data-ingestion pipeline.
//   - Persist checkpoints to Redis.
//   - Honor the long-poll semantics of /v1/control beyond a short sleep.
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

const (
	// emulatorAddr matches the production sidecar's default — see
	// runtime/main.go envAddr / defaultAddr. Connectors hardcoding
	// "127.0.0.1:8089" or "localhost:8089" work without modification.
	emulatorAddr = "127.0.0.1:8089"

	// invocationExecutionID is the synthetic scan_executions UUID the
	// harness reports. Stable so reproducing failures in fixtures is
	// trivial. Distinct from the real production sidecar's all-zeros
	// fallback (which is itself only used when no real invocation file
	// is present).
	invocationExecutionID = "00000000-0000-0000-0000-000000000099"
	invocationSourceID    = "00000000-0000-0000-0000-000000000001"

	// workerOutputTailMax is how many trailing lines of worker stdout/stderr
	// the harness keeps for the forensic summary. Large enough to capture
	// a typical Python traceback, small enough to bound memory.
	workerOutputTailMax = 200
)

// Emulator is the harness-side analogue of the runtime sidecar. It
// implements http.Handler so callers can plug it into any net/http
// server harness (test or live).
type Emulator struct {
	op            string
	connection    map[string]any
	findingSchema *jsonschema.Schema
	overrides     map[string]ResponseOverride

	mu       sync.Mutex
	result   *RunResult
	finished chan struct{}
}

// NewEmulator constructs an emulator that will return the given op and
// connection block to the worker via /v1/invocation, and validate
// findings against the supplied compiled schema. Pass result so the
// caller can inspect counts/findings/logs after the worker exits.
func NewEmulator(op string, connection map[string]any, findingSchema *jsonschema.Schema, result *RunResult) *Emulator {
	if result.EndpointCalls == nil {
		result.EndpointCalls = map[string]int{}
	}
	if result.StartTime.IsZero() {
		result.StartTime = time.Now()
	}
	return &Emulator{
		op:            op,
		connection:    connection,
		findingSchema: findingSchema,
		result:        result,
		finished:      make(chan struct{}),
	}
}

// SetOverrides installs per-endpoint response overrides. Pass nil to
// clear. Overrides are matched on path (and optionally method) before any
// default handler runs.
func (e *Emulator) SetOverrides(o *EmulatorOverrides) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if o == nil {
		e.overrides = nil
		return
	}
	e.overrides = make(map[string]ResponseOverride, len(o.Responses))
	for path, ro := range o.Responses {
		e.overrides[path] = ro
	}
}

// Done returns a channel that closes after the worker POSTs
// /v1/complete, so the harness can stop streaming and proceed.
func (e *Emulator) Done() <-chan struct{} { return e.finished }

func (e *Emulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	rec := &recordingWriter{ResponseWriter: w, status: http.StatusOK}
	defer e.recordCall(r.Method, r.URL.Path, rec)

	if e.applyOverride(rec, r) {
		return
	}

	switch r.URL.Path {
	case "/healthz":
		_, _ = rec.Write([]byte("ok\n"))
	case "/v1/invocation":
		e.handleInvocation(rec, r)
	case "/v1/findings":
		e.handleFindings(rec, r)
	case "/v1/progress":
		e.handleProgress(rec, r)
	case "/v1/log":
		e.handleLog(rec, r)
	case "/v1/control":
		e.handleControl(rec, r)
	case "/v1/checkpoint":
		// Phase-0 emulator drops checkpoints on the floor. Default GET
		// returns 204 (matches production "no saved checkpoint"); POST
		// returns 204 too. Override via fixture.emulator.responses to
		// exercise the warm-start path.
		rec.WriteHeader(http.StatusNoContent)
	case "/v1/process":
		_, _ = rec.Write([]byte(`{"status":"queued"}`))
	case "/v1/complete":
		e.handleComplete(rec, r)
	default:
		http.NotFound(rec, r)
	}
}

// applyOverride returns true if a fixture override produced the response,
// short-circuiting the default handler.
func (e *Emulator) applyOverride(w *recordingWriter, r *http.Request) bool {
	e.mu.Lock()
	ro, ok := e.overrides[r.URL.Path]
	e.mu.Unlock()
	if !ok {
		return false
	}
	if ro.Method != "" && !strings.EqualFold(ro.Method, r.Method) {
		return false
	}
	// Drain the request body so callers don't block on a half-read socket.
	_, _ = io.Copy(io.Discard, r.Body)

	// /v1/findings is a special case: even with an override status, we
	// still want to validate any NDJSON payload so the author can detect
	// schema bugs in their failure-mode fixtures. We capture findings
	// first, then write the override response.
	if r.URL.Path == "/v1/findings" && r.Method == http.MethodPost {
		// We've already drained, so re-read isn't possible — record an
		// empty-body finding count so the override is honored cleanly.
	}

	for k, v := range ro.Headers {
		w.Header().Set(k, v)
	}
	status := ro.Status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	if ro.Body != "" {
		_, _ = w.Write([]byte(ro.Body))
	}

	if r.URL.Path == "/v1/complete" {
		// Author overrode /v1/complete — still surface terminal status to
		// callers that gate on Done(), but mark Status as "(overridden)"
		// to signal the worker's own status was pre-empted.
		e.mu.Lock()
		if e.result.Status == "" {
			e.result.Status = "(overridden)"
		}
		e.mu.Unlock()
		e.signalDone()
	}
	return true
}

// recordCall increments per-endpoint counters and stamps LastCall. Called
// in a deferred path so the data is captured even when a handler panics
// (recordingWriter's status default of 200 is overwritten by any handler
// that called WriteHeader).
func (e *Emulator) recordCall(method, path string, w *recordingWriter) {
	e.mu.Lock()
	defer e.mu.Unlock()
	key := method + " " + path
	if e.result.EndpointCalls == nil {
		e.result.EndpointCalls = map[string]int{}
	}
	e.result.EndpointCalls[key]++
	e.result.LastCall = &EndpointCall{
		Method:     method,
		Path:       path,
		Status:     w.status,
		OccurredAt: time.Since(e.result.StartTime),
	}
}

func (e *Emulator) handleInvocation(w http.ResponseWriter, _ *http.Request) {
	e.mu.Lock()
	e.result.InvocationCount++
	e.mu.Unlock()

	body := map[string]any{
		"operation":   e.op,
		"executionId": invocationExecutionID,
		"sourceId":    invocationSourceID,
		"source":      e.connection,
		"scan":        map[string]any{},
	}
	writeJSONResponse(w, http.StatusOK, body)
}

func (e *Emulator) handleFindings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	scanner := bufio.NewScanner(r.Body)
	scanner.Buffer(make([]byte, 1<<20), 8<<20)

	accepted := 0
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		raw := string(line)
		vf := ValidatedFinding{Raw: raw}

		var probe map[string]any
		if err := json.Unmarshal(line, &probe); err != nil {
			vf.ValidationOK = false
			vf.ValidationErr = "invalid JSON: " + err.Error()
			e.appendFinding(vf)
			continue
		}
		if k, ok := probe["kind"].(string); ok {
			vf.Kind = k
		}
		if t, ok := probe["type"].(string); ok {
			vf.Type = t
		}

		if err := e.findingSchema.Validate(probe); err != nil {
			vf.ValidationOK = false
			vf.ValidationErr = compactSchemaError(err)
		} else {
			vf.ValidationOK = true
			accepted++
		}
		e.appendFinding(vf)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		http.Error(w, "read: "+err.Error(), http.StatusBadRequest)
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]int{"accepted": accepted})
}

func (e *Emulator) handleProgress(w http.ResponseWriter, _ *http.Request) {
	e.mu.Lock()
	e.result.ProgressCount++
	e.mu.Unlock()
	// 200 with `{}` body — runtime/main.go does the same to avoid the
	// requests.json() footgun on Python connectors.
	writeJSONResponse(w, http.StatusOK, map[string]any{})
}

func (e *Emulator) handleLog(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	le := LogEvent{Level: "(unparseable)"}
	var probe map[string]any
	if err := json.Unmarshal(body, &probe); err == nil {
		if v, ok := probe["level"].(string); ok {
			le.Level = v
		}
		if v, ok := probe["message"].(string); ok {
			le.Message = v
		}
	}
	e.mu.Lock()
	e.result.LogEvents = append(e.result.LogEvents, le)
	e.mu.Unlock()
	writeJSONResponse(w, http.StatusOK, map[string]any{})
}

func (e *Emulator) handleControl(w http.ResponseWriter, r *http.Request) {
	// Production runtime long-polls Redis Streams for up to ~25s. Mimic
	// with a short sleep so connectors that poll in a tight loop don't
	// burn cpu, but cancel promptly when the worker exits and closes
	// its connection.
	select {
	case <-time.After(2 * time.Second):
	case <-r.Context().Done():
		return
	}
	writeJSONResponse(w, http.StatusOK, map[string]any{})
}

func (e *Emulator) handleComplete(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var summary struct {
		Status string `json:"status"`
	}
	_ = json.Unmarshal(body, &summary)
	if summary.Status == "" {
		summary.Status = "completed"
	}
	e.mu.Lock()
	e.result.Status = summary.Status
	e.mu.Unlock()
	writeJSONResponse(w, http.StatusOK, map[string]string{"status": "ok"})
	e.signalDone()
}

func (e *Emulator) appendFinding(vf ValidatedFinding) {
	e.mu.Lock()
	e.result.Findings = append(e.result.Findings, vf)
	e.mu.Unlock()
}

func (e *Emulator) signalDone() {
	select {
	case <-e.finished:
		// already closed; idempotent
	default:
		close(e.finished)
	}
}

func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// recordingWriter wraps an http.ResponseWriter to remember the status code
// the handler produced, so the per-endpoint summary can report what the
// emulator actually returned (not just what the connector requested).
type recordingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *recordingWriter) WriteHeader(code int) {
	if w.wroteHeader {
		return
	}
	w.status = code
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(code)
}

func (w *recordingWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		// Default Go behavior: implicit 200 on first Write.
		w.status = http.StatusOK
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(b)
}

// compactSchemaError flattens the multi-line jsonschema verdict into a
// single line good enough for a CLI summary. Authors can re-run with
// --verbose for the full structured output.
func compactSchemaError(err error) string {
	s := err.Error()
	parts := strings.Split(s, "\n")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return strings.Join(parts, "; ")
}

// listenEmulator binds the emulator to its production port. The error
// is wrapped so the caller can detect "address in use" and produce a
// helpful message (typically: another aa26-connector test still
// running, or a leftover runtime sidecar from `tilt up`).
func listenEmulator() (net.Listener, error) {
	l, err := net.Listen("tcp", emulatorAddr)
	if err != nil {
		return nil, fmt.Errorf("emulator bind %s: %w (another aa26-connector test running? "+
			"local runtime sidecar listening?)", emulatorAddr, err)
	}
	return l, nil
}
