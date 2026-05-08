// Connection-parameter resolution for `aa26-connector test`.
//
// The author writes spec.source.schema / spec.credentials.schema /
// spec.auth.methods[].fields in connector.yaml. The harness combines
// those with whatever's already in the fixture's Connection map and
// prompts for any field still missing. The result is the connection
// block the worker container sees in REQUEST_DATA["connection"].
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

// fieldDecl describes one connection field as derived from the manifest.
type fieldDecl struct {
	Key         string
	Type        string // "string" (default), "integer", "boolean"
	Display     string
	Description string
	Required    bool
	Secret      bool
	Default     any
	Source      string // "source", "credentials", or "auth:<method>"
}

// resolveConnection returns the connection map the worker should see,
// merging the fixture's existing answers with prompts for missing
// fields. When interactive=false, a missing required field is an error
// rather than a prompt — that lets CI runs fail fast.
//
// The function never mutates the fixture's stored map directly; it
// returns a fresh map. The caller can write it back via SaveFixture.
func resolveConnection(manifestPath string, fixture *Fixture, interactive bool) (map[string]any, error) {
	manifest, err := loadManifest(manifestPath)
	if err != nil {
		return nil, err
	}
	fields, err := connectionFields(manifest, fixture.AuthMethod, interactive)
	if err != nil {
		return nil, err
	}

	out := map[string]any{}
	for k, v := range fixture.Connection {
		out[k] = v
	}

	prompted := false
	reader := bufio.NewReader(os.Stdin)
	for _, f := range fields {
		if _, ok := out[f.Key]; ok {
			continue
		}
		if !interactive {
			if f.Required {
				return nil, fmt.Errorf("missing required field %q (declared in spec.%s) — add it to fixture.connection or run interactively", f.Key, f.Source)
			}
			continue
		}
		if !prompted {
			fmt.Fprintln(os.Stderr, "→ collecting connection parameters (press enter to accept defaults)")
			prompted = true
		}
		val, err := promptField(reader, f)
		if err != nil {
			return nil, err
		}
		if val != nil {
			out[f.Key] = val
		}
	}
	return out, nil
}

// promptField asks the user for one value, masking when secret, and
// coerces to the declared JSON type. Returns nil for an empty answer
// when the field has no default and isn't required (fixture omits it).
func promptField(reader *bufio.Reader, f fieldDecl) (any, error) {
	label := f.Display
	if label == "" {
		label = f.Key
	}
	hint := f.Type
	if hint == "" {
		hint = "string"
	}
	suffix := ""
	if f.Default != nil {
		suffix = fmt.Sprintf(" [%v]", f.Default)
	} else if f.Required {
		suffix = " (required)"
	}
	if f.Description != "" {
		fmt.Fprintf(os.Stderr, "  ↳ %s\n", f.Description)
	}
	fmt.Fprintf(os.Stderr, "  %s (%s)%s: ", label, hint, suffix)

	var raw string
	if f.Secret && isTerminal(os.Stdin) {
		b, err := readPasswordNoEcho(os.Stdin)
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", f.Key, err)
		}
		raw = string(b)
	} else {
		line, err := reader.ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("read %s: %w", f.Key, err)
		}
		raw = strings.TrimRight(line, "\r\n")
	}

	if raw == "" {
		if f.Default != nil {
			return f.Default, nil
		}
		if f.Required {
			return nil, fmt.Errorf("%s is required", f.Key)
		}
		return nil, nil
	}
	return coerce(raw, f.Type)
}

func coerce(raw, typ string) (any, error) {
	switch typ {
	case "integer":
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("expected integer, got %q", raw)
		}
		return n, nil
	case "boolean":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("expected boolean (true/false), got %q", raw)
		}
		return b, nil
	default:
		return raw, nil
	}
}

// loadManifest reads connector.yaml as a generic map for field
// extraction. It avoids re-defining the manifest types — the schema
// already validated the document at this point in the flow.
func loadManifest(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("manifest yaml: %w", err)
	}
	var m map[string]any
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return nil, fmt.Errorf("manifest decode: %w", err)
	}
	return m, nil
}

// connectionFields collects field declarations from spec.source.schema,
// spec.credentials.schema, and the chosen auth method's fields.
//
// Field key collisions across sections are resolved by precedence:
// auth-method-fields > credentials > source. The least-likely one to be
// pre-known by the connector author wins, on the theory that whichever
// the wizard's last step is should be authoritative.
func connectionFields(manifest map[string]any, pinnedAuth string, interactive bool) ([]fieldDecl, error) {
	spec, _ := manifest["spec"].(map[string]any)
	if spec == nil {
		return nil, errors.New("manifest is missing spec block")
	}

	collected := map[string]fieldDecl{}
	order := []string{}
	add := func(d fieldDecl) {
		if _, exists := collected[d.Key]; !exists {
			order = append(order, d.Key)
		}
		collected[d.Key] = d
	}

	// 1. Source schema (least specific)
	if src, ok := nestedMap(spec, "source", "schema"); ok {
		for _, d := range jsonSchemaProperties(src, "source") {
			add(d)
		}
	}

	// 2. Credentials schema
	if cred, ok := nestedMap(spec, "credentials", "schema"); ok {
		for _, d := range jsonSchemaProperties(cred, "credentials") {
			add(d)
		}
	}

	// 3. Auth-method fields (most specific). Pick the method by `pinnedAuth`,
	//    or the first one when there's only one declared, or prompt when
	//    the manifest declares several and the user didn't pin one.
	if methods, ok := nestedSlice(spec, "auth", "methods"); ok && len(methods) > 0 {
		method, err := pickAuthMethod(methods, pinnedAuth, interactive)
		if err != nil {
			return nil, err
		}
		if method != nil {
			methodType, _ := method["type"].(string)
			if fields, ok := method["fields"].(map[string]any); ok {
				for _, d := range jsonSchemaProperties(fields, "auth:"+methodType) {
					add(d)
				}
			}
		}
	}

	out := make([]fieldDecl, 0, len(order))
	for _, k := range order {
		out = append(out, collected[k])
	}
	return out, nil
}

func pickAuthMethod(methods []any, pinned string, interactive bool) (map[string]any, error) {
	parsed := make([]map[string]any, 0, len(methods))
	for _, m := range methods {
		if mm, ok := m.(map[string]any); ok {
			parsed = append(parsed, mm)
		}
	}
	if len(parsed) == 0 {
		return nil, nil
	}
	if pinned != "" {
		for _, m := range parsed {
			if t, _ := m["type"].(string); t == pinned {
				return m, nil
			}
		}
		return nil, fmt.Errorf("authMethod %q not found in spec.auth.methods", pinned)
	}
	if len(parsed) == 1 {
		return parsed[0], nil
	}
	if !interactive {
		// Prefer "none" if present (test_connection workflows often don't
		// need credentials); otherwise fail loudly so CI fixtures pin one.
		for _, m := range parsed {
			if t, _ := m["type"].(string); t == "none" {
				return m, nil
			}
		}
		return nil, errors.New("multiple spec.auth.methods declared; set fixture.authMethod to choose one")
	}
	fmt.Fprintln(os.Stderr, "→ multiple auth methods declared. Pick one:")
	for i, m := range parsed {
		t, _ := m["type"].(string)
		dn, _ := m["displayName"].(string)
		if dn == "" {
			dn = t
		}
		fmt.Fprintf(os.Stderr, "  [%d] %s (%s)\n", i+1, dn, t)
	}
	fmt.Fprint(os.Stderr, "  choice: ")
	r := bufio.NewReader(os.Stdin)
	line, _ := r.ReadString('\n')
	line = strings.TrimSpace(line)
	idx, err := strconv.Atoi(line)
	if err != nil || idx < 1 || idx > len(parsed) {
		return nil, fmt.Errorf("invalid choice %q", line)
	}
	return parsed[idx-1], nil
}

// jsonSchemaProperties walks a JSON-Schema-subset object and returns
// fieldDecls for each property. Honors x-display, x-secret, description,
// required, and default — the same conventions the wizard renders.
func jsonSchemaProperties(schema map[string]any, source string) []fieldDecl {
	props, _ := schema["properties"].(map[string]any)
	if len(props) == 0 {
		return nil
	}
	required := stringSet(schema["required"])

	keys := make([]string, 0, len(props))
	for k := range props {
		keys = append(keys, k)
	}
	sort.Strings(keys) // stable prompt order

	out := make([]fieldDecl, 0, len(keys))
	for _, k := range keys {
		p, _ := props[k].(map[string]any)
		if p == nil {
			continue
		}
		d := fieldDecl{
			Key:      k,
			Source:   source,
			Required: required[k],
		}
		if t, ok := p["type"].(string); ok {
			d.Type = t
		}
		if v, ok := p["x-display"].(string); ok {
			d.Display = v
		} else if v, ok := p["xDisplay"].(string); ok {
			d.Display = v
		}
		if v, ok := p["description"].(string); ok {
			d.Description = v
		}
		if v, ok := p["x-secret"].(bool); ok {
			d.Secret = v
		} else if v, ok := p["xSecret"].(bool); ok {
			d.Secret = v
		}
		if name := strings.ToLower(k); strings.Contains(name, "password") || strings.Contains(name, "secret") || strings.Contains(name, "token") {
			d.Secret = true
		}
		if def, ok := p["default"]; ok {
			d.Default = def
		}
		out = append(out, d)
	}
	return out
}

func nestedMap(m map[string]any, path ...string) (map[string]any, bool) {
	cur := m
	for _, p := range path {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	return cur, true
}

func nestedSlice(m map[string]any, path ...string) ([]any, bool) {
	if len(path) == 0 {
		return nil, false
	}
	cur := m
	for _, p := range path[:len(path)-1] {
		next, ok := cur[p].(map[string]any)
		if !ok {
			return nil, false
		}
		cur = next
	}
	s, ok := cur[path[len(path)-1]].([]any)
	return s, ok
}

func stringSet(v any) map[string]bool {
	out := map[string]bool{}
	if arr, ok := v.([]any); ok {
		for _, e := range arr {
			if s, ok := e.(string); ok {
				out[s] = true
			}
		}
	}
	return out
}
