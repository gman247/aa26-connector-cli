// Ingestion mapping — §4/§5/§6/§9 of ingestion-mapping-v1.md.
//
// Three public entry points:
//
//   ValidateMappingRules  — offline §6 rules 1–11 run by `validate` and `validate-mapping`.
//   ParseSourceTypes      — parse spec.sourceTypes[] from a manifest map into typed structs.
//   ApplyMapping          — project one finding through the matching sourceType mapping.
//
// `schema entities` is handled in main.go (just prints entitiesColumns).
package main

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

//go:embed schema/entities-snapshot.json
var entitiesSnapshotJSON []byte

// EntityColumn is one row from the entities snapshot.
type EntityColumn struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// entitiesColumns is the §4.1 allow-list, loaded once at startup.
var entitiesColumns []EntityColumn

// entitiesAllowSet is a fast membership check over entitiesColumns names.
var entitiesAllowSet map[string]bool

func init() {
	var snap struct {
		Columns []EntityColumn `json:"columns"`
	}
	if err := json.Unmarshal(entitiesSnapshotJSON, &snap); err != nil {
		panic("entities-snapshot.json corrupt: " + err.Error())
	}
	entitiesColumns = snap.Columns
	entitiesAllowSet = make(map[string]bool, len(snap.Columns))
	for _, c := range snap.Columns {
		entitiesAllowSet[c.Name] = true
	}
}

// validDomains is the §4.5 allow-list.
var validDomains = map[string]bool{
	"Artifact": true, "Computer": true, "Group": true,
	"Principal": true, "Role": true, "SystemObject": true,
}

// hierarchicalDomains allows parentId.
var hierarchicalDomains = map[string]bool{"Artifact": true}

var (
	jsonPathRE    = regexp.MustCompile(`^\$(\.[a-zA-Z_][a-zA-Z0-9_]*)+$`)
	addlPropKeyRE = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_]*$`)
)

// ParsedSourceType is a pre-parsed spec.sourceTypes[] entry.
type ParsedSourceType struct {
	Name                 string
	Domain               string
	Mapping              map[string][]string // entities column → path segments
	AdditionalProperties map[string][]string // namespaced key → path segments
}

// ParseSourceTypes extracts and pre-parses spec.sourceTypes[] from a manifest map.
// Returns nil, nil when the key is absent (backward-compatible manifests).
func ParseSourceTypes(manifest map[string]any) ([]ParsedSourceType, error) {
	spec, _ := manifest["spec"].(map[string]any)
	if spec == nil {
		return nil, nil
	}
	raw, ok := spec["sourceTypes"]
	if !ok {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("spec.sourceTypes must be an array")
	}
	out := make([]ParsedSourceType, 0, len(arr))
	for i, item := range arr {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("spec.sourceTypes[%d]: must be an object", i)
		}
		st := ParsedSourceType{
			Mapping:              map[string][]string{},
			AdditionalProperties: map[string][]string{},
		}
		st.Name, _ = obj["name"].(string)
		st.Domain, _ = obj["domain"].(string)

		ingestion, _ := obj["ingestion"].(map[string]any)
		if ingestion != nil {
			if mapping, _ := ingestion["mapping"].(map[string]any); mapping != nil {
				for col, expr := range mapping {
					if s, _ := expr.(string); s != "" {
						st.Mapping[col] = parseDotPath(s)
					}
				}
			}
			if addl, _ := ingestion["additionalProperties"].(map[string]any); addl != nil {
				for key, expr := range addl {
					if s, _ := expr.(string); s != "" {
						st.AdditionalProperties[key] = parseDotPath(s)
					}
				}
			}
		}
		out = append(out, st)
	}
	return out, nil
}

// parseDotPath converts "$.object.url" → ["object","url"].
func parseDotPath(expr string) []string {
	if !strings.HasPrefix(expr, "$.") {
		return nil
	}
	return strings.Split(expr[2:], ".")
}

// lookupPath walks a segment slice through a JSON object tree.
func lookupPath(obj map[string]any, segments []string) (any, bool) {
	var cur any = obj
	for _, seg := range segments {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		v, exists := m[seg]
		if !exists {
			return nil, false
		}
		cur = v
	}
	return cur, true
}

// ApplyMapping projects one finding (parsed JSON) through the manifest's sourceType
// mapping. Returns (projectedRow, skipReason). skipReason is non-empty when the
// finding cannot be mapped (missing/unknown sourceType); the row is nil in that case.
//
// If exactly one sourceType is declared and the finding omits the sourceType field,
// the single mapping is applied and an info note is returned (not a skip).
func ApplyMapping(sourceTypes []ParsedSourceType, finding map[string]any) (row map[string]any, note string) {
	if len(sourceTypes) == 0 {
		return nil, "manifest declares no sourceTypes"
	}

	sourceTypeName, _ := finding["sourceType"].(string)
	var st *ParsedSourceType
	if sourceTypeName == "" {
		if len(sourceTypes) == 1 {
			st = &sourceTypes[0]
			note = fmt.Sprintf("sourceType absent — defaulted to %q (single sourceType declared)", st.Name)
		} else {
			return nil, "missing sourceType field (required when manifest declares multiple sourceTypes)"
		}
	} else {
		for i := range sourceTypes {
			if sourceTypes[i].Name == sourceTypeName {
				st = &sourceTypes[i]
				break
			}
		}
		if st == nil {
			return nil, fmt.Sprintf("unknown sourceType %q", sourceTypeName)
		}
	}

	row = map[string]any{}
	for col, segments := range st.Mapping {
		if v, ok := lookupPath(finding, segments); ok && v != nil {
			row[col] = v
		}
	}

	extras := map[string]string{}
	for key, segments := range st.AdditionalProperties {
		if v, ok := lookupPath(finding, segments); ok && v != nil {
			extras[key] = fmt.Sprintf("%v", v)
		}
	}
	if len(extras) > 0 {
		row["additionalProperties"] = extras
	}
	return row, note
}

// ValidateMappingRules runs the §6 offline rules (1–11, degrading cluster-only
// checks to warnings). connectorName is metadata.name from the manifest.
// Returns errors (blocking) and warnings (advisory).
func ValidateMappingRules(manifest map[string]any, connectorName string) (errs, warns []string) {
	spec, _ := manifest["spec"].(map[string]any)
	if spec == nil {
		return nil, nil
	}

	caps, _ := spec["capabilities"].(map[string]any)
	var needsMapping bool
	if caps != nil {
		if st, _ := caps["scanTypes"].([]any); st != nil {
			for _, v := range st {
				if v == "access_scan" || v == "sensitive_data_scan" {
					needsMapping = true
				}
			}
		}
	}

	sourceTypesRaw, _ := spec["sourceTypes"].([]any)

	// Rule 1
	if needsMapping && len(sourceTypesRaw) == 0 {
		errs = append(errs, "connector declares scanTypes but has no spec.sourceTypes ingestion mapping")
		return
	}
	if len(sourceTypesRaw) == 0 {
		return
	}

	seenNames := map[string]bool{}
	for i, item := range sourceTypesRaw {
		obj, ok := item.(map[string]any)
		if !ok {
			errs = append(errs, fmt.Sprintf("spec.sourceTypes[%d]: must be an object", i))
			continue
		}
		name, _ := obj["name"].(string)
		pre := fmt.Sprintf("spec.sourceTypes[%d](%s)", i, name)

		// Rule 2
		if seenNames[name] {
			errs = append(errs, fmt.Sprintf("%s: duplicate sourceType name %q within manifest", pre, name))
		}
		seenNames[name] = true

		// Rule 4
		domain, _ := obj["domain"].(string)
		if !validDomains[domain] {
			errs = append(errs, fmt.Sprintf("%s: unknown domain %q (must be one of Artifact, Computer, Group, Principal, Role, SystemObject)", pre, domain))
		}

		ingestion, _ := obj["ingestion"].(map[string]any)
		if ingestion == nil {
			errs = append(errs, fmt.Sprintf("%s: ingestion block is required", pre))
			continue
		}

		// Rule 5
		target, _ := ingestion["target"].(string)
		if target != "entities" {
			errs = append(errs, fmt.Sprintf("%s: ingestion.target must be \"entities\" (got %q)", pre, target))
		}

		mapping, _ := ingestion["mapping"].(map[string]any)
		addlProps, _ := ingestion["additionalProperties"].(map[string]any)
		hasSourceSystemId := false

		for col, expr := range mapping {
			exprStr, _ := expr.(string)
			// Rule 6
			if !entitiesAllowSet[col] {
				errs = append(errs, fmt.Sprintf("%s: mapping column %q is not in the entities allow-list", pre, col))
			}
			if col == "sourceSystemId" {
				hasSourceSystemId = true
			}
			// Rule 9
			if !jsonPathRE.MatchString(exprStr) {
				errs = append(errs, fmt.Sprintf("%s: mapping[%q]: invalid path expression %q (must match $.identifier.identifier...)", pre, col, exprStr))
			}
			// Rule 11
			if col == "parentId" && !hierarchicalDomains[domain] {
				errs = append(errs, fmt.Sprintf("%s: parentId is only valid for hierarchical domains (Artifact); domain is %q", pre, domain))
			}
		}

		// Rule 10
		if !hasSourceSystemId {
			warns = append(warns, fmt.Sprintf("%s: mapping omits sourceSystemId — entityId will not be stable across re-scans", pre))
		}

		// Rule 8 + Rule 9 on additionalProperties
		ns := connectorName + "."
		for key, expr := range addlProps {
			exprStr, _ := expr.(string)
			if !strings.HasPrefix(key, ns) {
				errs = append(errs, fmt.Sprintf("%s: additionalProperties key %q must be namespaced under %q", pre, key, ns))
			} else {
				suffix := key[len(ns):]
				if !addlPropKeyRE.MatchString(suffix) {
					errs = append(errs, fmt.Sprintf("%s: additionalProperties key %q has invalid suffix %q (must match [a-zA-Z][a-zA-Z0-9_]*)", pre, key, suffix))
				}
			}
			if !jsonPathRE.MatchString(exprStr) {
				errs = append(errs, fmt.Sprintf("%s: additionalProperties[%q]: invalid path expression %q", pre, key, exprStr))
			}
		}

		// Rule 3 / Rule 7: cluster-dependent — degrade to warning
		warns = append(warns, fmt.Sprintf("%s: rules 3 (registry collision) and 7 (column existence) require a live cluster and were skipped", pre))
	}
	return
}
