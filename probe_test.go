package main

import (
	"reflect"
	"testing"
)

func TestProbe_DefaultScenariosShape(t *testing.T) {
	scenarios := DefaultProbeScenarios()
	if len(scenarios) == 0 {
		t.Fatal("expected at least one default scenario")
	}
	seen := map[string]bool{}
	for _, s := range scenarios {
		if s.Name == "" {
			t.Errorf("scenario without name: %+v", s)
		}
		if seen[s.Name] {
			t.Errorf("duplicate scenario name: %s", s.Name)
		}
		seen[s.Name] = true
		if s.Description == "" {
			t.Errorf("scenario %s: missing Description", s.Name)
		}
	}
}

func TestProbe_AcceptsStatus(t *testing.T) {
	cases := []struct {
		name     string
		scenario ProbeScenario
		status   string
		want     bool
	}{
		{"empty list accepts any non-empty", ProbeScenario{}, "completed", true},
		{"empty list rejects empty", ProbeScenario{}, "", false},
		{"explicit allowlist", ProbeScenario{AcceptStatus: []string{"completed"}}, "completed", true},
		{"explicit allowlist rejects others", ProbeScenario{AcceptStatus: []string{"completed"}}, "failed", false},
		{"multi-allowlist", ProbeScenario{AcceptStatus: []string{"completed", "failed"}}, "failed", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.scenario.AcceptsStatus(c.status); got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}

func TestProbe_MergeOverrides(t *testing.T) {
	tests := []struct {
		name        string
		base, extra *EmulatorOverrides
		wantPaths   []string
		// expected status for /v1/checkpoint after merge (extra wins)
		wantCheckpointStatus int
	}{
		{
			name:                 "both nil",
			base:                 nil,
			extra:                nil,
			wantPaths:            nil,
			wantCheckpointStatus: 0,
		},
		{
			name:                 "extra wins on conflict",
			base:                 &EmulatorOverrides{Responses: map[string]ResponseOverride{"/v1/checkpoint": {Status: 200, Body: "{}"}}},
			extra:                &EmulatorOverrides{Responses: map[string]ResponseOverride{"/v1/checkpoint": {Status: 204}}},
			wantPaths:            []string{"/v1/checkpoint"},
			wantCheckpointStatus: 204,
		},
		{
			name:                 "union of distinct keys",
			base:                 &EmulatorOverrides{Responses: map[string]ResponseOverride{"/v1/log": {Status: 500}}},
			extra:                &EmulatorOverrides{Responses: map[string]ResponseOverride{"/v1/checkpoint": {Status: 204}}},
			wantPaths:            []string{"/v1/checkpoint", "/v1/log"},
			wantCheckpointStatus: 204,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := MergeOverrides(tc.base, tc.extra)
			if tc.wantPaths == nil {
				if got != nil {
					t.Errorf("want nil, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("want merged, got nil")
			}
			gotPaths := []string{}
			for k := range got.Responses {
				gotPaths = append(gotPaths, k)
			}
			// order-independent compare via map
			gotSet := setify(gotPaths)
			wantSet := setify(tc.wantPaths)
			if !reflect.DeepEqual(gotSet, wantSet) {
				t.Errorf("paths: got %v want %v", gotPaths, tc.wantPaths)
			}
			if cp, ok := got.Responses["/v1/checkpoint"]; ok && cp.Status != tc.wantCheckpointStatus {
				t.Errorf("/v1/checkpoint status: got %d want %d", cp.Status, tc.wantCheckpointStatus)
			}
		})
	}
}

func setify(ss []string) map[string]bool {
	m := map[string]bool{}
	for _, s := range ss {
		m[s] = true
	}
	return m
}
