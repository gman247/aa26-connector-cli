// Real-runtime mode (--real-runtime) runs the actual connector-prototype
// runtime sidecar container instead of the in-process emulator. Eliminates
// emulator-vs-production drift entirely: if your connector works against
// the real sidecar locally, you have a much stronger guarantee it works in
// cluster.
//
// Architecture (mirrors a real K8s pod):
//
//   ┌──── runtime container ────┐    ┌──── worker container ────┐
//   │  127.0.0.1:8089 listener  │    │  hardcoded localhost:8089│
//   └─────────────┬─────────────┘    └─────────────┬────────────┘
//                 │ same network namespace via                  │
//                 └──── docker --network=container:<runtime> ───┘
//
// We mount an invocation file into the runtime container so it serves
// the same /v1/invocation contents the in-process emulator would, and
// point FINDINGS_OUTPUT_FILE at a host file we can post-process for
// schema validation. Findings forwarding to data-ingestion is left
// disabled — the harness has no AA26 cluster to forward to.
//
// This mode is opt-in (via --real-runtime). Authors who want pure
// determinism + the validation feedback loop stick with the emulator;
// authors who suspect emulator drift reach for --real-runtime.
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v5"
)

// RealRuntimeConfig describes how to launch the real runtime sidecar.
type RealRuntimeConfig struct {
	// Image is the runtime container ref. Defaults to
	// "docker.io/connector-prototype/runtime:dev" — matches what
	// connector-api configures in production.
	Image string

	// ContainerName is what we pass to `docker run --name`. The worker's
	// `--network=container:<name>` reference uses this. A unique name
	// per harness run avoids collisions with leftover containers.
	ContainerName string
}

// runRealRuntime starts the runtime container, runs the worker on its
// network namespace, harvests findings from the mounted output file, and
// validates them against the embedded schema. The signature mirrors the
// in-process path so cmdTest can swap them transparently.
//
// Returns the docker exit code of the worker. Result is updated in place.
func runRealRuntime(
	ctx context.Context,
	rrCfg RealRuntimeConfig,
	manifest *manifestSummary,
	fixture *Fixture,
	connection map[string]any,
	findingSchema *jsonschema.Schema,
	result *RunResult,
) (int, error) {
	if rrCfg.Image == "" {
		rrCfg.Image = "docker.io/connector-prototype/runtime:dev"
	}
	if rrCfg.ContainerName == "" {
		rrCfg.ContainerName = "aa26-rt-emu"
	}

	if err := ensureWorkerImage(rrCfg.Image); err != nil {
		return -1, fmt.Errorf("real-runtime image %s missing — pull or build it first: %w", rrCfg.Image, err)
	}

	tmpDir, err := os.MkdirTemp("", "aa26-rt-*")
	if err != nil {
		return -1, fmt.Errorf("scratch dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	invocationPath := tmpDir + "/invocation.json"
	if err := writeInvocationFile(invocationPath, fixture, connection); err != nil {
		return -1, err
	}

	findingsPath := tmpDir + "/findings.ndjson"
	if err := os.WriteFile(findingsPath, nil, 0o644); err != nil {
		return -1, fmt.Errorf("findings sink: %w", err)
	}

	// Make sure no zombie container holds the name. `docker rm -f` is a
	// no-op when the container doesn't exist; the harness owns this name
	// (aa26-rt-emu) and the user shouldn't have anything else using it.
	_ = exec.Command("docker", "rm", "-f", rrCfg.ContainerName).Run()

	runtimeArgs := []string{
		"run", "--rm", "-d",
		"--name", rrCfg.ContainerName,
		// Publish the listener so the worker container — which joins
		// this network namespace — can reach 127.0.0.1:8089. Note that
		// `--network=container:<name>` shares the loopback verbatim, so
		// no port mapping is needed.
		"-e", "CONNECTOR_INVOCATION_FILE=/aa26/invocation.json",
		"-e", "FINDINGS_OUTPUT_FILE=/aa26/findings.ndjson",
		"-v", invocationPath + ":/aa26/invocation.json:ro",
		"-v", findingsPath + ":/aa26/findings.ndjson",
		rrCfg.Image,
	}
	runtimeID, err := dockerRunDetached(runtimeArgs)
	if err != nil {
		return -1, fmt.Errorf("start real runtime: %w", err)
	}
	defer func() {
		_ = exec.Command("docker", "rm", "-f", runtimeID).Run()
	}()

	// Stream the runtime's stdout/stderr in the background — same prefix
	// treatment as the worker so authors can tell who logged what.
	go streamContainerLogs(ctx, runtimeID, "runtime | ")

	// Build the worker env exactly as in-process mode would.
	env := buildWorkerEnv(fixture, manifest, connection)

	// Run the worker on the runtime's network namespace.
	workerArgs := []string{"run", "--rm", "--network=container:" + runtimeID}
	for _, e := range env {
		workerArgs = append(workerArgs, "-e", e)
	}
	workerArgs = append(workerArgs, manifest.imageRef)

	cmd := exec.CommandContext(ctx, "docker", workerArgs...)
	tail := &outputTail{cap: workerOutputTailMax}
	cmd.Stdout = teeWithTail(os.Stderr, "worker | ", tail)
	cmd.Stderr = teeWithTail(os.Stderr, "worker | ", tail)

	exitCode, runErr := runCommandReportExit(cmd)
	result.WorkerOutputTail = tail.snapshot()
	if runErr != nil {
		return exitCode, runErr
	}

	// Worker has exited. Harvest the findings file.
	if err := loadAndValidateFindings(findingsPath, findingSchema, result); err != nil {
		return exitCode, fmt.Errorf("read findings: %w", err)
	}

	// In real-runtime mode we don't see /v1/complete from the harness side
	// (the runtime swallows it before exiting). Fall back to the docker
	// exit code as the implicit terminal status — the same behavior as
	// the in-process path when the worker dies before /v1/complete.
	if result.Status == "" {
		if exitCode == 0 {
			result.Status = "completed"
		} else {
			result.Status = "failed"
		}
	}
	return exitCode, nil
}

// writeInvocationFile produces the JSON the runtime serves on /v1/invocation.
// Shape matches runtime/main.go's invocation struct.
func writeInvocationFile(path string, f *Fixture, connection map[string]any) error {
	body := map[string]any{
		"operation":   f.OperationName(),
		"executionId": invocationExecutionID,
		"sourceId":    invocationSourceID,
		"source":      connection,
		"scan":        map[string]any{},
	}
	raw, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal invocation: %w", err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return fmt.Errorf("write invocation: %w", err)
	}
	return nil
}

func dockerRunDetached(args []string) (string, error) {
	cmd := exec.Command("docker", args...)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return "", fmt.Errorf("docker run: %s", strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func streamContainerLogs(ctx context.Context, containerID, prefix string) {
	cmd := exec.CommandContext(ctx, "docker", "logs", "-f", containerID)
	cmd.Stdout = streamPrefix(os.Stderr, prefix)
	cmd.Stderr = streamPrefix(os.Stderr, prefix)
	_ = cmd.Run()
}

// runCommandReportExit runs cmd, returning the exit code and any wrapping
// error (start failure, cancellation). A non-zero exit code is reported
// without an error — that's normal termination, just unsuccessful.
func runCommandReportExit(cmd *exec.Cmd) (int, error) {
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode(), nil
		}
		return -1, fmt.Errorf("worker run: %w", err)
	}
	return 0, nil
}

// loadAndValidateFindings reads the runtime's findings sink (one envelope
// per line) and replays each through the same schema validator the
// in-process emulator uses. The harness can then assert on counts/types
// the same way regardless of which sidecar produced them.
func loadAndValidateFindings(path string, schema *jsonschema.Schema, result *RunResult) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<20), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		vf := ValidatedFinding{Raw: line}
		var probe map[string]any
		if err := json.Unmarshal([]byte(line), &probe); err != nil {
			vf.ValidationOK = false
			vf.ValidationErr = "invalid JSON: " + err.Error()
			result.Findings = append(result.Findings, vf)
			continue
		}
		if k, ok := probe["kind"].(string); ok {
			vf.Kind = k
		}
		if t, ok := probe["type"].(string); ok {
			vf.Type = t
		}
		if err := schema.Validate(probe); err != nil {
			vf.ValidationOK = false
			vf.ValidationErr = compactSchemaError(err)
		} else {
			vf.ValidationOK = true
		}
		result.Findings = append(result.Findings, vf)
	}
	return scanner.Err()
}

// outputTail keeps the trailing N lines of an io.Writer. Used to capture
// worker stdout/stderr for the forensic summary without retaining unbounded
// output in memory.
type outputTail struct {
	cap   int
	lines []string
}

func (t *outputTail) addLine(s string) {
	t.lines = append(t.lines, s)
	if len(t.lines) > t.cap {
		t.lines = t.lines[len(t.lines)-t.cap:]
	}
}

func (t *outputTail) snapshot() []string {
	out := make([]string, len(t.lines))
	copy(out, t.lines)
	return out
}

// teeWithTail returns a writer that prefixes each line for live display
// AND records it in the supplied tail buffer. The tail snapshot is what
// the forensic summary quotes when the worker dies before /v1/complete.
func teeWithTail(w io.Writer, prefix string, tail *outputTail) io.Writer {
	pr, pw := io.Pipe()
	go func() {
		buf := make([]byte, 4096)
		var line strings.Builder
		for {
			n, err := pr.Read(buf)
			if n > 0 {
				for _, b := range buf[:n] {
					line.WriteByte(b)
					if b == '\n' {
						s := line.String()
						_, _ = w.Write([]byte(prefix + s))
						tail.addLine(strings.TrimRight(s, "\n"))
						line.Reset()
					}
				}
			}
			if err != nil {
				if line.Len() > 0 {
					s := line.String()
					_, _ = w.Write([]byte(prefix + s + "\n"))
					tail.addLine(strings.TrimRight(s, "\n"))
				}
				return
			}
		}
	}()
	return pw
}
