package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadFixture_MissingFileReturnsDefaults(t *testing.T) {
	f, err := LoadFixture(filepath.Join(t.TempDir(), "does-not-exist.yaml"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if f.Op != "access-scan" {
		t.Errorf("default Op = %q, want access-scan", f.Op)
	}
	if f.Expect.Status != "completed" {
		t.Errorf("default Expect.Status = %q, want completed", f.Expect.Status)
	}
	if f.Connection == nil || f.Env == nil {
		t.Errorf("default fixture should have non-nil maps")
	}
}

func TestLoadFixture_ParsesYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix.yaml")
	if err := os.WriteFile(path, []byte(`
op: test-connection
authMethod: basic
connection:
  rootPath: /tmp
  port: 8080
env:
  DEBUG: "1"
expect:
  status: completed
  noErrorLogs: true
  findings:
    minCount: 1
    maxCount: 10
    types:
      - object_metadata
`), 0o644); err != nil {
		t.Fatal(err)
	}
	f, err := LoadFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	if f.Op != "test-connection" {
		t.Errorf("Op = %q", f.Op)
	}
	if f.AuthMethod != "basic" {
		t.Errorf("AuthMethod = %q", f.AuthMethod)
	}
	if f.Connection["rootPath"] != "/tmp" {
		t.Errorf("rootPath = %v", f.Connection["rootPath"])
	}
	// YAML-decoded ints become float64 via json round-trip; that's fine
	// since REQUEST_DATA marshals back to JSON either way.
	if v, ok := f.Connection["port"]; !ok {
		t.Errorf("port missing from Connection")
	} else if vf, ok := v.(float64); !ok || vf != 8080 {
		t.Errorf("port = %v (%T), want 8080", v, v)
	}
	if f.Env["DEBUG"] != "1" {
		t.Errorf("env.DEBUG = %q", f.Env["DEBUG"])
	}
	if f.Expect.Findings == nil || f.Expect.Findings.MinCount != 1 || f.Expect.Findings.MaxCount != 10 {
		t.Errorf("Findings = %+v", f.Expect.Findings)
	}
	if !f.Expect.NoErrorLogs {
		t.Errorf("NoErrorLogs = false, want true")
	}
}

func TestFixture_OperationName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"access-scan", "access_scan"},
		{"sensitive-data-scan", "sensitive_data_scan"},
		{"test-connection", "test_connection"},
		{"sync", "sync"},
	}
	for _, c := range cases {
		f := &Fixture{Op: c.in}
		if got := f.OperationName(); got != c.want {
			t.Errorf("OperationName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFixture_Evaluate(t *testing.T) {
	tests := []struct {
		name     string
		fixture  Fixture
		result   RunResult
		wantPass bool
		wantSubs []string // substrings expected in failures (when not passing)
	}{
		{
			name:     "happy path",
			fixture:  Fixture{Expect: ExpectBlock{Status: "completed"}},
			result:   RunResult{Status: "completed"},
			wantPass: true,
		},
		{
			name:     "missing /v1/complete + zero exit treated as completed",
			fixture:  Fixture{Expect: ExpectBlock{Status: "completed"}},
			result:   RunResult{Status: "", ExitCode: 0},
			wantPass: true,
		},
		{
			name:     "missing /v1/complete + non-zero exit treated as failed",
			fixture:  Fixture{Expect: ExpectBlock{Status: "completed"}},
			result:   RunResult{Status: "", ExitCode: 1},
			wantPass: false,
			wantSubs: []string{`want "completed", got "failed"`},
		},
		{
			name: "minCount under-shoot",
			fixture: Fixture{
				Expect: ExpectBlock{
					Status:   "completed",
					Findings: &FindingExpect{MinCount: 5},
				},
			},
			result: RunResult{
				Status:   "completed",
				Findings: []ValidatedFinding{{ValidationOK: true}, {ValidationOK: true}},
			},
			wantPass: false,
			wantSubs: []string{"minCount: want >= 5, got 2"},
		},
		{
			name: "schema-violation always fails",
			fixture: Fixture{
				Expect: ExpectBlock{Status: "completed"},
			},
			result: RunResult{
				Status: "completed",
				Findings: []ValidatedFinding{
					{ValidationOK: false, ValidationErr: "missing kind"},
				},
			},
			wantPass: false,
			wantSubs: []string{"finding #1 failed schema validation"},
		},
		{
			name: "noErrorLogs trips on error log",
			fixture: Fixture{
				Expect: ExpectBlock{Status: "completed", NoErrorLogs: true},
			},
			result: RunResult{
				Status:    "completed",
				LogEvents: []LogEvent{{Level: "error", Message: "kaboom"}},
			},
			wantPass: false,
			wantSubs: []string{"error log observed: kaboom"},
		},
		{
			name: "types: missing expected type",
			fixture: Fixture{
				Expect: ExpectBlock{
					Status:   "completed",
					Findings: &FindingExpect{Types: []string{"object_metadata"}},
				},
			},
			result: RunResult{
				Status:   "completed",
				Findings: []ValidatedFinding{{ValidationOK: true, Type: "access_grant"}},
			},
			wantPass: false,
			wantSubs: []string{`"object_metadata" never observed`},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fail := tc.fixture.Evaluate(&tc.result)
			if tc.wantPass && len(fail) != 0 {
				t.Fatalf("want pass, got %d failures: %v", len(fail), fail)
			}
			if !tc.wantPass && len(fail) == 0 {
				t.Fatalf("want fail, got pass")
			}
			for _, sub := range tc.wantSubs {
				found := false
				for _, f := range fail {
					if containsString(f, sub) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("expected failure containing %q, got %v", sub, fail)
				}
			}
		})
	}
}

func containsString(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

func TestSaveFixture_Roundtrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fix.yaml")
	in := &Fixture{
		Op:         "access-scan",
		Connection: map[string]any{"rootPath": "/srv"},
		Env:        map[string]string{"DEBUG": "1"},
		Expect:     ExpectBlock{Status: "completed"},
	}
	if err := SaveFixture(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	if out.Op != in.Op {
		t.Errorf("Op roundtrip: got %q want %q", out.Op, in.Op)
	}
	if out.Connection["rootPath"] != "/srv" {
		t.Errorf("Connection roundtrip: %v", out.Connection)
	}
	if out.Env["DEBUG"] != "1" {
		t.Errorf("Env roundtrip: %v", out.Env)
	}
}
