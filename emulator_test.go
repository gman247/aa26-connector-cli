package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestEmulator(t *testing.T) (*Emulator, *RunResult) {
	t.Helper()
	schema, err := loadFindingSchema()
	if err != nil {
		t.Fatalf("loadFindingSchema: %v", err)
	}
	r := &RunResult{}
	conn := map[string]any{"rootPath": "/tmp"}
	return NewEmulator("scan", conn, schema, r), r
}

func TestEmulator_Invocation(t *testing.T) {
	e, r := newTestEmulator(t)
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/invocation")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	if body["operation"] != "scan" {
		t.Errorf("operation = %v", body["operation"])
	}
	src, _ := body["source"].(map[string]any)
	if src["rootPath"] != "/tmp" {
		t.Errorf("source.rootPath = %v", src["rootPath"])
	}
	if r.InvocationCount != 1 {
		t.Errorf("InvocationCount = %d", r.InvocationCount)
	}
}

func TestEmulator_ProgressReturns200WithBody(t *testing.T) {
	e, r := newTestEmulator(t)
	srv := httptest.NewServer(e)
	defer srv.Close()

	// Production runtime returns 200 + `{}` to dodge requests.json()'s
	// JSONDecodeError on empty bodies. Harness must match.
	resp, err := http.Post(srv.URL+"/v1/progress", "application/json", strings.NewReader(`{"processed":1}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Errorf("body should be JSON: %v", err)
	}
	if r.ProgressCount != 1 {
		t.Errorf("ProgressCount = %d", r.ProgressCount)
	}
}

func TestEmulator_LogReturns200WithBody(t *testing.T) {
	e, r := newTestEmulator(t)
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/log", "application/json",
		strings.NewReader(`{"kind":"log","level":"error","message":"boom"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
	if len(r.LogEvents) != 1 || r.LogEvents[0].Level != "error" || r.LogEvents[0].Message != "boom" {
		t.Errorf("LogEvents = %+v", r.LogEvents)
	}
}

func TestEmulator_FindingsValidatesEachLine(t *testing.T) {
	e, r := newTestEmulator(t)
	srv := httptest.NewServer(e)
	defer srv.Close()

	good := `{"schemaVersion":"1.0","kind":"finding","type":"object_metadata","executionId":"00000000-0000-0000-0000-000000000099","occurredAt":"2026-05-07T00:00:00Z","object":{"kind":"file","id":"/tmp/a"}}`
	bad := `{"schemaVersion":"1.0","kind":"finding","executionId":"00000000-0000-0000-0000-000000000099"}` // missing required type + occurredAt

	body := strings.NewReader(good + "\n" + bad + "\n")
	resp, err := http.Post(srv.URL+"/v1/findings", "application/x-ndjson", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(r.Findings) != 2 {
		t.Fatalf("Findings count = %d, want 2", len(r.Findings))
	}
	if !r.Findings[0].ValidationOK {
		t.Errorf("first should be valid, got err: %s", r.Findings[0].ValidationErr)
	}
	if r.Findings[1].ValidationOK {
		t.Errorf("second should be invalid (missing required fields)")
	}
	if r.Findings[0].Type != "object_metadata" {
		t.Errorf("Type = %q", r.Findings[0].Type)
	}
}

func TestEmulator_CompleteSignalsDone(t *testing.T) {
	e, r := newTestEmulator(t)
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/complete", "application/json",
		bytes.NewBufferString(`{"status":"completed"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	select {
	case <-e.Done():
	default:
		t.Fatal("Done() should be closed after /v1/complete")
	}
	if r.Status != "completed" {
		t.Errorf("Status = %q", r.Status)
	}
}

func TestEmulator_Healthz(t *testing.T) {
	e, _ := newTestEmulator(t)
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d", resp.StatusCode)
	}
}

func TestEmulator_RecordsEndpointHits(t *testing.T) {
	e, r := newTestEmulator(t)
	srv := httptest.NewServer(e)
	defer srv.Close()

	// Mix of methods + paths. Each should land in EndpointCalls.
	if _, err := http.Get(srv.URL + "/v1/invocation"); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Get(srv.URL + "/v1/checkpoint"); err != nil {
		t.Fatal(err)
	}
	if _, err := http.Post(srv.URL+"/v1/log", "application/json",
		strings.NewReader(`{"level":"info"}`)); err != nil {
		t.Fatal(err)
	}

	if r.EndpointCalls["GET /v1/invocation"] != 1 {
		t.Errorf("invocation hits = %d", r.EndpointCalls["GET /v1/invocation"])
	}
	if r.EndpointCalls["GET /v1/checkpoint"] != 1 {
		t.Errorf("checkpoint hits = %d", r.EndpointCalls["GET /v1/checkpoint"])
	}
	if r.EndpointCalls["POST /v1/log"] != 1 {
		t.Errorf("log hits = %d", r.EndpointCalls["POST /v1/log"])
	}
	if r.LastCall == nil || r.LastCall.Path != "/v1/log" {
		t.Errorf("LastCall = %+v", r.LastCall)
	}
}

func TestEmulator_DefaultCheckpointReturns204Empty(t *testing.T) {
	// Production sidecar returns 204 with empty body when no saved
	// checkpoint exists; harness must match. This is the exact response
	// shape that broke web-crawler in 2026-05-07 production scan.
	e, _ := newTestEmulator(t)
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("status = %d, want 204", resp.StatusCode)
	}
	body := make([]byte, 16)
	n, _ := resp.Body.Read(body)
	if n != 0 {
		t.Errorf("body should be empty, got %d bytes: %q", n, body[:n])
	}
}

func TestEmulator_OverrideReplacesDefault(t *testing.T) {
	e, _ := newTestEmulator(t)
	e.SetOverrides(&EmulatorOverrides{
		Responses: map[string]ResponseOverride{
			"/v1/checkpoint": {
				Method:  "GET",
				Status:  200,
				Body:    `{"cursor":"abc"}`,
				Headers: map[string]string{"Content-Type": "application/json"},
			},
		},
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/v1/checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Content-Type") != "application/json" {
		t.Errorf("Content-Type = %q", resp.Header.Get("Content-Type"))
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body["cursor"] != "abc" {
		t.Errorf("body.cursor = %v", body["cursor"])
	}
}

func TestEmulator_OverrideRespectsMethod(t *testing.T) {
	e, _ := newTestEmulator(t)
	// Override only GET. POST should fall through to default (204).
	e.SetOverrides(&EmulatorOverrides{
		Responses: map[string]ResponseOverride{
			"/v1/checkpoint": {Method: "GET", Status: 200, Body: `{}`},
		},
	})
	srv := httptest.NewServer(e)
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/checkpoint", "application/json",
		bytes.NewBufferString(`{"cursor":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Errorf("POST should hit default (204), got %d", resp.StatusCode)
	}
}
