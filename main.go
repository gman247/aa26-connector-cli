// aa26-connector — author tooling for the AA26 connector framework.
//
// Subcommands:
//   new <name>  --lang=python|bash    scaffold a connector skeleton
//   validate [path]                    validate a connector.yaml against the schema
//   test [path] --root=DIR             run the connector locally against an
//                                      in-process sidecar emulator
//   package [--out=FILE]               bundle the current directory into a
//                                      deployable .tar.gz for upload
//
// Designed for "write a connector in <30 minutes" — no daemon to install,
// no Docker required for `new`/`validate`. `test` shells out to `docker run`
// to exercise the connector image; `package` shells out to `docker save`.
//
// The connector.yaml JSON Schema is embedded at build time so the binary
// is self-contained — no external schema file required.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"
	"sigs.k8s.io/yaml"
)

//go:embed schema/connector.schema.json
var embeddedSchema []byte

const usage = `aa26-connector — author tooling for the AA26 connector framework

Usage:
  aa26-connector new <name> --lang=python|bash [--dir=PATH]
      Scaffold a new connector. Drops connector.yaml + Dockerfile +
      handler skeleton into ./<name>/ (or --dir).

  aa26-connector validate [PATH]
      Validate connector.yaml against the schema. Defaults to ./connector.yaml.

  aa26-connector test [PATH] --root=DIR [--op=test_connection|scan]
      Run the connector locally against an in-process sidecar emulator.
      Prints findings as they arrive. Requires docker.

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
	fmt.Printf("\nNext:\n  cd %s\n  aa26-connector validate\n  # build + test:\n  docker build -t localhost/%s:dev .\n  aa26-connector test --root=/tmp\n", dir, name)
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

// loadSchema returns the embedded connector.yaml JSON Schema. To override
// the embedded copy (e.g. testing against an unreleased schema), set
// CONNECTOR_SCHEMA to a path on disk.
func loadSchema() (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()

	if override := os.Getenv("CONNECTOR_SCHEMA"); override != "" {
		s, err := c.Compile(override)
		if err != nil {
			return nil, fmt.Errorf("compile schema from CONNECTOR_SCHEMA=%s: %w", override, err)
		}
		return s, nil
	}

	const id = "https://connectors.netwrix.io/schema/connector.schema.json"
	if err := c.AddResource(id, strings.NewReader(string(embeddedSchema))); err != nil {
		return nil, fmt.Errorf("register embedded schema: %w", err)
	}
	s, err := c.Compile(id)
	if err != nil {
		return nil, fmt.Errorf("compile embedded schema: %w", err)
	}
	return s, nil
}

// ─── test ──────────────────────────────────────────────────────────────

func cmdTest(args []string) error {
	path := "connector.yaml"
	rootPath := ""
	op := "scan"
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "--root="):
			rootPath = strings.TrimPrefix(a, "--root=")
		case strings.HasPrefix(a, "--op="):
			op = strings.TrimPrefix(a, "--op=")
		case !strings.HasPrefix(a, "-"):
			path = a
		}
	}
	if rootPath == "" {
		return errors.New("--root=PATH required (a directory the connector will scan)")
	}
	// 1. Validate the manifest first.
	if err := cmdValidate([]string{path}); err != nil {
		return fmt.Errorf("manifest invalid: %w", err)
	}
	// 2. Read it for image/repository.
	raw, _ := os.ReadFile(path)
	jsonBytes, _ := yaml.YAMLToJSON(raw)
	var m struct {
		Metadata struct {
			Name string `json:"name"`
		} `json:"metadata"`
		Spec struct {
			Image struct {
				Repository string `json:"repository"`
				Tag        string `json:"tag"`
			} `json:"image"`
		} `json:"spec"`
	}
	_ = json.Unmarshal(jsonBytes, &m)
	tag := m.Spec.Image.Tag
	if tag == "" {
		tag = "dev"
	}
	imageRef := m.Spec.Image.Repository + ":" + tag

	// 3. Start the in-process sidecar emulator.
	port, err := pickPort()
	if err != nil {
		return err
	}
	emulator := newSidecarEmulator(rootPath, op, m.Metadata.Name)
	server := &http.Server{Addr: fmt.Sprintf("127.0.0.1:%d", port), Handler: emulator}
	go func() { _ = server.ListenAndServe() }()
	defer server.Shutdown(context.Background())

	// Wait for emulator to listen.
	if err := waitListening(port, 5*time.Second); err != nil {
		return err
	}

	// 4. Run the connector image with SIDECAR_URL pointed at the emulator.
	// Use --network=host so localhost in the container resolves to the
	// host's emulator port. Simpler than wiring a docker network.
	fmt.Printf("→ running %s (op=%s, root=%s)\n", imageRef, op, rootPath)
	dockerArgs := []string{"run", "--rm", "--network=host",
		"-e", fmt.Sprintf("SIDECAR_URL=http://127.0.0.1:%d", port),
		imageRef}
	if err := runDocker(dockerArgs); err != nil {
		return fmt.Errorf("connector exited: %w", err)
	}

	// 5. Summarize.
	fmt.Printf("\n✓ test complete\n")
	fmt.Printf("  invocations:  %d\n", emulator.invocationCount())
	fmt.Printf("  findings:     %d\n", emulator.findingCount())
	fmt.Printf("  progress:     %d\n", emulator.progressCount())
	fmt.Printf("  logs:         %d\n", emulator.logCount())
	if !emulator.completed() {
		return errors.New("connector did not POST /v1/complete — see runtime-contract docs")
	}
	fmt.Printf("  status:       %s\n", emulator.completion())
	return nil
}

// ─── sidecar emulator ──────────────────────────────────────────────────

type sidecarEmulator struct {
	rootPath, operation, sourceID string

	mu          sync.Mutex
	invocs      int
	findings    int
	progress    int
	logs        int
	completed_  string
}

func newSidecarEmulator(rootPath, op, name string) *sidecarEmulator {
	return &sidecarEmulator{
		rootPath:  rootPath,
		operation: op,
		sourceID:  "00000000-0000-0000-0000-000000000001",
	}
}

func (e *sidecarEmulator) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/healthz":
		_, _ = w.Write([]byte("ok\n"))
	case "/v1/invocation":
		e.mu.Lock()
		e.invocs++
		e.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"operation":   e.operation,
			"executionId": "00000000-0000-0000-0000-000000000099",
			"sourceId":    e.sourceID,
			"source":      map[string]interface{}{"rootPath": e.rootPath},
			"scan":        map[string]interface{}{},
		})
	case "/v1/findings":
		body, _ := io.ReadAll(r.Body)
		count := strings.Count(strings.TrimRight(string(body), "\n"), "\n") + 1
		e.mu.Lock()
		e.findings += count
		e.mu.Unlock()
		fmt.Fprintln(os.Stderr, "  findings batch (", count, " events)")
		_, _ = w.Write([]byte(`{"accepted":0}`))
	case "/v1/progress":
		e.mu.Lock()
		e.progress++
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "/v1/log":
		e.mu.Lock()
		e.logs++
		e.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	case "/v1/control":
		// always-no-signal, with a small delay so connectors that long-poll
		// don't burn cpu.
		time.Sleep(500 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	case "/v1/checkpoint":
		w.WriteHeader(http.StatusNoContent)
	case "/v1/complete":
		body, _ := io.ReadAll(r.Body)
		var summary struct {
			Status string `json:"status"`
		}
		_ = json.Unmarshal(body, &summary)
		e.mu.Lock()
		e.completed_ = summary.Status
		if e.completed_ == "" {
			e.completed_ = "completed"
		}
		e.mu.Unlock()
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	default:
		http.NotFound(w, r)
	}
}

func (e *sidecarEmulator) invocationCount() int { e.mu.Lock(); defer e.mu.Unlock(); return e.invocs }
func (e *sidecarEmulator) findingCount() int    { e.mu.Lock(); defer e.mu.Unlock(); return e.findings }
func (e *sidecarEmulator) progressCount() int   { e.mu.Lock(); defer e.mu.Unlock(); return e.progress }
func (e *sidecarEmulator) logCount() int        { e.mu.Lock(); defer e.mu.Unlock(); return e.logs }
func (e *sidecarEmulator) completed() bool      { e.mu.Lock(); defer e.mu.Unlock(); return e.completed_ != "" }
func (e *sidecarEmulator) completion() string   { e.mu.Lock(); defer e.mu.Unlock(); return e.completed_ }

// ─── helpers ───────────────────────────────────────────────────────────

func pickPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := l.Addr().(*net.TCPAddr).Port
	_ = l.Close()
	return port, nil
}

// runDocker executes `docker <args>` with stdout/stderr inherited.
// Falls back to `sudo docker` on permission-denied to support hosts
// where the user isn't in the docker group yet.
func runDocker(args []string) error {
	cmd := exec.Command("docker", args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	if err == nil {
		return nil
	}
	// Probe for the canonical permission error message.
	probeOut, probeErr := exec.Command("docker", "info").CombinedOutput()
	if probeErr != nil && strings.Contains(string(probeOut), "permission denied") {
		fmt.Fprintln(os.Stderr, "  (docker without sudo failed — retrying with sudo)")
		cmd = exec.Command("sudo", append([]string{"docker"}, args...)...)
		cmd.Stdout = os.Stderr
		cmd.Stderr = os.Stderr
		return cmd.Run()
	}
	return err
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

func waitListening(port int, timeout time.Duration) error {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("emulator did not start listening on %s within %s", addr, timeout)
}
