package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLintFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for rel, body := range files {
		full := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestLint_R001_PythonUrlopenJsonLoad(t *testing.T) {
	src := `import urllib.request, json

def load_checkpoint(self):
    with urllib.request.urlopen(self.base + "/v1/checkpoint") as resp:
        return json.load(resp)
`
	dir := writeLintFiles(t, map[string]string{"connector.py": src})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) == 0 {
		t.Fatal("expected R001 hit, got none")
	}
	found := false
	for _, f := range got {
		if f.Rule == "R001" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected R001 finding, got %+v", got)
	}
}

func TestLint_R002_PythonRequestsJson(t *testing.T) {
	src := `import requests

def fetch(url):
    return requests.get(url).json()
`
	dir := writeLintFiles(t, map[string]string{"app.py": src})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	hasR002 := false
	for _, f := range got {
		if f.Rule == "R002" {
			hasR002 = true
		}
	}
	if !hasR002 {
		t.Errorf("expected R002 hit, got %+v", got)
	}
}

func TestLint_R003_GoDecodeWithoutStatusCheck(t *testing.T) {
	src := `package main

import (
	"encoding/json"
	"net/http"
)

func main() {
	resp, _ := http.Get("http://localhost:8089/v1/checkpoint")
	var v map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&v)
}
`
	dir := writeLintFiles(t, map[string]string{"go-conn/main.go": src})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	hasR003 := false
	for _, f := range got {
		if f.Rule == "R003" {
			hasR003 = true
		}
	}
	if !hasR003 {
		t.Errorf("expected R003 hit, got %+v", got)
	}
}

func TestLint_R004_TSAwaitJson(t *testing.T) {
	src := `export async function fetchCheckpoint() {
  const r = await fetch("http://localhost:8089/v1/checkpoint");
  return await r.json();
}
`
	dir := writeLintFiles(t, map[string]string{"src/checkpoint.ts": src})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	hasR004 := false
	for _, f := range got {
		if f.Rule == "R004" {
			hasR004 = true
		}
	}
	if !hasR004 {
		t.Errorf("expected R004 hit, got %+v", got)
	}
}

func TestLint_R005_PortDrift(t *testing.T) {
	src := `import requests
BASE = "http://127.0.0.1:8090"  # drift
`
	dir := writeLintFiles(t, map[string]string{"connector.py": src})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	hasR005 := false
	for _, f := range got {
		if f.Rule == "R005" && f.Severity == "error" {
			hasR005 = true
		}
	}
	if !hasR005 {
		t.Errorf("expected R005 error, got %+v", got)
	}
}

func TestLint_R006_SidecarsAtSpecLevel(t *testing.T) {
	// `sidecars: [extraction]` at 2-space indent = spec.sidecars, not
	// spec.capabilities.sidecars. Silently ignored by core-api, so the
	// linter is the only thing standing between the author and a broken
	// SDS pipeline.
	bad := `apiVersion: connectors.netwrix.io/v1
kind: Connector
spec:
  capabilities:
    operations: [test_connection, scan]
    scanTypes: [access_scan, sensitive_data_scan]

  sidecars: [extraction]
`
	dir := writeLintFiles(t, map[string]string{"connector.yaml": bad})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	hasR006 := false
	for _, f := range got {
		if f.Rule == "R006" && f.Severity == "error" {
			hasR006 = true
		}
	}
	if !hasR006 {
		t.Errorf("expected R006 error, got %+v", got)
	}
}

func TestLint_R006_NoFalsePositiveOnCorrectlyNested(t *testing.T) {
	// Correctly nested under capabilities — must not fire.
	good := `apiVersion: connectors.netwrix.io/v1
kind: Connector
spec:
  capabilities:
    operations: [test_connection, scan]
    scanTypes: [access_scan, sensitive_data_scan]
    sidecars: [extraction]
`
	dir := writeLintFiles(t, map[string]string{"connector.yaml": good})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if f.Rule == "R006" {
			t.Errorf("R006 false positive on correctly nested sidecars: %+v", f)
		}
	}
}

func TestLint_R007_FloatingImageTag(t *testing.T) {
	// `tag: dev` on a connector manifest is the floating-tag footgun
	// that silently installs stale images. R007 warns at lint time
	// (and `aa26-connector package` rejects strictly at package time).
	for _, bad := range []string{"dev", "latest"} {
		t.Run("tag="+bad, func(t *testing.T) {
			src := `apiVersion: connectors.netwrix.io/v1
kind: Connector
metadata:
  name: floaty
  version: 0.5.0
spec:
  image:
    repository: localhost/floaty
    tag: ` + bad + `
    pullPolicy: Never
`
			dir := writeLintFiles(t, map[string]string{"connector.yaml": src})
			got, err := Lint(LintConfig{Root: dir})
			if err != nil {
				t.Fatal(err)
			}
			hasR007 := false
			for _, f := range got {
				if f.Rule == "R007" && f.Severity == "warn" {
					hasR007 = true
				}
			}
			if !hasR007 {
				t.Errorf("expected R007 warn for tag=%s, got %+v", bad, got)
			}
		})
	}
}

func TestLint_R007_NoFalsePositiveOnVersionedTag(t *testing.T) {
	// Pinned semver tag — must not fire. Three flavors that should
	// all pass: bare semver, pre-release, build metadata.
	for _, good := range []string{"0.5.0", "1.2.0", "2.0.0-rc.1", "1.0.0+build.42"} {
		t.Run("tag="+good, func(t *testing.T) {
			src := `apiVersion: connectors.netwrix.io/v1
kind: Connector
metadata:
  name: pinned
  version: ` + good + `
spec:
  image:
    repository: localhost/pinned
    tag: ` + good + `
`
			dir := writeLintFiles(t, map[string]string{"connector.yaml": src})
			got, err := Lint(LintConfig{Root: dir})
			if err != nil {
				t.Fatal(err)
			}
			for _, f := range got {
				if f.Rule == "R007" {
					t.Errorf("R007 false positive on tag=%s: %+v", good, f)
				}
			}
		})
	}
}

func TestLint_NoFalsePositiveOnSafeCode(t *testing.T) {
	// Safe Python: status check before .json()
	src := `import requests

def fetch(url):
    r = requests.get(url)
    if r.status_code == 204:
        return None
    return r.json()
`
	dir := writeLintFiles(t, map[string]string{"safe.py": src})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	// R002 fires on the .json() call regardless of guard; that's deliberate
	// (we'd rather over-warn than miss). What we DO want absent: R001.
	for _, f := range got {
		if f.Rule == "R001" {
			t.Errorf("R001 false positive on safe code: %+v", f)
		}
	}
}

func TestLint_SkipsIgnoredDirs(t *testing.T) {
	bad := `import urllib.request, json
def x():
    with urllib.request.urlopen("/v1/checkpoint") as r:
        return json.load(r)
`
	dir := writeLintFiles(t, map[string]string{
		"node_modules/pkg/index.py": bad,
		"vendor/dep/code.py":        bad,
		".git/hooks/pre-commit.py":  bad,
	})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected ignored dirs to be skipped, got %+v", got)
	}
}

func TestLint_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("expected zero findings on empty dir, got %d", len(got))
	}
}

func TestLint_FormattingHasFileLineAndRule(t *testing.T) {
	src := `import urllib.request, json
def f():
    with urllib.request.urlopen("/x") as r:
        return json.load(r)
`
	dir := writeLintFiles(t, map[string]string{"a.py": src})
	got, _ := Lint(LintConfig{Root: dir})
	if len(got) == 0 {
		t.Fatal("expected lint hit")
	}
	var b strings.Builder
	_ = PrintLintFindings(got, &b)
	out := b.String()
	if !strings.Contains(out, "R001") {
		t.Errorf("output missing rule id: %s", out)
	}
	if !strings.Contains(out, "a.py:") {
		t.Errorf("output missing file:line: %s", out)
	}
}
