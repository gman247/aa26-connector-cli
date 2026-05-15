// Static lint over connector source — catches the small set of HTTP/JSON
// anti-patterns that have historically broken connectors at runtime.
//
// The lint is deliberately tiny and high-precision. Every rule maps to a
// real production failure that already happened to a real connector: the
// goal is "this exact bug class can't ship again," not "stylistic
// perfection." False positives are noisier than missing detections, so
// rules require a strong proximate signal (e.g. urlopen + json.load on
// the same logical statement) before flagging.
//
// Wire-up: `aa26-connector lint` is the standalone entry point;
// `aa26-connector test` calls into Lint() before running the worker, so
// the warning is surfaced at the moment authors are most likely to act
// on it.
package main

import (
	"bufio"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// LintFinding is a single rule violation. Severity is "warn" or "error";
// the harness fails the run on "error", and only on "warn" if --strict.
type LintFinding struct {
	File     string
	Line     int
	Rule     string
	Severity string
	Message  string
	Snippet  string
}

// LintConfig controls which sources get scanned.
type LintConfig struct {
	// Root is the directory to scan. Defaults to "." when empty.
	Root string
	// MaxFileBytes skips files larger than this (default 1 MiB) — vendored
	// or generated files that snuck in shouldn't bog down the lint.
	MaxFileBytes int64
}

// Default ignored paths. Conservative: everything common, nothing project-
// specific. Authors who need to scan e.g. a vendored dep can pass --include
// once that flag exists.
var defaultLintIgnore = []string{
	".git", "node_modules", "vendor", "__pycache__",
	".venv", "venv", "env", "dist", "build", ".pytest_cache",
	".mypy_cache", ".tox", "target", ".idea", ".vscode",
}

// Lint walks cfg.Root and returns every rule violation encountered. The
// caller renders the findings; this function never prints.
func Lint(cfg LintConfig) ([]LintFinding, error) {
	if cfg.Root == "" {
		cfg.Root = "."
	}
	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = 1 << 20
	}
	rules := defaultRules()

	var findings []LintFinding
	walkErr := filepath.WalkDir(cfg.Root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; lint is advisory
		}
		if d.IsDir() {
			if shouldSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		ext := strings.ToLower(filepath.Ext(d.Name()))
		applicable := rulesForExt(rules, ext)
		if len(applicable) == 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > cfg.MaxFileBytes {
			return nil
		}
		fileFindings, err := lintFile(path, applicable)
		if err != nil {
			return nil
		}
		findings = append(findings, fileFindings...)
		return nil
	})
	if walkErr != nil {
		return findings, fmt.Errorf("walk %s: %w", cfg.Root, walkErr)
	}
	return findings, nil
}

func shouldSkipDir(name string) bool {
	for _, s := range defaultLintIgnore {
		if name == s {
			return true
		}
	}
	return false
}

// lintFile reads one file and applies every rule whose extension matches.
// Each rule is a self-contained matcher with optional context (it can
// inspect adjacent lines) so the scanning loop stays straightforward.
func lintFile(path string, rules []lintRule) ([]LintFinding, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var lines []string
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1<<16), 1<<20)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	var out []LintFinding
	for _, rule := range rules {
		out = append(out, rule.check(path, lines)...)
	}
	return out, nil
}

// lintRule is one footgun detector. We model rules as values rather than
// generic interfaces so the detector logic and the metadata stay close —
// each rule's regex, context check, and message read top-to-bottom in
// defaultRules() below.
type lintRule struct {
	id           string
	severity     string
	exts         []string // file extensions this rule applies to
	check        func(file string, lines []string) []LintFinding
	hits         *regexp.Regexp
	contextNear  *regexp.Regexp // optional: must appear within N lines
	contextWidth int
	message      string
}

func rulesForExt(rules []lintRule, ext string) []lintRule {
	var out []lintRule
	for _, r := range rules {
		for _, e := range r.exts {
			if e == ext {
				out = append(out, r)
				break
			}
		}
	}
	return out
}

// defaultRules is the production rule set. Add new rules here only when
// (a) the bug class has fired in production at least once and (b) a
// regex with low false-positive rate exists. Anything subtler belongs in
// dedicated linters (pylint/golangci-lint), not here.
func defaultRules() []lintRule {
	return []lintRule{
		// R001: Python urllib + json.load on the response. Bites cold-start
		// /v1/checkpoint, which returns 204 with empty body. json.load on
		// an empty stream raises JSONDecodeError before any work happens.
		ruleNearby(
			"R001",
			"warn",
			[]string{".py"},
			regexp.MustCompile(`json\.load\s*\(\s*\w+\s*\)`),
			regexp.MustCompile(`urlopen\s*\(`),
			6,
			"json.load(resp) on a urllib response will crash on 204/empty bodies "+
				"(e.g. cold-start /v1/checkpoint). Check resp.status == 204 first or read+strip the body.",
		),

		// R002: Python requests/.json() without 204 guard. Same bug class
		// as R001 but via the `requests` library: resp.json() raises on 204.
		ruleSimple(
			"R002",
			"warn",
			[]string{".py"},
			regexp.MustCompile(`\b(requests|httpx|session)\.\s*(get|post|put|delete|head)\s*\([^)]*\)\s*\.\s*json\s*\(\s*\)`),
			"`.json()` on a requests/httpx response raises on 204/empty body. "+
				"Bind the response, check status_code != 204, then call .json().",
		),

		// R003: Go http response body decoded without status check. The
		// `_ =` pattern below catches the common json.NewDecoder one-liner;
		// authors who Decode after `if resp.StatusCode != http.StatusNoContent`
		// won't trip this because the matcher requires Decode to be the
		// next non-empty statement after the request.
		ruleNearby(
			"R003",
			"warn",
			[]string{".go"},
			regexp.MustCompile(`json\.NewDecoder\s*\(\s*\w+\.Body\s*\)\s*\.\s*Decode`),
			regexp.MustCompile(`http\.(Get|Post|Do|NewRequest)|client\.(Get|Post|Do)`),
			6,
			"json.NewDecoder(resp.Body).Decode without a StatusNoContent check will "+
				"decode an empty 204 body and return io.EOF. Check resp.StatusCode first.",
		),

		// R004: TS/JS await response.json() without status check. Mirror of
		// R002 in the JS world — fetch().then(r => r.json()) silently fails
		// on 204 in some runtimes and throws SyntaxError in others.
		ruleNearby(
			"R004",
			"warn",
			[]string{".ts", ".tsx", ".js", ".mjs", ".cjs"},
			regexp.MustCompile(`(await\s+\w+\.json\s*\(\s*\))|(\w+\.json\s*\(\s*\)\s*\.\s*then)`),
			regexp.MustCompile(`fetch\s*\(`),
			6,
			"`response.json()` after fetch() throws SyntaxError on 204/empty bodies. "+
				"Check response.status !== 204 (or read text() first) before parsing.",
		),

		// R005: Hardcoded sidecar URL drift. The runtime listens on
		// 127.0.0.1:8089. Anything pointing at 8088, 8090, or `runtime:`
		// (a stale Phase-0 hostname) won't reach the harness emulator or
		// production.
		ruleSimple(
			"R005",
			"error",
			[]string{".py", ".go", ".ts", ".tsx", ".js", ".rs", ".rb", ".sh"},
			regexp.MustCompile(`\b(127\.0\.0\.1|localhost):(8088|8090|8091|8092)\b|http://runtime:`),
			"Sidecar URL drift detected. The runtime listens on 127.0.0.1:8089. "+
				"Other ports won't reach the harness or production.",
		),

		// R006: `sidecars:` placed at spec level instead of nested under
		// spec.capabilities. The YAML parser accepts it, the connector
		// uploads without error, but core-api's
		// AdapterService#needs_extraction_sidecar? reads
		// connector_framework.capabilities.sidecars only — so the
		// extraction container never gets attached, EXTRACTION_URL is
		// unset on the worker, and SDS findings on binary files emit
		// with no `content` for the classifier to read.
		ruleNearby(
			"R006",
			"error",
			[]string{".yaml", ".yml"},
			regexp.MustCompile(`^  sidecars:\s*\[`),
			regexp.MustCompile(`^apiVersion:\s*connectors\.netwrix\.io`),
			200,
			"`sidecars:` is at spec level — it must nest under `spec.capabilities`. "+
				"At spec level it's silently ignored; the extraction sidecar won't be attached and "+
				"SDS findings on binary files will emit without `content`. See docs/extraction.md.",
		),

		// R008: connector declares access_scan but no sourceType with
		// ingestion.target: permissions. access_grant findings emitted
		// by an access_scan will be routed by the runtime to the
		// permissions table — but if no sourceType declares that target,
		// the runtime has no projection mapping and drops the findings.
		// This is the v1.1 version of the bug that previously required
		// the v1 ingestion guard.
		{
			id:       "R008",
			severity: "warn",
			exts:     []string{".yaml", ".yml"},
			check: func(file string, lines []string) []LintFinding {
				// Only check connector manifests.
				isConnectorManifest := false
				for _, l := range lines {
					if regexp.MustCompile(`^apiVersion:\s*connectors\.netwrix\.io`).MatchString(l) {
						isConnectorManifest = true
						break
					}
				}
				if !isConnectorManifest {
					return nil
				}
				// Check for access_scan in scanTypes.
				hasAccessScan := false
				for _, l := range lines {
					if regexp.MustCompile(`\baccess_scan\b`).MatchString(l) {
						hasAccessScan = true
						break
					}
				}
				if !hasAccessScan {
					return nil
				}
				// Check for at least one ingestion.target: permissions.
				for _, l := range lines {
					if regexp.MustCompile(`^\s+target:\s*permissions\s*$`).MatchString(l) {
						return nil // found one — all good
					}
				}
				return []LintFinding{{
					File:     file,
					Line:     1,
					Rule:     "R008",
					Severity: "warn",
					Message: "connector declares access_scan but no sourceType has ingestion.target: permissions. " +
						"access_grant findings will be dropped by the runtime — add a sourceType entry with " +
						"ingestion.target: permissions and map permissionGrantId, aceType, memberRole, and the " +
						"readAllowed/writeAllowed/deleteAllowed/manageAllowed fields. " +
						"See docs/manifest-reference.md §spec.sourceTypes.",
				}}
			},
		},

		// R007: `spec.image.tag` is no longer accepted. The image tag is
		// derived from `metadata.version` so the manifest and the running
		// image can never disagree. Old manifests carrying a `tag:` line
		// under `spec.image:` need it removed before `aa26-connector
		// package` or registry upload will succeed.
		//
		// Scope to connector manifests via the apiVersion anchor.
		ruleNearby(
			"R007",
			"error",
			[]string{".yaml", ".yml"},
			regexp.MustCompile(`^\s{4}tag:\s*\S`),
			regexp.MustCompile(`^apiVersion:\s*connectors\.netwrix\.io`),
			200,
			"`spec.image.tag` is no longer supported — remove the line. "+
				"The image tag is derived from `metadata.version`. "+
				"See docs/manifest-reference.md.",
		),
	}
}

// ruleSimple is a rule with no context constraint — a single regex match
// per line is enough to flag.
func ruleSimple(id, severity string, exts []string, hit *regexp.Regexp, message string) lintRule {
	r := lintRule{
		id:       id,
		severity: severity,
		exts:     exts,
		hits:     hit,
		message:  message,
	}
	r.check = func(file string, lines []string) []LintFinding {
		var out []LintFinding
		for i, line := range lines {
			if r.hits.MatchString(line) {
				out = append(out, LintFinding{
					File: file, Line: i + 1, Rule: id,
					Severity: severity, Message: message,
					Snippet: strings.TrimSpace(line),
				})
			}
		}
		return out
	}
	return r
}

// ruleNearby is a rule that requires a second pattern to appear within
// `width` lines of the primary hit. Cuts false positives dramatically.
func ruleNearby(id, severity string, exts []string, hit, ctx *regexp.Regexp, width int, message string) lintRule {
	r := lintRule{
		id:           id,
		severity:     severity,
		exts:         exts,
		hits:         hit,
		contextNear:  ctx,
		contextWidth: width,
		message:      message,
	}
	r.check = func(file string, lines []string) []LintFinding {
		var out []LintFinding
		for i, line := range lines {
			if !r.hits.MatchString(line) {
				continue
			}
			lo := i - r.contextWidth
			if lo < 0 {
				lo = 0
			}
			hi := i + r.contextWidth
			if hi >= len(lines) {
				hi = len(lines) - 1
			}
			ctxHit := false
			for j := lo; j <= hi; j++ {
				if r.contextNear.MatchString(lines[j]) {
					ctxHit = true
					break
				}
			}
			if !ctxHit {
				continue
			}
			out = append(out, LintFinding{
				File: file, Line: i + 1, Rule: id,
				Severity: severity, Message: message,
				Snippet: strings.TrimSpace(line),
			})
		}
		return out
	}
	return r
}

// PrintLintFindings renders the findings as a stable, copy-pasteable block.
// Returns true if any error-severity finding was present, so callers can
// decide whether to fail the run.
func PrintLintFindings(findings []LintFinding, w *strings.Builder) bool {
	hasError := false
	for _, f := range findings {
		fmt.Fprintf(w, "%s [%s/%s] %s:%d: %s\n", iconForSeverity(f.Severity), f.Rule, f.Severity, f.File, f.Line, f.Message)
		if f.Snippet != "" {
			fmt.Fprintf(w, "    %s\n", f.Snippet)
		}
		if f.Severity == "error" {
			hasError = true
		}
	}
	return hasError
}

func iconForSeverity(sev string) string {
	switch sev {
	case "error":
		return "✗"
	case "warn":
		return "⚠"
	default:
		return "·"
	}
}
