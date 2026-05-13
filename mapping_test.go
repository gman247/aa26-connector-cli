package main

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// ValidateMappingRules — permissions target
// ---------------------------------------------------------------------------

func TestValidateMapping_PermissionsTarget_Valid(t *testing.T) {
	manifest := map[string]any{
		"metadata": map[string]any{"name": "dropbox"},
		"spec": map[string]any{
			"capabilities": map[string]any{
				"scanTypes": []any{"access_scan"},
			},
			"sourceTypes": []any{
				map[string]any{
					"name": "DropboxPermission",
					"ingestion": map[string]any{
						"target": "permissions",
						"mapping": map[string]any{
							"permissionGrantId": "$.subject.id",
							"aceType":           "$.permissions.aceType",
							"memberRole":        "$.permissions.memberRole",
							"readAllowed":       "$.permissions.readAllowed",
							"writeAllowed":      "$.permissions.writeAllowed",
						},
					},
				},
			},
		},
	}
	errs, warns := ValidateMappingRules(manifest, "dropbox")
	if len(errs) != 0 {
		t.Errorf("expected no errors, got: %v", errs)
	}
	// Only the cluster-skip warning should appear
	for _, w := range warns {
		if strings.Contains(w, "sourceSystemId") {
			t.Errorf("unexpected sourceSystemId warning for permissions target: %s", w)
		}
	}
}

func TestValidateMapping_PermissionsTarget_MissingPermissionGrantId_Warns(t *testing.T) {
	manifest := map[string]any{
		"metadata": map[string]any{"name": "myconn"},
		"spec": map[string]any{
			"capabilities": map[string]any{
				"scanTypes": []any{"access_scan"},
			},
			"sourceTypes": []any{
				map[string]any{
					"name": "MyPermission",
					"ingestion": map[string]any{
						"target": "permissions",
						"mapping": map[string]any{
							"aceType": "$.permissions.aceType",
						},
					},
				},
			},
		},
	}
	_, warns := ValidateMappingRules(manifest, "myconn")
	found := false
	for _, w := range warns {
		if strings.Contains(w, "permissionGrantId") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected permissionGrantId warning, got warns: %v", warns)
	}
}

func TestValidateMapping_PermissionsTarget_InvalidColumn_Errors(t *testing.T) {
	manifest := map[string]any{
		"metadata": map[string]any{"name": "myconn"},
		"spec": map[string]any{
			"capabilities": map[string]any{
				"scanTypes": []any{"access_scan"},
			},
			"sourceTypes": []any{
				map[string]any{
					"name": "MyPermission",
					"ingestion": map[string]any{
						"target": "permissions",
						"mapping": map[string]any{
							"permissionGrantId": "$.subject.id",
							"entityId":          "$.object.id", // entities column — wrong table
						},
					},
				},
			},
		},
	}
	errs, _ := ValidateMappingRules(manifest, "myconn")
	found := false
	for _, e := range errs {
		if strings.Contains(e, "entityId") && strings.Contains(e, "permissions allow-list") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected column-not-in-permissions-allow-list error, got: %v", errs)
	}
}

func TestValidateMapping_PermissionsTarget_NoDomainRequired(t *testing.T) {
	// domain is not required (or validated) when target is permissions
	manifest := map[string]any{
		"metadata": map[string]any{"name": "myconn"},
		"spec": map[string]any{
			"capabilities": map[string]any{
				"scanTypes": []any{"access_scan"},
			},
			"sourceTypes": []any{
				map[string]any{
					"name": "MyPermission",
					// no domain field
					"ingestion": map[string]any{
						"target": "permissions",
						"mapping": map[string]any{
							"permissionGrantId": "$.subject.id",
						},
					},
				},
			},
		},
	}
	errs, _ := ValidateMappingRules(manifest, "myconn")
	for _, e := range errs {
		if strings.Contains(e, "domain") {
			t.Errorf("unexpected domain error for permissions target: %s", e)
		}
	}
}

func TestValidateMapping_InvalidTarget_Errors(t *testing.T) {
	manifest := map[string]any{
		"metadata": map[string]any{"name": "myconn"},
		"spec": map[string]any{
			"capabilities": map[string]any{
				"scanTypes": []any{"sensitive_data_scan"},
			},
			"sourceTypes": []any{
				map[string]any{
					"name":   "MyFile",
					"domain": "Artifact",
					"ingestion": map[string]any{
						"target": "custom_table", // invalid
						"mapping": map[string]any{
							"name": "$.object.name",
						},
					},
				},
			},
		},
	}
	errs, _ := ValidateMappingRules(manifest, "myconn")
	found := false
	for _, e := range errs {
		if strings.Contains(e, "custom_table") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected invalid-target error, got: %v", errs)
	}
}

func TestValidateMapping_EntitiesTarget_StillRequiresDomain(t *testing.T) {
	manifest := map[string]any{
		"metadata": map[string]any{"name": "myconn"},
		"spec": map[string]any{
			"capabilities": map[string]any{
				"scanTypes": []any{"sensitive_data_scan"},
			},
			"sourceTypes": []any{
				map[string]any{
					"name": "MyFile",
					// missing domain
					"ingestion": map[string]any{
						"target": "entities",
						"mapping": map[string]any{
							"name":           "$.object.name",
							"sourceSystemId": "$.object.id",
						},
					},
				},
			},
		},
	}
	errs, _ := ValidateMappingRules(manifest, "myconn")
	found := false
	for _, e := range errs {
		if strings.Contains(e, "domain") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected domain error for entities target, got: %v", errs)
	}
}

// ---------------------------------------------------------------------------
// ParseSourceTypes — IngestionTarget field
// ---------------------------------------------------------------------------

func TestParseSourceTypes_IngestionTarget(t *testing.T) {
	manifest := map[string]any{
		"spec": map[string]any{
			"sourceTypes": []any{
				map[string]any{
					"name":   "MyFile",
					"domain": "Artifact",
					"ingestion": map[string]any{
						"target": "entities",
						"mapping": map[string]any{
							"name": "$.object.name",
						},
					},
				},
				map[string]any{
					"name": "MyPermission",
					"ingestion": map[string]any{
						"target": "permissions",
						"mapping": map[string]any{
							"permissionGrantId": "$.subject.id",
						},
					},
				},
			},
		},
	}
	sts, err := ParseSourceTypes(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if len(sts) != 2 {
		t.Fatalf("expected 2 sourceTypes, got %d", len(sts))
	}
	if sts[0].IngestionTarget != "entities" {
		t.Errorf("sts[0].IngestionTarget = %q, want entities", sts[0].IngestionTarget)
	}
	if sts[1].IngestionTarget != "permissions" {
		t.Errorf("sts[1].IngestionTarget = %q, want permissions", sts[1].IngestionTarget)
	}
}

func TestParseSourceTypes_DefaultsToEntities(t *testing.T) {
	manifest := map[string]any{
		"spec": map[string]any{
			"sourceTypes": []any{
				map[string]any{
					"name":   "MyFile",
					"domain": "Artifact",
					"ingestion": map[string]any{
						// no target field
						"mapping": map[string]any{
							"name": "$.object.name",
						},
					},
				},
			},
		},
	}
	sts, err := ParseSourceTypes(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if sts[0].IngestionTarget != "entities" {
		t.Errorf("IngestionTarget = %q, want entities (default)", sts[0].IngestionTarget)
	}
}

// ---------------------------------------------------------------------------
// R008 lint rule
// ---------------------------------------------------------------------------

func TestLint_R008_AccessScanWithoutPermissionsTarget(t *testing.T) {
	bad := `apiVersion: connectors.netwrix.io/v1
kind: Connector
spec:
  capabilities:
    operations: [test_connection, scan]
    scanTypes: [access_scan, sensitive_data_scan]
  sourceTypes:
    - name: MyFile
      domain: Artifact
      ingestion:
        target: entities
        mapping:
          name: $.object.name
`
	dir := writeLintFiles(t, map[string]string{"connector.yaml": bad})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range got {
		if f.Rule == "R008" && f.Severity == "warn" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected R008 warn, got %+v", got)
	}
}

func TestLint_R008_NoFalsePositiveWhenPermissionsTargetPresent(t *testing.T) {
	good := `apiVersion: connectors.netwrix.io/v1
kind: Connector
spec:
  capabilities:
    operations: [test_connection, scan]
    scanTypes: [access_scan, sensitive_data_scan]
  sourceTypes:
    - name: MyFile
      domain: Artifact
      ingestion:
        target: entities
        mapping:
          name: $.object.name
    - name: MyPermission
      ingestion:
        target: permissions
        mapping:
          permissionGrantId: $.subject.id
`
	dir := writeLintFiles(t, map[string]string{"connector.yaml": good})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if f.Rule == "R008" {
			t.Errorf("R008 false positive when permissions target present: %+v", f)
		}
	}
}

func TestLint_R008_NoFalsePositiveForSDSOnly(t *testing.T) {
	// SDS-only connector — no access_scan, no permissions target needed.
	sdsOnly := `apiVersion: connectors.netwrix.io/v1
kind: Connector
spec:
  capabilities:
    operations: [test_connection, scan]
    scanTypes: [sensitive_data_scan]
  sourceTypes:
    - name: MyFile
      domain: Artifact
      ingestion:
        target: entities
        mapping:
          name: $.object.name
`
	dir := writeLintFiles(t, map[string]string{"connector.yaml": sdsOnly})
	got, err := Lint(LintConfig{Root: dir})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range got {
		if f.Rule == "R008" {
			t.Errorf("R008 false positive for SDS-only connector: %+v", f)
		}
	}
}
