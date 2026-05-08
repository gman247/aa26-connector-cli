// Test fixtures describe a single harness run: which operation to invoke,
// what connection parameters the connector should see, optional extra env
// vars to inject, and the expectations to evaluate after the worker exits.
//
// A connector author who runs `aa26-connector test` without arguments gets
// a default fixture (./test-fixture.yaml) — the convention of putting the
// fixture next to connector.yaml mirrors how Docker Compose stacks live
// next to their Dockerfile.
//
// Fields the manifest declares (spec.source.schema, spec.credentials.schema,
// spec.auth.methods[].fields) are resolved interactively at run time when
// the fixture doesn't already supply them — the author doesn't need to
// pre-write a fixture for the first run.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"sigs.k8s.io/yaml"
)

// Fixture is the on-disk shape of a `test-fixture.yaml`. All fields are
// optional; sensible defaults make the simplest fixture a single line
// (`op: access-scan`).
type Fixture struct {
	// Op is the FUNCTION_TYPE the worker will see. Defaults to
	// "access-scan". Use dashes, not underscores, to match the production
	// convention (adapter_service.rb#build_scan_env tr("_", "-")).
	Op string `json:"op,omitempty"`

	// AuthMethod pins which spec.auth.methods[] entry the harness uses
	// when the manifest declares more than one. Matches on the method's
	// `type` field. Optional — when unset, the harness uses the first
	// method, or prompts the user when stdin is a tty.
	AuthMethod string `json:"authMethod,omitempty"`

	// Connection is the connection-parameter block the worker receives via
	// REQUEST_DATA["connection"] and via /v1/invocation source. Anything
	// the manifest declares that's missing here is prompted for at run
	// time (unless --non-interactive is set).
	Connection map[string]any `json:"connection,omitempty"`

	// Env injects additional environment variables on top of the framework
	// defaults. Useful for connectors that read extra knobs the harness
	// doesn't model directly (e.g. DEBUG, custom timeouts).
	Env map[string]string `json:"env,omitempty"`

	// Emulator lets a fixture override how the in-process emulator
	// responds on selected endpoints. Used to write deliberate negative
	// tests (e.g. "respond to /v1/checkpoint with 200 and a payload" to
	// exercise the warm-start path) without modifying connector code.
	Emulator *EmulatorOverrides `json:"emulator,omitempty"`

	// Expect describes what the harness checks after the worker exits.
	// An empty Expect block is equivalent to "any successful run passes".
	Expect ExpectBlock `json:"expect,omitempty"`
}

// EmulatorOverrides are per-endpoint response overrides. The keys of
// Responses are paths exactly as the connector would request them (e.g.
// "/v1/checkpoint"). Each ResponseOverride replaces the emulator's default
// reply for that path. The override applies to all methods unless Method
// is set.
type EmulatorOverrides struct {
	Responses map[string]ResponseOverride `json:"responses,omitempty"`
}

// ResponseOverride tells the emulator to return a custom status/body for
// one endpoint. Status defaults to 200 when unset; Body defaults to "".
// Headers are merged on top of the emulator's defaults — Content-Type
// can be overridden here.
type ResponseOverride struct {
	// Method, when set, restricts the override to a single HTTP method
	// (e.g. "GET"). When empty the override matches every method.
	Method string `json:"method,omitempty"`
	// Status is the HTTP status the emulator returns. 0 → 200.
	Status int `json:"status,omitempty"`
	// Body is the raw response body. Use "" to send an empty body (handy
	// for testing 204 / empty-body handling).
	Body string `json:"body,omitempty"`
	// Headers are response headers added on top of the emulator's defaults.
	Headers map[string]string `json:"headers,omitempty"`
}

// ExpectBlock is the post-run assertion set.
type ExpectBlock struct {
	// Status is the terminal scan status the worker must POST to
	// /v1/complete (or, equivalently, the exit-code-derived status if it
	// never POSTs). Default: "completed". Use "failed" for negative tests.
	Status string `json:"status,omitempty"`

	// Findings, when non-nil, is a count + type-set assertion over the
	// envelope stream the worker POSTs to /v1/findings.
	Findings *FindingExpect `json:"findings,omitempty"`

	// NoErrorLogs, when true, fails the run if any /v1/log POST carried
	// kind=log with level=error.
	NoErrorLogs bool `json:"noErrorLogs,omitempty"`

	// RequiredEndpoints, when non-empty, fails the run if the worker did
	// not call every listed sidecar endpoint at least once. Useful for
	// connectors with optional code paths the author wants to keep covered
	// in CI (e.g. ensure /v1/checkpoint was exercised).
	RequiredEndpoints []string `json:"requiredEndpoints,omitempty"`
}

// FindingExpect bounds and types the findings stream.
type FindingExpect struct {
	MinCount int      `json:"minCount,omitempty"`
	MaxCount int      `json:"maxCount,omitempty"`
	Types    []string `json:"types,omitempty"`
}

// LoadFixture parses a fixture file. Missing files yield a default
// fixture so authors can run `aa26-connector test` immediately on a fresh
// scaffold and have the prompter walk them through the connection block.
func LoadFixture(path string) (*Fixture, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return defaultFixture(), nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: yaml parse: %w", path, err)
	}
	f := defaultFixture()
	if err := json.Unmarshal(jsonBytes, f); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if f.Op == "" {
		f.Op = "access-scan"
	}
	if f.Connection == nil {
		f.Connection = map[string]any{}
	}
	if f.Env == nil {
		f.Env = map[string]string{}
	}
	if f.Expect.Status == "" {
		f.Expect.Status = "completed"
	}
	return f, nil
}

func defaultFixture() *Fixture {
	return &Fixture{
		Op:         "access-scan",
		Connection: map[string]any{},
		Env:        map[string]string{},
		Expect:     ExpectBlock{Status: "completed"},
	}
}

// SaveFixture writes the fixture back as YAML. Used by `--save-fixture`
// after the prompter fills in answers, so the next run is reproducible.
func SaveFixture(path string, f *Fixture) error {
	jsonBytes, err := json.Marshal(f)
	if err != nil {
		return fmt.Errorf("marshal fixture: %w", err)
	}
	yamlBytes, err := yaml.JSONToYAML(jsonBytes)
	if err != nil {
		return fmt.Errorf("yaml encode fixture: %w", err)
	}
	header := []byte("# Saved by `aa26-connector test --save-fixture`. Hand-edit to add\n" +
		"# expectations or pin auth methods.\n")
	return os.WriteFile(path, append(header, yamlBytes...), 0o644)
}

// OperationName returns the underscored form of Op, the way connectors
// see it via /v1/invocation.operation. The dashed form lives in
// FUNCTION_TYPE; this is the underscore-equivalent. (See
// adapter_service.rb: scan_type is underscored, FUNCTION_TYPE is dashed.)
func (f *Fixture) OperationName() string {
	return strings.ReplaceAll(f.Op, "-", "_")
}

// RunResult is what the harness collects during a single docker-run. It
// is what fixture expectations are evaluated against.
type RunResult struct {
	// Status is the last status the worker POSTed to /v1/complete, or
	// empty if it never did.
	Status string

	// ExitCode is the docker process exit code.
	ExitCode int

	// Findings is every NDJSON line the worker POSTed to /v1/findings,
	// in arrival order, with the schema-validation outcome attached.
	Findings []ValidatedFinding

	// LogEvents is every body the worker POSTed to /v1/log, parsed as a
	// log-kind envelope. Lines that fail to parse are still recorded with
	// Level set to "(unparseable)" so noErrorLogs can flag them.
	LogEvents []LogEvent

	// ProgressCount and InvocationCount are counters retained for legacy
	// reporting. New code should prefer EndpointCalls below, which is the
	// authoritative per-endpoint counter.
	ProgressCount   int
	InvocationCount int

	// StartTime is when the harness began the worker run. Used to compute
	// EndpointCall.OccurredAt as a relative duration so failure summaries
	// say "T+0.31s" rather than absolute timestamps.
	StartTime time.Time

	// EndpointCalls counts every sidecar endpoint the worker hit, keyed
	// by "<METHOD> <PATH>". Used by the coverage report and by
	// expect.requiredEndpoints.
	EndpointCalls map[string]int

	// LastCall records the most recent sidecar interaction. The forensic
	// summary on first-call failure quotes this so authors can pinpoint
	// which call killed the worker.
	LastCall *EndpointCall

	// WorkerOutputTail is a ring buffer of the last N lines of worker
	// stdout/stderr, kept so the forensic summary can quote terminal
	// output without needing a separate log file.
	WorkerOutputTail []string
}

// EndpointCall describes a single sidecar interaction.
type EndpointCall struct {
	Method     string
	Path       string
	Status     int           // status the emulator returned
	OccurredAt time.Duration // since RunResult.StartTime
}

// HitKey returns the canonical map key used for EndpointCalls.
func (c EndpointCall) HitKey() string { return c.Method + " " + c.Path }

// ValidatedFinding is one /v1/findings NDJSON line plus the validator's
// verdict. Raw is preserved so the summary can quote bad lines back to
// the author verbatim.
type ValidatedFinding struct {
	Raw           string
	ValidationOK  bool
	ValidationErr string
	Kind          string
	Type          string
}

// LogEvent is one parsed /v1/log envelope.
type LogEvent struct {
	Level   string
	Message string
}

// Evaluate compares the result against the fixture's expectations and
// returns a list of human-readable failure strings. An empty slice means
// every expectation passed.
func (f *Fixture) Evaluate(r *RunResult) []string {
	var failures []string

	want := f.Expect.Status
	if want == "" {
		want = "completed"
	}
	got := r.Status
	if got == "" {
		// Worker exited without posting /v1/complete. Treat exit-code
		// as the implicit terminal status: 0 → completed, non-0 → failed.
		if r.ExitCode == 0 {
			got = "completed"
		} else {
			got = "failed"
		}
	}
	if got != want {
		failures = append(failures, fmt.Sprintf("expect.status: want %q, got %q", want, got))
	}

	if fe := f.Expect.Findings; fe != nil {
		validCount := 0
		seenTypes := map[string]bool{}
		for _, vf := range r.Findings {
			if vf.ValidationOK {
				validCount++
			}
			if vf.Type != "" {
				seenTypes[vf.Type] = true
			}
		}
		if fe.MinCount > 0 && validCount < fe.MinCount {
			failures = append(failures, fmt.Sprintf("expect.findings.minCount: want >= %d, got %d", fe.MinCount, validCount))
		}
		if fe.MaxCount > 0 && validCount > fe.MaxCount {
			failures = append(failures, fmt.Sprintf("expect.findings.maxCount: want <= %d, got %d", fe.MaxCount, validCount))
		}
		for _, t := range fe.Types {
			if !seenTypes[t] {
				failures = append(failures, fmt.Sprintf("expect.findings.types: %q never observed", t))
			}
		}
	}

	// Schema-validation: any malformed finding is always a failure,
	// independent of fixture expectations. The whole point of the
	// harness is to catch these locally rather than in production.
	for i, vf := range r.Findings {
		if !vf.ValidationOK {
			failures = append(failures, fmt.Sprintf("finding #%d failed schema validation: %s", i+1, vf.ValidationErr))
		}
	}

	if f.Expect.NoErrorLogs {
		for _, le := range r.LogEvents {
			if le.Level == "error" {
				failures = append(failures, fmt.Sprintf("expect.noErrorLogs: error log observed: %s", le.Message))
			}
		}
	}

	for _, ep := range f.Expect.RequiredEndpoints {
		if !endpointHit(r.EndpointCalls, ep) {
			failures = append(failures, fmt.Sprintf("expect.requiredEndpoints: %q never called", ep))
		}
	}

	return failures
}

// endpointHit reports whether `want` was called at least once. `want` may
// be a bare path ("/v1/checkpoint", matches any method) or a fully-qualified
// "METHOD /v1/path" (matches that method only).
func endpointHit(hits map[string]int, want string) bool {
	if hits == nil {
		return false
	}
	if strings.Contains(want, " ") {
		return hits[want] > 0
	}
	for k, v := range hits {
		if v == 0 {
			continue
		}
		// k is "METHOD /path"
		idx := strings.IndexByte(k, ' ')
		if idx > 0 && k[idx+1:] == want {
			return true
		}
	}
	return false
}
