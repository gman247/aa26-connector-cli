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

	// Expect describes what the harness checks after the worker exits.
	// An empty Expect block is equivalent to "any successful run passes".
	Expect ExpectBlock `json:"expect,omitempty"`
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

	// ProgressCount and InvocationCount are counters for the summary line.
	ProgressCount   int
	InvocationCount int
}

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

	return failures
}
