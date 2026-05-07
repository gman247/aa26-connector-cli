// Contract probe — runs a connector through a curated matrix of edge cases
// the production sidecar can produce. Goal: a connector that passes the
// probe handles every documented response shape without crashing.
//
// Each scenario is a fixture overlay applied to whatever the user's normal
// fixture provides. We re-run the worker once per scenario so a partial
// failure doesn't poison subsequent runs (channels, in-process emulator
// state, etc. are reset between iterations).
//
// Scenarios are intentionally narrow. Adding new probe cases is a real
// commitment: every author who runs --probe-contract pays the runtime cost.
// New entries should map to a documented contract behavior or a real
// historical bug, not a speculative "what if".
package main

// ProbeScenario is one row in the probe matrix.
type ProbeScenario struct {
	// Name is what shows up in the result table. Keep short — the table
	// is meant to be skim-readable.
	Name string

	// Description is one sentence explaining what failure mode this
	// scenario reproduces. Printed under verbose mode and included in
	// the harness's documentation site.
	Description string

	// Overrides is layered onto the user's fixture for this run. nil
	// means "use the fixture as-is" (the default cold-start case).
	Overrides *EmulatorOverrides

	// AcceptStatus is the set of terminal statuses this scenario tolerates.
	// Most negative scenarios accept either "completed" (the connector
	// gracefully handled the failure) or "failed" (it failed loudly,
	// which is also acceptable as long as it didn't crash). Empty means
	// "any non-empty status is fine."
	AcceptStatus []string
}

// DefaultProbeScenarios returns the contract probe matrix. Order matters
// only for output readability — runs are independent.
func DefaultProbeScenarios() []ProbeScenario {
	return []ProbeScenario{
		{
			Name:        "cold-start",
			Description: "GET /v1/checkpoint returns 204 with empty body (no prior run).",
			Overrides: &EmulatorOverrides{
				Responses: map[string]ResponseOverride{
					"/v1/checkpoint": {Method: "GET", Status: 204, Body: ""},
				},
			},
			AcceptStatus: []string{"completed"},
		},
		{
			Name:        "warm-start",
			Description: "GET /v1/checkpoint returns 200 with a saved-state JSON object.",
			Overrides: &EmulatorOverrides{
				Responses: map[string]ResponseOverride{
					"/v1/checkpoint": {
						Method:  "GET",
						Status:  200,
						Body:    `{"cursor":"resume-token","seenCount":42}`,
						Headers: map[string]string{"Content-Type": "application/json"},
					},
				},
			},
			AcceptStatus: []string{"completed"},
		},
		{
			Name:        "control-empty-200",
			Description: "GET /v1/control returns 200 with `{}` (the production no-signal default).",
			Overrides: &EmulatorOverrides{
				Responses: map[string]ResponseOverride{
					"/v1/control": {Method: "GET", Status: 200, Body: `{}`,
						Headers: map[string]string{"Content-Type": "application/json"}},
				},
			},
			AcceptStatus: []string{"completed"},
		},
		{
			Name:        "log-rejected",
			Description: "POST /v1/log returns 500 — connector should keep working without logs.",
			Overrides: &EmulatorOverrides{
				Responses: map[string]ResponseOverride{
					"/v1/log": {Method: "POST", Status: 500, Body: "internal error"},
				},
			},
			AcceptStatus: []string{"completed", "failed"},
		},
		{
			Name:        "progress-rejected",
			Description: "POST /v1/progress returns 500 — connector should keep working without progress.",
			Overrides: &EmulatorOverrides{
				Responses: map[string]ResponseOverride{
					"/v1/progress": {Method: "POST", Status: 500, Body: "internal error"},
				},
			},
			AcceptStatus: []string{"completed", "failed"},
		},
	}
}

// MergeOverrides layers `extra` on top of `base`, returning a fresh map.
// Used to combine a scenario's overrides with whatever the user already
// declared in their fixture (scenario wins on key conflict — the probe
// is testing a specific behavior).
func MergeOverrides(base, extra *EmulatorOverrides) *EmulatorOverrides {
	if base == nil && extra == nil {
		return nil
	}
	out := &EmulatorOverrides{Responses: map[string]ResponseOverride{}}
	if base != nil {
		for k, v := range base.Responses {
			out.Responses[k] = v
		}
	}
	if extra != nil {
		for k, v := range extra.Responses {
			out.Responses[k] = v
		}
	}
	return out
}

// AcceptsStatus reports whether `status` is in the scenario's allow-list.
// An empty AcceptStatus list accepts anything non-empty (the worker at
// least made it to /v1/complete).
func (p ProbeScenario) AcceptsStatus(status string) bool {
	if len(p.AcceptStatus) == 0 {
		return status != ""
	}
	for _, s := range p.AcceptStatus {
		if s == status {
			return true
		}
	}
	return false
}
