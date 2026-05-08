// aa26-connector — author tooling for the AA26 connector framework.
//
// Subcommands:
//   new <name>  --lang=python|bash    scaffold a connector skeleton
//   validate [path]                    validate a connector.yaml against the schema
//   lint [path]                        static-lint the connector source for known footguns
//   test [path] [flags]                run the connector locally against an
//                                      in-process sidecar emulator (or the real
//                                      runtime under --real-runtime). Lint runs
//                                      first; warnings are advisory unless --strict.
//   package [--out=FILE]               bundle the current directory into a
//                                      deployable .tar.gz for upload
//
// Designed for "write a connector in <30 minutes" — no daemon to install,
// no Docker required for `new`/`validate`/`lint`. `test` shells out to
// `docker run` to exercise the connector image; `package` shells out to
// `docker save`.
//
// The connector + finding JSON Schemas are embedded at build time so the
// binary is self-contained — no external schema files required.
package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"sigs.k8s.io/yaml"
)

//go:embed schema/connector.schema.json
var connectorSchemaJSON []byte

//go:embed schema/finding.schema.json
var findingSchemaJSON []byte

const usage = `aa26-connector — author tooling for the AA26 connector framework

Usage:
  aa26-connector new <name> --lang=python|bash [--dir=PATH]
      Scaffold a new connector. Drops connector.yaml + Dockerfile +
      handler skeleton into ./<name>/ (or --dir).

  aa26-connector validate [PATH]
      Validate connector.yaml against the schema. Defaults to ./connector.yaml.

  aa26-connector lint [PATH]
      Statically scan connector source for known footguns (e.g. json.load
      on a 204 response, sidecar URL drift). Advisory by default; pair
      with --strict to fail on any warning.

  aa26-connector test [PATH] [flags]
      Run the connector locally against the sidecar contract. By default
      uses the in-process emulator; --real-runtime swaps in the production
      runtime container. Validates findings against the envelope schema,
      evaluates fixture expectations, and streams worker stdout/stderr.
      Requires docker.

      Flags:
        --fixture=FILE         Test fixture YAML (default ./test-fixture.yaml).
                               Created on the fly if absent.
        --non-interactive      Don't prompt for missing connection params;
                               fail loudly. Use in CI.
        --save-fixture         Write resolved connection params back to the
                               fixture so the next run is reproducible.
        --keep-going           Don't exit non-zero on expectation failures.
        --probe-contract       Run the worker through every contract probe
                               scenario (cold start, warm start, sidecar
                               errors). Slower; cleanest pre-PR check.
        --real-runtime         Run the real connector-prototype runtime
                               container instead of the in-process emulator.
        --runtime-image=IMG    Override the runtime image used by --real-runtime.
        --strict               Fail on lint warnings, not just lint errors.
        --skip-lint            Skip the lint pre-check.
        --sample=N             Print the first N findings as pretty JSON after
                               the summary (default 10; --no-sample to suppress).

  aa26-connector schema entities
      Print the entities table allow-list (column names, types, descriptions).
      These are the only columns connector authors may reference in
      spec.sourceTypes[].ingestion.mapping.

  aa26-connector validate-mapping [--manifest=FILE] FINDINGS_FILE
      Apply the manifest's sourceTypes ingestion mapping to each finding in
      FINDINGS_FILE (raw NDJSON, one JSON object per line; use - for stdin)
      and print the resulting flat entity rows. Useful for iterating on
      mapping paths without a live cluster.

  aa26-connector package [--out=FILE]
      Bundle the current directory into a deployable .tar.gz for upload
      to AA26 (Add New Source). Validates the manifest, runs docker save
      on the image declared in spec.image, and emits <name>-<version>.tar.gz.

  aa26-connector --help
      This message.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "new":
		err := cmdNew(os.Args[2:])
		fail(err)
	case "validate":
		err := cmdValidate(os.Args[2:])
		fail(err)
	case "lint":
		err := cmdLint(os.Args[2:])
		fail(err)
	case "test":
		err := cmdTest(os.Args[2:])
		fail(err)
	case "package":
		err := cmdPackage(os.Args[2:])
		fail(err)
	case "schema":
		err := cmdSchema(os.Args[2:])
		fail(err)
	case "validate-mapping":
		err := cmdValidateMapping(os.Args[2:])
		fail(err)
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func fail(err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ %s\n", err)
		os.Exit(1)
	}
}

// ─── new ───────────────────────────────────────────────────────────────

func cmdNew(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: aa26-connector new <name> [--lang=python|bash] [--dir=PATH]")
	}
	name := args[0]
	lang := "python"
	dir := ""
	for _, a := range args[1:] {
		switch {
		case strings.HasPrefix(a, "--lang="):
			lang = strings.TrimPrefix(a, "--lang=")
		case strings.HasPrefix(a, "--dir="):
			dir = strings.TrimPrefix(a, "--dir=")
		}
	}
	if dir == "" {
		dir = name
	}
	if _, err := os.Stat(dir); err == nil {
		return fmt.Errorf("%s already exists — pick a different --dir", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	display := humanize(name)
	files := map[string]string{
		"connector.yaml": fmt.Sprintf(connectorYAMLTmpl, name, display, "0.1.0", name, name),
	}
	switch lang {
	case "python":
		files["connector.py"] = pythonHandler
		files["Dockerfile"] = pythonDockerfile
	case "bash":
		files["run.sh"] = bashHandler
		files["Dockerfile"] = bashDockerfile
	default:
		return fmt.Errorf("--lang must be python or bash, got %s", lang)
	}
	files["README.md"] = fmt.Sprintf(readmeTmpl, display, lang, name)

	for filename, content := range files {
		path := filepath.Join(dir, filename)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return err
		}
	}
	if lang == "bash" {
		_ = os.Chmod(filepath.Join(dir, "run.sh"), 0o755)
	}
	fmt.Printf("✓ created %s/\n", dir)
	fmt.Printf("  connector.yaml\n")
	for f := range files {
		if f != "connector.yaml" {
			fmt.Printf("  %s\n", f)
		}
	}
	fmt.Printf("\nNext:\n  cd %s\n  aa26-connector validate\n  # build + test:\n  docker build -t localhost/%s:dev .\n  aa26-connector test\n", dir, name)
	return nil
}

// ─── validate ──────────────────────────────────────────────────────────

func cmdValidate(args []string) error {
	path := "connector.yaml"
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		path = args[0]
	}
	schema, err := loadSchema()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return fmt.Errorf("yaml parse: %w", err)
	}
	var doc interface{}
	if err := json.Unmarshal(jsonBytes, &doc); err != nil {
		return fmt.Errorf("json decode: %w", err)
	}
	if err := schema.Validate(doc); err != nil {
		return fmt.Errorf("invalid:\n%s", err)
	}
	fmt.Printf("✓ %s is valid\n", path)

	// Run offline §6 mapping rules.
	docMap, _ := doc.(map[string]any)
	connName := ""
	if meta, _ := docMap["metadata"].(map[string]any); meta != nil {
		connName, _ = meta["name"].(string)
	}
	mappingErrs, mappingWarns := ValidateMappingRules(docMap, connName)
	for _, w := range mappingWarns {
		fmt.Fprintf(os.Stderr, "⚠ [M/warn] %s\n", w)
	}
	for _, e := range mappingErrs {
		fmt.Fprintf(os.Stderr, "✗ [M/error] %s\n", e)
	}
	if len(mappingErrs) > 0 {
		return fmt.Errorf("mapping validation failed (%d error(s))", len(mappingErrs))
	}
	return nil
}

// ─── lint ──────────────────────────────────────────────────────────────

func cmdLint(args []string) error {
	root := "."
	strict := false
	for _, a := range args {
		switch {
		case a == "--strict":
			strict = true
		case strings.HasPrefix(a, "--"):
			return fmt.Errorf("unknown flag %q", a)
		default:
			root = a
		}
	}
	findings, err := Lint(LintConfig{Root: root})
	if err != nil {
		return err
	}
	if len(findings) == 0 {
		fmt.Println("✓ lint clean")
		return nil
	}
	var b strings.Builder
	hasError := PrintLintFindings(findings, &b)
	fmt.Print(b.String())

	warnings := 0
	errs := 0
	for _, f := range findings {
		if f.Severity == "error" {
			errs++
		} else {
			warnings++
		}
	}
	fmt.Fprintf(os.Stderr, "\n%d error(s), %d warning(s)\n", errs, warnings)
	if hasError {
		return errors.New("lint reported errors")
	}
	if strict && warnings > 0 {
		return errors.New("lint reported warnings (--strict)")
	}
	return nil
}

// loadSchema returns the compiled connector-manifest JSON Schema. The
// schema is embedded into the binary at build time so the CLI works the
// same on a fresh checkout, in CI, on a developer laptop, and behind
// air-gapped networks. Honors CONNECTOR_SCHEMA as an override for
// authors iterating on the schema itself.
func loadSchema() (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	if override := os.Getenv("CONNECTOR_SCHEMA"); override != "" {
		s, err := c.Compile(override)
		if err == nil {
			return s, nil
		}
		return nil, fmt.Errorf("CONNECTOR_SCHEMA=%s: %w", override, err)
	}
	if err := c.AddResource("connector.schema.json", bytes.NewReader(connectorSchemaJSON)); err != nil {
		return nil, fmt.Errorf("register embedded connector schema: %w", err)
	}
	return c.Compile("connector.schema.json")
}

// loadFindingSchema returns the compiled finding-envelope JSON Schema,
// loaded from the embedded copy. Used by the test harness to validate
// every NDJSON line a connector POSTs to /v1/findings.
func loadFindingSchema() (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	if override := os.Getenv("FINDING_SCHEMA"); override != "" {
		s, err := c.Compile(override)
		if err == nil {
			return s, nil
		}
		return nil, fmt.Errorf("FINDING_SCHEMA=%s: %w", override, err)
	}
	if err := c.AddResource("finding.schema.json", bytes.NewReader(findingSchemaJSON)); err != nil {
		return nil, fmt.Errorf("register embedded finding schema: %w", err)
	}
	return c.Compile("finding.schema.json")
}

// ─── test ──────────────────────────────────────────────────────────────

type testFlags struct {
	manifestPath   string
	fixturePath    string
	nonInteractive bool
	saveFixture    bool
	keepGoing      bool
	probeContract  bool
	realRuntime    bool
	runtimeImage   string
	strict         bool
	skipLint       bool
	sampleCount    int // how many findings to print verbatim; 0 = off
}

func parseTestFlags(args []string) (testFlags, error) {
	f := testFlags{
		manifestPath: "connector.yaml",
		fixturePath:  "test-fixture.yaml",
		sampleCount:  10,
	}
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--fixture="):
			f.fixturePath = strings.TrimPrefix(a, "--fixture=")
		case a == "--non-interactive":
			f.nonInteractive = true
		case a == "--save-fixture":
			f.saveFixture = true
		case a == "--keep-going":
			f.keepGoing = true
		case a == "--probe-contract":
			f.probeContract = true
		case a == "--real-runtime":
			f.realRuntime = true
		case strings.HasPrefix(a, "--runtime-image="):
			f.runtimeImage = strings.TrimPrefix(a, "--runtime-image=")
		case a == "--strict":
			f.strict = true
		case a == "--skip-lint":
			f.skipLint = true
		case strings.HasPrefix(a, "--sample="):
			n, err := fmt.Sscanf(strings.TrimPrefix(a, "--sample="), "%d", &f.sampleCount)
			if err != nil || n != 1 || f.sampleCount < 0 {
				return f, fmt.Errorf("--sample= requires a non-negative integer")
			}
		case a == "--no-sample":
			f.sampleCount = 0
		case strings.HasPrefix(a, "--"):
			return f, fmt.Errorf("unknown flag %q", a)
		default:
			f.manifestPath = a
		}
	}
	return f, nil
}

// cmdTest is the harness entry point. It validates the manifest, runs the
// pre-flight linter, resolves the connection block (fixture + interactive
// prompt), starts an emulator on the production port (or the real runtime
// under --real-runtime), runs the worker container, and evaluates the
// fixture's expectations.
func cmdTest(args []string) error {
	flags, err := parseTestFlags(args)
	if err != nil {
		return err
	}

	if err := cmdValidate([]string{flags.manifestPath}); err != nil {
		return fmt.Errorf("manifest invalid: %w", err)
	}

	if !flags.skipLint {
		if err := runPreflightLint(filepath.Dir(flags.manifestPath), flags.strict); err != nil {
			return err
		}
	}

	manifest, err := loadManifestForTest(flags.manifestPath)
	if err != nil {
		return err
	}

	fixture, err := LoadFixture(flags.fixturePath)
	if err != nil {
		return err
	}

	connection, err := resolveConnection(flags.manifestPath, fixture, !flags.nonInteractive)
	if err != nil {
		return err
	}
	fixture.Connection = connection

	if flags.saveFixture {
		if err := SaveFixture(flags.fixturePath, fixture); err != nil {
			return fmt.Errorf("save fixture: %w", err)
		}
		fmt.Fprintf(os.Stderr, "→ saved fixture to %s\n", flags.fixturePath)
	}

	if err := ensureWorkerImage(manifest.imageRef); err != nil {
		return err
	}

	findingSchema, err := loadFindingSchema()
	if err != nil {
		return err
	}

	if flags.probeContract {
		return runProbeContract(flags, manifest, fixture, connection, findingSchema)
	}
	return runSingleScenario(flags, manifest, fixture, connection, findingSchema, fixture.Emulator)
}

// runPreflightLint executes the static lint and prints findings. By
// default warnings are advisory (so first-time authors aren't blocked by
// stylistic rules), and only errors fail the run. --strict promotes
// warnings to failures.
func runPreflightLint(root string, strict bool) error {
	if root == "" {
		root = "."
	}
	findings, err := Lint(LintConfig{Root: root})
	if err != nil {
		return fmt.Errorf("lint: %w", err)
	}
	if len(findings) == 0 {
		fmt.Fprintln(os.Stderr, "→ lint clean")
		return nil
	}
	var b strings.Builder
	hasError := PrintLintFindings(findings, &b)
	fmt.Fprintln(os.Stderr, "→ lint:")
	for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		fmt.Fprintln(os.Stderr, "  "+line)
	}
	if hasError {
		return errors.New("lint reported errors (use --skip-lint to bypass)")
	}
	if strict {
		return errors.New("lint reported warnings (--strict)")
	}
	return nil
}

// runSingleScenario does one harness pass: emulator (or real runtime),
// run worker, evaluate. Used for the default test mode and for each
// iteration of --probe-contract.
func runSingleScenario(
	flags testFlags,
	manifest *manifestSummary,
	fixture *Fixture,
	connection map[string]any,
	findingSchema *jsonschema.Schema,
	overrides *EmulatorOverrides,
) error {
	result := &RunResult{StartTime: time.Now(), EndpointCalls: map[string]int{}}

	exitCode, err := executeWorker(flags, manifest, fixture, connection, findingSchema, overrides, result)
	if err != nil {
		return err
	}
	result.ExitCode = exitCode

	failures := fixture.Evaluate(result)
	printSummary(result, failures, manifest, flags)

	if len(failures) > 0 && !flags.keepGoing {
		return fmt.Errorf("%d expectation(s) failed", len(failures))
	}
	return nil
}

// executeWorker dispatches between the in-process emulator and the
// real-runtime container. Returns the worker's exit code; result is
// populated in-place either way.
func executeWorker(
	flags testFlags,
	manifest *manifestSummary,
	fixture *Fixture,
	connection map[string]any,
	findingSchema *jsonschema.Schema,
	overrides *EmulatorOverrides,
	result *RunResult,
) (int, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if flags.realRuntime {
		fmt.Fprintf(os.Stderr, "→ real runtime: %s\n", flags.runtimeImage)
		fmt.Fprintf(os.Stderr, "→ running %s (op=%s, function_type=%s)\n",
			manifest.imageRef, fixture.OperationName(), fixture.Op)
		return runRealRuntime(ctx, RealRuntimeConfig{Image: flags.runtimeImage}, manifest, fixture, connection, findingSchema, result)
	}

	emulator := NewEmulator(fixture.OperationName(), connection, findingSchema, result)
	emulator.SetOverrides(overrides)

	listener, err := listenEmulator()
	if err != nil {
		return -1, err
	}
	server := &http.Server{Handler: emulator, ReadHeaderTimeout: 10 * time.Second}
	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- server.Serve(listener) }()
	defer func() {
		shutdownTimeout, scancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer scancel()
		_ = server.Shutdown(shutdownTimeout)
	}()

	// Optional second listener for the extraction sidecar mock. Only
	// spawned when the manifest declares it, so authors who don't use
	// extraction don't fight over port 8087. EXTRACTION_URL is set on
	// the worker by buildWorkerEnv based on the same condition.
	if manifest.hasSidecar("extraction") {
		extractionEmu := newExtractionEmulator()
		extractionListener, err := listenExtractionEmulator()
		if err != nil {
			return -1, err
		}
		extractionServer := &http.Server{Handler: extractionEmu, ReadHeaderTimeout: 10 * time.Second}
		go func() { _ = extractionServer.Serve(extractionListener) }()
		defer func() {
			shutdownTimeout, scancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer scancel()
			_ = extractionServer.Shutdown(shutdownTimeout)
		}()
		fmt.Fprintf(os.Stderr, "→ extraction emulator listening on %s (mock — Tika+OCR not running)\n",
			extractionEmulatorAddr)
	}

	fmt.Fprintf(os.Stderr, "→ emulator listening on %s\n", emulatorAddr)
	fmt.Fprintf(os.Stderr, "→ running %s (op=%s, function_type=%s)\n",
		manifest.imageRef, fixture.OperationName(), fixture.Op)

	dockerEnv := buildWorkerEnv(fixture, manifest, connection)
	tail := &outputTail{cap: workerOutputTailMax}
	exitCode, err := runWorker(ctx, manifest.imageRef, dockerEnv, emulator.Done(), tail)
	result.WorkerOutputTail = tail.snapshot()
	return exitCode, err
}

// runProbeContract loops the worker through every probe scenario,
// collecting per-scenario pass/fail outcomes and printing a matrix at
// the end. A scenario "passes" when the worker reaches a status the
// scenario's allow-list accepts AND no schema-validation errors fired.
func runProbeContract(
	flags testFlags,
	manifest *manifestSummary,
	fixture *Fixture,
	connection map[string]any,
	findingSchema *jsonschema.Schema,
) error {
	scenarios := DefaultProbeScenarios()
	type outcome struct {
		Name   string
		Status string
		Pass   bool
		Detail string
	}
	outcomes := make([]outcome, 0, len(scenarios))
	totalFailures := 0

	for i, sc := range scenarios {
		fmt.Fprintf(os.Stderr, "\n══ probe %d/%d: %s ══\n", i+1, len(scenarios), sc.Name)
		fmt.Fprintf(os.Stderr, "   %s\n", sc.Description)

		merged := MergeOverrides(fixture.Emulator, sc.Overrides)
		result := &RunResult{StartTime: time.Now(), EndpointCalls: map[string]int{}}

		exitCode, err := executeWorker(flags, manifest, fixture, connection, findingSchema, merged, result)
		if err != nil {
			outcomes = append(outcomes, outcome{Name: sc.Name, Status: "(error)", Pass: false, Detail: err.Error()})
			totalFailures++
			continue
		}
		result.ExitCode = exitCode

		// Override-derived scenarios may legitimately produce schema-invalid
		// findings (e.g. /v1/findings 500), so we relax the schema-validation
		// failure rule here. Status acceptance is the single gate.
		got := result.Status
		if got == "" {
			if exitCode == 0 {
				got = "completed"
			} else {
				got = "failed"
			}
		}
		pass := sc.AcceptsStatus(got)
		detail := ""
		if !pass {
			detail = fmt.Sprintf("got status=%q, want one of %v", got, sc.AcceptStatus)
			totalFailures++
		}
		outcomes = append(outcomes, outcome{Name: sc.Name, Status: got, Pass: pass, Detail: detail})
	}

	fmt.Fprintln(os.Stderr, "\n─── probe summary ─────────────────────────────────")
	for _, o := range outcomes {
		mark := "✓"
		if !o.Pass {
			mark = "✗"
		}
		fmt.Fprintf(os.Stderr, "  %s %-20s status=%s\n", mark, o.Name, o.Status)
		if o.Detail != "" {
			fmt.Fprintf(os.Stderr, "      %s\n", o.Detail)
		}
	}
	if totalFailures > 0 && !flags.keepGoing {
		return fmt.Errorf("%d/%d probe scenario(s) failed", totalFailures, len(scenarios))
	}
	return nil
}

// manifestSummary holds the bits of connector.yaml the harness needs at
// run time. Re-parsing as a typed struct avoids walking the generic map
// twice.
type manifestSummary struct {
	name        string
	version     string
	imageRef    string
	pullPolicy  string
	sourceTypes []ParsedSourceType
	sidecars    []string // capabilities.sidecars list (e.g. ["extraction"])
}

// hasSidecar tests whether `name` appears in spec.capabilities.sidecars.
// Used to gate the extra emulator listener and EXTRACTION_URL env-var
// injection when the manifest opts into a framework utility sidecar.
func (m *manifestSummary) hasSidecar(name string) bool {
	for _, s := range m.sidecars {
		if s == name {
			return true
		}
	}
	return false
}

func loadManifestForTest(path string) (*manifestSummary, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read manifest: %w", err)
	}
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return nil, fmt.Errorf("manifest yaml: %w", err)
	}
	var m struct {
		Metadata struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"metadata"`
		Spec struct {
			Image struct {
				Repository string `json:"repository"`
				Tag        string `json:"tag"`
				PullPolicy string `json:"pullPolicy"`
			} `json:"image"`
			Capabilities struct {
				// Framework-managed sidecars the connector opted into.
				// v1: ["extraction"]. When present, the harness spins up
				// a mock for each so SDK calls succeed without a real
				// cluster. See docs/extraction.md.
				Sidecars []string `json:"sidecars"`
			} `json:"capabilities"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return nil, fmt.Errorf("manifest decode: %w", err)
	}
	tag := m.Spec.Image.Tag
	if tag == "" {
		tag = "dev"
	}
	var rawDoc map[string]any
	_ = json.Unmarshal(jsonBytes, &rawDoc)
	sourceTypes, _ := ParseSourceTypes(rawDoc)
	return &manifestSummary{
		name:        m.Metadata.Name,
		version:     m.Metadata.Version,
		imageRef:    m.Spec.Image.Repository + ":" + tag,
		pullPolicy:  m.Spec.Image.PullPolicy,
		sourceTypes: sourceTypes,
		sidecars:    m.Spec.Capabilities.Sidecars,
	}, nil
}

// ensureWorkerImage fails fast when the image isn't already present in
// the local docker daemon. The harness deliberately doesn't run a docker
// pull for the user — connectors under development typically build
// against a private/local repository and a silent pull would either
// 404 or, worse, pull a stale published copy.
func ensureWorkerImage(ref string) error {
	cmd := exec.Command("docker", "image", "inspect", ref)
	cmd.Stdout = io.Discard
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("worker image %s not found locally — run `docker build -t %s .` first (%s)",
			ref, ref, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// buildWorkerEnv mirrors core-api's connector_api/adapter_service.rb
// build_scan_env. The harness exposes the connector to the same env-var
// surface the production runtime sets, so a connector that works under
// `aa26-connector test` runs unmodified in cluster.
func buildWorkerEnv(f *Fixture, m *manifestSummary, connection map[string]any) []string {
	requestData := map[string]any{
		"scanExecutionId": invocationExecutionID,
		"connection":      connection,
	}
	requestJSON, _ := json.Marshal(requestData)

	env := map[string]string{
		"SCAN_EXECUTION_ID": invocationExecutionID,
		"SCAN_ID":           "00000000-0000-0000-0000-0000000000aa",
		"SOURCE_ID":         invocationSourceID,
		"SOURCE_TYPE":       m.name,
		"SOURCE_VERSION":    nonEmpty(m.version, "1.0.0"),
		"FUNCTION_TYPE":     f.Op,
		"REQUEST_DATA":      string(requestJSON),
	}
	// Mirror adapter_service.rb's env wiring: when the manifest opts into
	// the extraction sidecar, expose EXTRACTION_URL so SDK calls resolve
	// to the harness's mock on the documented localhost port.
	if m.hasSidecar("extraction") {
		env["EXTRACTION_URL"] = "http://" + extractionEmulatorAddr
	}
	for k, v := range f.Env {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

func nonEmpty(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// runWorker shells out to `docker run --rm` with the constructed env vars
// and streams the worker's stdout/stderr to the caller live. Returns the
// exit code so callers can correlate with the fixture's expected status.
//
// On Linux, --network=host puts the worker on the same loopback as the
// in-process emulator. On macOS/Windows (where host-network is unavailable),
// the emulator binds 0.0.0.0:8089 and the worker connects via
// host.docker.internal (Docker Desktop's special hostname for the host).
//
// The completion signal (emulator.Done) lets us exit promptly when the
// worker POSTs /v1/complete — some long-running connectors take a few
// seconds to wind down their own goroutines after, and we'd rather
// surface results to the author immediately than wait.
func runWorker(ctx context.Context, imageRef string, env []string, done <-chan struct{}, tail *outputTail) (int, error) {
	var args []string
	if runtime.GOOS == "linux" {
		args = []string{"run", "--rm", "--network=host"}
	} else {
		// macOS / Windows: host.docker.internal reaches the host from inside a container.
		args = []string{"run", "--rm"}
		env = append(env, "SIDECAR_URL=http://host.docker.internal:8089")
		env = append(env, "EXTRACTION_URL=http://host.docker.internal:8087")
	}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, imageRef)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = teeWithTail(os.Stderr, "worker | ", tail)
	cmd.Stderr = teeWithTail(os.Stderr, "worker | ", tail)
	if err := cmd.Start(); err != nil {
		if isDockerPermissionError(err) {
			fmt.Fprintln(os.Stderr, "  (docker without sudo failed — retrying with sudo)")
			cmd = exec.CommandContext(ctx, "sudo", append([]string{"docker"}, args...)...)
			cmd.Stdout = teeWithTail(os.Stderr, "worker | ", tail)
			cmd.Stderr = teeWithTail(os.Stderr, "worker | ", tail)
			if err := cmd.Start(); err != nil {
				return -1, fmt.Errorf("docker run: %w", err)
			}
		} else {
			return -1, fmt.Errorf("docker run: %w", err)
		}
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	select {
	case err := <-exited:
		return exitCodeFromErr(err), nil
	case <-done:
		select {
		case err := <-exited:
			return exitCodeFromErr(err), nil
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			return 0, nil
		}
	case <-sigCh:
		fmt.Fprintln(os.Stderr, "→ interrupted; shutting down worker")
		_ = cmd.Process.Signal(syscall.SIGINT)
		select {
		case err := <-exited:
			return exitCodeFromErr(err), nil
		case <-time.After(3 * time.Second):
			_ = cmd.Process.Kill()
			<-exited
			return 130, nil
		}
	}
}

func exitCodeFromErr(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

func isDockerPermissionError(err error) bool {
	probeOut, _ := exec.Command("docker", "info").CombinedOutput()
	return strings.Contains(string(probeOut), "permission denied") ||
		strings.Contains(err.Error(), "permission denied")
}

// streamPrefix returns an io.Writer that prefixes every line with `prefix`
// before forwarding to `w`. Used for child-process logs that don't need
// to be captured into a tail buffer (e.g. the runtime container in
// real-runtime mode — its findings are read from a file separately).
func streamPrefix(w io.Writer, prefix string) io.Writer {
	pr, pw := io.Pipe()
	go func() {
		buf := make([]byte, 4096)
		var line bytes.Buffer
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				for _, b := range buf[:n] {
					line.WriteByte(b)
					if b == '\n' {
						_, _ = w.Write([]byte(prefix + line.String()))
						line.Reset()
					}
				}
			}
			if err != nil {
				if line.Len() > 0 {
					_, _ = w.Write([]byte(prefix + line.String() + "\n"))
				}
				return
			}
		}
	}()
	return pw
}

// printSummary renders the post-run report. Three sections:
//  1. counts (existing behavior, retained)
//  2. endpoint coverage table — every sidecar path the harness recognizes,
//     with hit count
//  3. forensic block (only if the worker exited non-zero before /v1/complete)
//     summarizing the last sidecar call, recent worker output, and a
//     heuristic suggestion when one fits the failure shape.
func printSummary(r *RunResult, failures []string, m *manifestSummary, flags testFlags) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "─── summary ───────────────────────────────────────")
	fmt.Fprintf(os.Stderr, "  invocations:  %d\n", r.InvocationCount)
	fmt.Fprintf(os.Stderr, "  findings:     %d (valid: %d)\n", len(r.Findings), countValid(r.Findings))
	fmt.Fprintf(os.Stderr, "  progress:     %d\n", r.ProgressCount)
	fmt.Fprintf(os.Stderr, "  log events:   %d (errors: %d)\n", len(r.LogEvents), countLevel(r.LogEvents, "error"))
	if r.Status != "" {
		fmt.Fprintf(os.Stderr, "  status:       %s\n", r.Status)
	} else {
		fmt.Fprintf(os.Stderr, "  status:       (worker did not POST /v1/complete; exit=%d)\n", r.ExitCode)
	}

	printCoverage(r)
	printErrorLogs(r)
	printFindingSample(r, flags.sampleCount, m.sourceTypes)

	if isFirstCallFailure(r) {
		printForensic(r, m, flags)
	}

	if len(failures) == 0 {
		fmt.Fprintln(os.Stderr, "  result:       ✓ pass")
		return
	}
	fmt.Fprintln(os.Stderr, "  result:       ✗ fail")
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "    - %s\n", f)
	}
}

// known sidecar endpoints that show up in coverage tables. The order is
// the canonical ordering used in docs/runtime-contract.md so the report
// reads top-to-bottom the way the contract is documented.
var coverageEndpoints = []struct{ Method, Path string }{
	{"GET", "/v1/invocation"},
	{"GET", "/v1/checkpoint"},
	{"POST", "/v1/checkpoint"},
	{"POST", "/v1/findings"},
	{"POST", "/v1/progress"},
	{"POST", "/v1/log"},
	{"GET", "/v1/control"},
	{"POST", "/v1/process"},
	{"POST", "/v1/complete"},
}

// printFindingSample prints the first n findings as pretty-printed JSON so
// authors can eyeball the shape of what the connector is emitting without
// having to add external tooling. Invalid findings are flagged with ✗ and
// their schema error is printed inline. Pass n=0 to suppress entirely.
// When sourceTypes are declared in the manifest, it also shows the projected
// entity row for each finding.
func printFindingSample(r *RunResult, n int, sourceTypes []ParsedSourceType) {
	if n <= 0 || len(r.Findings) == 0 {
		return
	}
	cap := n
	if cap > len(r.Findings) {
		cap = len(r.Findings)
	}
	label := fmt.Sprintf("first %d", cap)
	if cap == len(r.Findings) {
		label = "all"
	}
	fmt.Fprintf(os.Stderr, "  findings sample (%s of %d):\n", label, len(r.Findings))
	for i, f := range r.Findings[:cap] {
		mark := "✓"
		if !f.ValidationOK {
			mark = "✗"
		}
		var pretty bytes.Buffer
		if err := json.Indent(&pretty, []byte(f.Raw), "      ", "  "); err == nil {
			fmt.Fprintf(os.Stderr, "    %s [%d] %s\n", mark, i+1, pretty.String())
		} else {
			fmt.Fprintf(os.Stderr, "    %s [%d] %s\n", mark, i+1, f.Raw)
		}
		if !f.ValidationOK {
			fmt.Fprintf(os.Stderr, "        schema error: %s\n", f.ValidationErr)
		}
		// Show projected entity row if sourceTypes are declared
		if len(sourceTypes) > 0 {
			var findingObj map[string]any
			if err := json.Unmarshal([]byte(f.Raw), &findingObj); err == nil {
				row, note := ApplyMapping(sourceTypes, findingObj)
				if row != nil {
					var rowPretty bytes.Buffer
					if rowJSON, err := json.Marshal(row); err == nil {
						if err := json.Indent(&rowPretty, rowJSON, "        ", "  "); err == nil {
							fmt.Fprintf(os.Stderr, "      → entities row: %s\n", rowPretty.String())
						}
					}
				} else if note != "" {
					fmt.Fprintf(os.Stderr, "      → entities row: (skipped: %s)\n", note)
				}
				if note != "" && row != nil {
					fmt.Fprintf(os.Stderr, "      → note: %s\n", note)
				}
			}
		}
	}
}

func printCoverage(r *RunResult) {
	fmt.Fprintln(os.Stderr, "  coverage:")
	keysSeen := map[string]bool{}
	for _, ep := range coverageEndpoints {
		key := ep.Method + " " + ep.Path
		count := r.EndpointCalls[key]
		mark := "✓"
		if count == 0 {
			mark = "·"
		}
		fmt.Fprintf(os.Stderr, "    %s %-4s %-18s %dx\n", mark, ep.Method, ep.Path, count)
		keysSeen[key] = true
	}
	// Surface unexpected paths (e.g. /healthz, typos) so coverage isn't
	// silently mis-attributed.
	var extra []string
	for k := range r.EndpointCalls {
		if !keysSeen[k] {
			extra = append(extra, k)
		}
	}
	if len(extra) > 0 {
		sort.Strings(extra)
		for _, k := range extra {
			fmt.Fprintf(os.Stderr, "    ? %-23s %dx (unrecognized path)\n", k, r.EndpointCalls[k])
		}
	}
}

// printErrorLogs surfaces every error-level log message the worker emitted,
// but only when something is actually wrong (status=failed, non-zero exit,
// or any error log at all). The point is to put the worker's own error
// reason in the summary instead of forcing authors to re-read the streamed
// worker output. On healthy runs the section is suppressed to keep the
// summary tight.
func printErrorLogs(r *RunResult) {
	errs := 0
	for _, e := range r.LogEvents {
		if e.Level == "error" {
			errs++
		}
	}
	if errs == 0 {
		return
	}
	healthy := r.Status == "completed" && r.ExitCode == 0
	if healthy {
		// Errors logged on a healthy run are usually transient (retried
		// successfully). Keep them out of the summary unless the run
		// failed — but mention the count so they're not invisible.
		return
	}
	fmt.Fprintln(os.Stderr, "  error logs:")
	const maxShown = 5
	shown := 0
	for _, e := range r.LogEvents {
		if e.Level != "error" {
			continue
		}
		if shown >= maxShown {
			break
		}
		msg := e.Message
		if msg == "" {
			msg = "(empty message)"
		}
		fmt.Fprintf(os.Stderr, "    - %s\n", msg)
		shown++
	}
	if errs > maxShown {
		fmt.Fprintf(os.Stderr, "    (and %d more — re-run with --keep-going for full output)\n", errs-maxShown)
	}
}

func isFirstCallFailure(r *RunResult) bool {
	if r.Status != "" {
		return false
	}
	if r.ExitCode == 0 {
		return false
	}
	return true
}

// printForensic emits the high-signal "what killed the worker" block when
// the worker died before /v1/complete. Designed to be the first thing
// authors look at when a test fails — concrete pointers ahead of generic
// "expectation failed" messages.
func printForensic(r *RunResult, m *manifestSummary, flags testFlags) {
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "─── forensic ──────────────────────────────────────")
	if r.LastCall != nil {
		fmt.Fprintf(os.Stderr, "  last sidecar call: %s %s → %d at T+%.2fs\n",
			r.LastCall.Method, r.LastCall.Path, r.LastCall.Status, r.LastCall.OccurredAt.Seconds())
	} else {
		fmt.Fprintln(os.Stderr, "  last sidecar call: none — worker died before any HTTP call.")
	}
	fmt.Fprintf(os.Stderr, "  worker exit: %d\n", r.ExitCode)
	fmt.Fprintf(os.Stderr, "  image: %s\n", m.imageRef)

	if hint := heuristicHint(r); hint != "" {
		fmt.Fprintf(os.Stderr, "  likely cause: %s\n", hint)
	}

	if len(r.WorkerOutputTail) > 0 {
		n := 20
		if len(r.WorkerOutputTail) < n {
			n = len(r.WorkerOutputTail)
		}
		fmt.Fprintf(os.Stderr, "  worker output (last %d lines):\n", n)
		for _, line := range r.WorkerOutputTail[len(r.WorkerOutputTail)-n:] {
			fmt.Fprintf(os.Stderr, "    | %s\n", line)
		}
	}

	if !flags.skipLint {
		// Authors who already saw a lint warning above don't need it
		// repeated; the forensic block aims to avoid duplication. We do
		// re-print here only if the lint pass was skipped.
		return
	}
	fmt.Fprintln(os.Stderr, "  re-run with `aa26-connector lint` for static suggestions.")
}

// heuristicHint maps observed failure shapes to known root causes. Matches
// are intentionally narrow — false positives are worse than no hint.
func heuristicHint(r *RunResult) string {
	if r.LastCall == nil {
		return ""
	}
	if r.LastCall.Path == "/v1/checkpoint" && r.LastCall.Method == "GET" && r.LastCall.Status == 204 {
		return "worker likely crashed parsing the empty 204 response from GET /v1/checkpoint " +
			"(common: json.load on empty body)."
	}
	if r.LastCall.Path == "/v1/invocation" {
		return "worker died after fetching invocation — likely a connection-block parse error " +
			"or missing required field. Inspect REQUEST_DATA env."
	}
	for _, line := range r.WorkerOutputTail {
		l := strings.ToLower(line)
		if strings.Contains(l, "expecting value: line 1 column 1") || strings.Contains(l, "jsondecodeerror") {
			return "JSON decode error in worker output — likely .json() on a 204/empty response."
		}
	}
	return ""
}

func countValid(fs []ValidatedFinding) int {
	n := 0
	for _, f := range fs {
		if f.ValidationOK {
			n++
		}
	}
	return n
}

// countLevel counts log events at the given level. Used by both the
// summary header line ("log events: N (errors: M)") and the printErrorLogs
// short-circuit decision.
func countLevel(es []LogEvent, level string) int {
	n := 0
	for _, e := range es {
		if e.Level == level {
			n++
		}
	}
	return n
}

// humanize turns "demo-fs" / "snowflake_warehouse" into "Demo Fs" / "Snowflake Warehouse"
// for use as the default displayName.
func humanize(name string) string {
	parts := strings.FieldsFunc(name, func(r rune) bool { return r == '-' || r == '_' })
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// ─── schema ────────────────────────────────────────────────────────────

func cmdSchema(args []string) error {
	if len(args) == 0 || args[0] != "entities" {
		return fmt.Errorf("usage: aa26-connector schema entities")
	}
	fmt.Printf("%-24s  %-28s  %s\n", "COLUMN", "TYPE", "DESCRIPTION")
	fmt.Printf("%-24s  %-28s  %s\n", strings.Repeat("─", 24), strings.Repeat("─", 28), strings.Repeat("─", 40))
	for _, c := range entitiesColumns {
		fmt.Printf("%-24s  %-28s  %s\n", c.Name, c.Type, c.Description)
	}
	return nil
}

// ─── validate-mapping ──────────────────────────────────────────────────

func cmdValidateMapping(args []string) error {
	manifestPath := "connector.yaml"
	findingsPath := ""
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--manifest="):
			manifestPath = strings.TrimPrefix(a, "--manifest=")
		case strings.HasPrefix(a, "--"):
			return fmt.Errorf("unknown flag %q", a)
		default:
			findingsPath = a
		}
	}

	// Load and parse manifest
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return fmt.Errorf("read manifest: %w", err)
	}
	jsonBytes, err := yaml.YAMLToJSON(raw)
	if err != nil {
		return fmt.Errorf("manifest yaml: %w", err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(jsonBytes, &manifest); err != nil {
		return fmt.Errorf("manifest decode: %w", err)
	}
	sourceTypes, err := ParseSourceTypes(manifest)
	if err != nil {
		return fmt.Errorf("parse sourceTypes: %w", err)
	}
	if len(sourceTypes) == 0 {
		return fmt.Errorf("manifest has no spec.sourceTypes — nothing to map")
	}

	// Open findings input
	var in *os.File
	if findingsPath == "" || findingsPath == "-" {
		in = os.Stdin
	} else {
		in, err = os.Open(findingsPath)
		if err != nil {
			return fmt.Errorf("open findings: %w", err)
		}
		defer in.Close()
	}

	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 1<<20), 8<<20)
	lineNum := 0
	projected := 0
	skipped := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		lineNum++
		var finding map[string]any
		if err := json.Unmarshal([]byte(line), &finding); err != nil {
			fmt.Fprintf(os.Stderr, "line %d: invalid JSON: %s\n", lineNum, err)
			skipped++
			continue
		}
		row, note := ApplyMapping(sourceTypes, finding)
		if row == nil {
			fmt.Fprintf(os.Stderr, "line %d: skipped: %s\n", lineNum, note)
			skipped++
			continue
		}
		rowJSON, err := json.MarshalIndent(row, "", "  ")
		if err != nil {
			fmt.Fprintf(os.Stderr, "line %d: marshal error: %s\n", lineNum, err)
			skipped++
			continue
		}
		fmt.Printf("// line %d", lineNum)
		if note != "" {
			fmt.Printf(" (%s)", note)
		}
		fmt.Println()
		fmt.Println(string(rowJSON))
		projected++
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read findings: %w", err)
	}
	fmt.Fprintf(os.Stderr, "\n%d projected, %d skipped\n", projected, skipped)
	return nil
}
