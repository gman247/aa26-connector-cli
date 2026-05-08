package main

import (
	"reflect"
	"sort"
	"testing"
)

func TestConnectionFields_HonorsAllSchemas(t *testing.T) {
	manifest := map[string]any{
		"spec": map[string]any{
			"source": map[string]any{
				"schema": map[string]any{
					"required": []any{"rootPath"},
					"properties": map[string]any{
						"rootPath": map[string]any{
							"type":      "string",
							"x-display": "Root path",
						},
						"port": map[string]any{
							"type":    "integer",
							"default": 8080,
						},
					},
				},
			},
			"credentials": map[string]any{
				"schema": map[string]any{
					"required": []any{"username"},
					"properties": map[string]any{
						"username": map[string]any{"type": "string"},
						"password": map[string]any{"type": "string"},
					},
				},
			},
		},
	}

	fields, err := connectionFields(manifest, "", false)
	if err != nil {
		t.Fatal(err)
	}

	keys := []string{}
	for _, f := range fields {
		keys = append(keys, f.Key)
	}
	sort.Strings(keys)
	want := []string{"password", "port", "rootPath", "username"}
	if !reflect.DeepEqual(keys, want) {
		t.Errorf("keys = %v, want %v", keys, want)
	}

	byKey := map[string]fieldDecl{}
	for _, f := range fields {
		byKey[f.Key] = f
	}
	if !byKey["password"].Secret {
		t.Errorf("password should be detected as secret by name heuristic")
	}
	if !byKey["rootPath"].Required {
		t.Errorf("rootPath should be required")
	}
	if byKey["port"].Type != "integer" {
		t.Errorf("port type = %q, want integer", byKey["port"].Type)
	}
	if byKey["rootPath"].Display != "Root path" {
		t.Errorf("rootPath Display = %q", byKey["rootPath"].Display)
	}
}

func TestConnectionFields_PinnedAuthMethod(t *testing.T) {
	manifest := map[string]any{
		"spec": map[string]any{
			"auth": map[string]any{
				"methods": []any{
					map[string]any{
						"type": "basic",
						"fields": map[string]any{
							"properties": map[string]any{
								"username": map[string]any{"type": "string"},
							},
						},
					},
					map[string]any{
						"type": "api_key",
						"fields": map[string]any{
							"properties": map[string]any{
								"apiKey": map[string]any{"type": "string", "x-secret": true},
							},
						},
					},
				},
			},
		},
	}

	fields, err := connectionFields(manifest, "api_key", false)
	if err != nil {
		t.Fatalf("pinned api_key: %v", err)
	}
	if len(fields) != 1 || fields[0].Key != "apiKey" {
		t.Errorf("fields = %+v", fields)
	}
	if !fields[0].Secret {
		t.Errorf("apiKey should be secret via x-secret")
	}

	if _, err := connectionFields(manifest, "totally-not-a-method", false); err == nil {
		t.Error("expected error pinning a missing method")
	}
}

func TestConnectionFields_NoneAuthAutoSelectedNonInteractive(t *testing.T) {
	// Multiple methods + non-interactive: prefer "none" when present
	// (test_connection scenarios where credentials aren't needed).
	manifest := map[string]any{
		"spec": map[string]any{
			"auth": map[string]any{
				"methods": []any{
					map[string]any{"type": "basic", "fields": map[string]any{
						"properties": map[string]any{"u": map[string]any{"type": "string"}},
					}},
					map[string]any{"type": "none"},
				},
			},
		},
	}
	fields, err := connectionFields(manifest, "", false)
	if err != nil {
		t.Fatalf("none auto-pick: %v", err)
	}
	if len(fields) != 0 {
		t.Errorf("none method should yield zero fields, got %+v", fields)
	}
}

func TestCoerce(t *testing.T) {
	cases := []struct {
		raw  string
		typ  string
		want any
		err  bool
	}{
		{"hello", "string", "hello", false},
		{"42", "integer", 42, false},
		{"forty-two", "integer", nil, true},
		{"true", "boolean", true, false},
		{"yes", "boolean", nil, true},
		{"x", "", "x", false}, // default = string
	}
	for _, c := range cases {
		got, err := coerce(c.raw, c.typ)
		if c.err {
			if err == nil {
				t.Errorf("coerce(%q,%q) want error", c.raw, c.typ)
			}
			continue
		}
		if err != nil {
			t.Errorf("coerce(%q,%q) err = %v", c.raw, c.typ, err)
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("coerce(%q,%q) = %v, want %v", c.raw, c.typ, got, c.want)
		}
	}
}

func TestResolveConnection_FixtureWins(t *testing.T) {
	// Author already wrote a value into fixture.connection — prompter
	// must NOT re-prompt for it (and the test runs non-interactive
	// to enforce that — any prompt would be a hang).
	dir := t.TempDir()
	manifestPath := writeManifest(t, dir, `
apiVersion: connectors.netwrix.io/v1
kind: Connector
metadata:
  name: test
  displayName: Test
  version: 0.1.0
spec:
  image:
    repository: localhost/test
  capabilities:
    operations: [scan]
    scanTypes: [access_scan]
  source:
    schema:
      required: [rootPath]
      properties:
        rootPath:
          type: string
`)
	f := &Fixture{Connection: map[string]any{"rootPath": "/srv"}}
	out, err := resolveConnection(manifestPath, f, false)
	if err != nil {
		t.Fatalf("resolveConnection: %v", err)
	}
	if out["rootPath"] != "/srv" {
		t.Errorf("rootPath = %v, want /srv", out["rootPath"])
	}
}

func TestResolveConnection_NonInteractiveMissingRequiredErrors(t *testing.T) {
	dir := t.TempDir()
	manifestPath := writeManifest(t, dir, `
apiVersion: connectors.netwrix.io/v1
kind: Connector
metadata:
  name: test
  displayName: Test
  version: 0.1.0
spec:
  image:
    repository: localhost/test
  capabilities:
    operations: [scan]
    scanTypes: [access_scan]
  source:
    schema:
      required: [rootPath]
      properties:
        rootPath:
          type: string
`)
	f := &Fixture{Connection: map[string]any{}}
	if _, err := resolveConnection(manifestPath, f, false); err == nil {
		t.Error("expected missing-required error")
	}
}

func writeManifest(t *testing.T, dir, body string) string {
	t.Helper()
	path := dir + "/connector.yaml"
	if err := writeFileOrFatal(t, path, body); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFileOrFatal(t *testing.T, path, body string) error {
	t.Helper()
	return writeFileOnly(path, body)
}
