// aa26-connector — author tooling for the AA26 connector framework.
//
// Subcommands:
//   new <name>  --lang=python|bash    scaffold a connector skeleton
//   validate [path]                    validate a connector.yaml against the schema
//   test [path] [flags]                run the connector locally against an
//                                      in-process sidecar emulator
//   package [--out=FILE]               bundle the current directory into a
//                                      deployable .tar.gz for upload
//
// Designed for "write a connector in <30 minutes" — no daemon to install,
// no Docker required for `new`/`validate`. `test` shells out to `docker run`
// to exercise the connector image; `package` shells out to `docker save`.
//
// The connector + finding JSON Schemas are embedded at build time so the
// binary is self-contained — no external schema files required.
package main

import (
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

  aa26-connector test [PATH] [flags]
      Run the connector locally against an in-process sidecar emulator
      that matches the production runtime contract. Validates findings
      against the envelope schema, evaluates fixture expectations, and
      streams worker stdout/stderr. Requires docker.

      Flags:
        --fixture=FILE        Test fixture YAML (default ./test-fixture.yaml).
                              Created on the fly if absent.
        --non-interactive     Don't prompt for missing connection params;
                              fail loudly. Use in CI.
        --save-fixture        Write resolved connection params back to the
                              fixture so the next run is reproducible.
        --keep-going          Don't exit non-zero on expectation failures.

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
	case "test":
		err := cmdTest(os.Args[2:])
		fail(err)
	case "package":
		err := cmdPackage(os.Args[2:])
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
		"connector.yaml": fmt.Sprintf(connectorYAMLTmpl, name, display, "0.1.0", name),
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
}

func parseTestFlags(args []string) (testFlags, error) {
	f := testFlags{
		manifestPath: "connector.yaml",
		fixturePath:  "test-fixture.yaml",
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
		case strings.HasPrefix(a, "--"):
			return f, fmt.Errorf("unknown flag %q", a)
		default:
			f.manifestPath = a
		}
	}
	return f, nil
}

// cmdTest is the harness entry point. It validates the manifest, resolves
// the connection block (fixture + interactive prompt), starts an emulator
// on the production port, runs the worker container with the same env
// vars core-api would set, and evaluates the fixture's expectations.
func cmdTest(args []string) error {
	flags, err := parseTestFlags(args)
	if err != nil {
		return err
	}

	if err := cmdValidate([]string{flags.manifestPath}); err != nil {
		return fmt.Errorf("manifest invalid: %w", err)
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

	result := &RunResult{}
	emulator := NewEmulator(fixture.OperationName(), connection, findingSchema, result)

	listener, err := listenEmulator()
	if err != nil {
		return err
	}
	server := &http.Server{Handler: emulator, ReadHeaderTimeout: 10 * time.Second}
	serverErrCh := make(chan error, 1)
	go func() { serverErrCh <- server.Serve(listener) }()

	shutdownCtx, cancelShutdown := context.WithCancel(context.Background())
	defer cancelShutdown()
	defer func() {
		shutdownTimeout, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownTimeout)
	}()

	fmt.Fprintf(os.Stderr, "→ emulator listening on %s\n", emulatorAddr)
	fmt.Fprintf(os.Stderr, "→ running %s (op=%s, function_type=%s)\n",
		manifest.imageRef, fixture.OperationName(), fixture.Op)

	dockerEnv := buildWorkerEnv(fixture, manifest, connection)
	exitCode, err := runWorker(shutdownCtx, manifest.imageRef, dockerEnv, emulator.Done())
	if err != nil {
		return err
	}
	result.ExitCode = exitCode

	failures := fixture.Evaluate(result)
	printSummary(result, failures)

	if len(failures) > 0 && !flags.keepGoing {
		return fmt.Errorf("%d expectation(s) failed", len(failures))
	}
	return nil
}

// manifestSummary holds the bits of connector.yaml the harness needs at
// run time. Re-parsing as a typed struct avoids walking the generic map
// twice.
type manifestSummary struct {
	name       string
	version    string
	imageRef   string
	pullPolicy string
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
		} `json:"spec"`
	}
	if err := json.Unmarshal(jsonBytes, &m); err != nil {
		return nil, fmt.Errorf("manifest decode: %w", err)
	}
	tag := m.Spec.Image.Tag
	if tag == "" {
		tag = "dev"
	}
	return &manifestSummary{
		name:       m.Metadata.Name,
		version:    m.Metadata.Version,
		imageRef:   m.Spec.Image.Repository + ":" + tag,
		pullPolicy: m.Spec.Image.PullPolicy,
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
func runWorker(ctx context.Context, imageRef string, env []string, done <-chan struct{}) (int, error) {
	var args []string
	if runtime.GOOS == "linux" {
		args = []string{"run", "--rm", "--network=host"}
	} else {
		// macOS / Windows: host.docker.internal reaches the host from inside a container.
		args = []string{"run", "--rm"}
		env = append(env, "SIDECAR_URL=http://host.docker.internal:8089")
	}
	for _, e := range env {
		args = append(args, "-e", e)
	}
	args = append(args, imageRef)

	cmd := exec.CommandContext(ctx, "docker", args...)
	cmd.Stdout = streamPrefix(os.Stderr, "worker | ")
	cmd.Stderr = streamPrefix(os.Stderr, "worker | ")
	if err := cmd.Start(); err != nil {
		if isDockerPermissionError(err) {
			fmt.Fprintln(os.Stderr, "  (docker without sudo failed — retrying with sudo)")
			cmd = exec.CommandContext(ctx, "sudo", append([]string{"docker"}, args...)...)
			cmd.Stdout = streamPrefix(os.Stderr, "worker | ")
			cmd.Stderr = streamPrefix(os.Stderr, "worker | ")
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
// before forwarding to `w`. Used so the harness's own stderr lines and
// the worker's interleaved output are easy to tell apart.
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

func printSummary(r *RunResult, failures []string) {
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
	if len(failures) == 0 {
		fmt.Fprintln(os.Stderr, "  result:       ✓ pass")
		return
	}
	fmt.Fprintln(os.Stderr, "  result:       ✗ fail")
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "    - %s\n", f)
	}
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
