# Text extraction (Tika + OCR) from your connector

The framework ships a per-Pod **extraction sidecar** so connectors that need to pull text out of files (PDF, DOCX, XLSX, PPTX, HTML, RTF, ePub, scanned images) don't have to bundle Apache Tika or Tesseract themselves. You opt in via your manifest, call a one-line SDK function from your worker, and the framework handles the rest — image bloat, JVM tuning, startup probes, OCR language packs.

This is the right tool for any connector emitting `content`-bearing findings during sensitive-data scans (the input to the framework's classifier relay). If your connector doesn't deal with files (a permissions-only catalog scanner, a SaaS API that returns text directly), skip the sidecar — it costs ~768 MiB of pod memory.

## TL;DR

1. Add `sidecars: [extraction]` to your manifest's `spec.capabilities`.
2. Read `EXTRACTION_URL` from your worker's env (the framework sets it for you).
3. POST file bytes to `${EXTRACTION_URL}/v1/extract` — or use the SDK in your language.

```yaml
# connector.yaml
spec:
  capabilities:
    operations: [test_connection, scan]
    scanTypes: [access_scan, sensitive_data_scan]
    sidecars: [extraction]
  extraction:
    maxDepth: 2     # default — unwrap one level of archive (ZIP → its entries)
```

`extraction.maxDepth` is optional. It controls how deep into nested archives the sidecar will recurse before stopping. The default of 2 covers the common case (a ZIP containing PDFs/DOCX). Bump to 3+ when your sources commonly include nested archives. Hard-capped at 10.

> **⚠️ Indentation gotcha — `sidecars` must nest under `capabilities`.** Core-api reads sidecar declarations from `spec.capabilities.sidecars` only. A `sidecars: [extraction]` line at `spec.sidecars` (peer to `capabilities`) is silently accepted by the YAML parser and ignored by the framework — your SDS pod will ship **without** the extraction container, your worker's `EXTRACTION_URL` will be unset, and binary files (PDF/DOCX/XLSX/…) will emit findings with no `content` so the classifier never sees them. Symptom on a running scan: SDS pod terminates fast, entities populate fine, but `v2_document_assessments` stays empty and no `/classify` traffic shows up in the classifier logs. `aa26-connector lint` flags this misplacement; run it before uploading.

```python
# worker code (Python)
from aa26_connector_sdk import extract_text, ExtractionError

file_bytes = fetch_object(...)
try:
    result = extract_text(file_bytes,
                          content_type="application/pdf",
                          filename="report.pdf")
    finding["content"] = result["text"]
except ExtractionError as e:
    log.warning("extraction failed for %s: %s", filename, e)
    # emit the finding without `content` — V2 just won't classify this object
```

## What the sidecar can do

* **Tika-routed** (everything except `image/*`):
  * PDF (text + metadata)
  * Microsoft Office: DOCX, XLSX, PPTX, plus the older binary formats
  * OpenDocument: ODT, ODS, ODP
  * HTML, XHTML, plain text, RTF, ePub
  * Email: EML, MSG (subject + body + attachments to nested extraction)
  * 1,000+ formats supported by Apache Tika 2.x
* **Tesseract-routed** (`image/*`):
  * PNG, JPEG, TIFF, GIF, WebP — runs OCR with the language packs declared on the sidecar (default: `eng`)

What it does NOT do (yet):

* Streaming extraction (whole file is buffered in memory)
* OCR on PDFs without an embedded text layer (Phase 2)
* Custom parsers (Tika's bundle is fixed at the framework's published version)

## HTTP contract — POST /v1/extract

If you're not using one of the SDK clients, you can call the endpoint directly. Request:

```
POST http://127.0.0.1:8087/v1/extract
Content-Type: <source mime>            # required; drives tool selection
X-Filename: <hint>                     # optional; Tika uses for format detection
X-Languages: eng,spa                   # optional; OCR language hint

<raw file bytes>
```

Response:

```json
200 OK
{
  "text": "concatenation of every entry's extracted text...",
  "tool": "tika" | "tesseract",
  "entries": [
    {
      "path": "",
      "filename": "archive.zip",
      "contentType": "application/zip",
      "depth": 0,
      "text": "",
      "metadata": { /* Tika fields for the container */ }
    },
    {
      "path": "/payroll.pdf",
      "filename": "payroll.pdf",
      "contentType": "application/pdf",
      "depth": 1,
      "text": "real extracted PDF text...",
      "metadata": { "xmpTPg:NPages": "12" }
    }
  ],
  "metadata": {
    "originalContentType": "application/zip",
    "filename": "archive.zip",
    "entryCount": 2,
    "maxDepth": 2
  }
}
```

For a single PDF / DOCX, `entries` has one element (the container, depth=0) carrying the document's text. For a ZIP, it carries the container plus one element per inner file. **Iterate `entries` and skip `depth=0` to emit one finding per parsed file**; or use the top-level `text` field if you only need the whole-archive blob (e.g. for a single Evidence AI relay per archive). The Python and Go SDKs include `iter_entries()` / `IterInner()` helpers that do the container-skip for you.

### Archive handling

The sidecar uses Tika's `/tika/recursive` endpoint and unwraps these formats automatically:

* ZIP, TAR, GZ, BZ2, 7z, RAR (subject to Tika's parser bundle)
* Office formats are technically ZIPs internally — Tika handles them transparently with no recursion overhead

Recursion depth is capped at your manifest's `spec.extraction.maxDepth` (default **2** — i.e. unwrap one level of archive). Bump to 3+ if your sources commonly contain nested archives. Cap is hard-limited to 10. Caveats:

* OCR is NOT applied to images inside an archive (Tesseract isn't piped through Tika's recursion).
* Password-protected entries fail silently — you get whatever Tika could parse before hitting the encrypted block.
* Spanned/multi-part archives are not supported.

Failure modes you should handle:

| Status | Code              | Meaning                                              | Recommended action |
| ------ | ----------------- | ---------------------------------------------------- | ------------------ |
| 400    | `missing-content-type` | You forgot `Content-Type`                       | Fix the call |
| 413    | `too-large`       | Body exceeded `MAX_EXTRACT_BYTES` (default 50 MiB)  | Truncate or skip the object |
| 415    | `unsupported`     | No extractor for that MIME (e.g. `application/x-binary-blob`) | Skip extraction; emit finding without `content` |
| 500    | `tika-not-ready`  | Sidecar JVM still warming up                        | Retry after a few seconds, or skip and continue |
| 500    | `tika-failed`     | Tika parser raised on this document                 | Skip; log; consider whether the file is corrupt |
| 500    | `tesseract-failed` | OCR exec failed                                    | Skip; log |
| 504    | (transport)       | Extraction exceeded `EXTRACT_TIMEOUT_S` (default 60s) | Skip; log |

The SDK clients map all of these into typed exceptions / errors so your code can branch cleanly. See the language sections below.

## Python SDK

```python
from aa26_connector_sdk import extract_text, iter_entries, ExtractionError, ExtractionUnavailable

# Pattern 1: one finding per object (e.g. one PDF → one finding).
def emit_one_per_object(obj):
    file_bytes = fetch(obj.url)
    try:
        result = extract_text(
            file_bytes,
            content_type=obj.content_type,
            filename=obj.name,
        )
        return {
            **base_envelope(obj),
            "content": result["text"],
        }
    except ExtractionUnavailable:
        # Sidecar isn't attached — manifest didn't opt in.
        # Treat as a permanent skip for this run.
        return base_envelope(obj)
    except ExtractionError as e:
        log.warning("extraction failed: %s", e)
        return base_envelope(obj)
```

```python
# Pattern 2: one finding per archive entry — for ZIP/TAR sources you
# want enumerated. iter_entries() yields the inner items, skipping the
# container by default.
def emit_one_per_entry(obj):
    archive_bytes = fetch(obj.url)
    result = extract_text(archive_bytes, content_type="application/zip", filename=obj.name)
    findings = []
    for entry in iter_entries(result):       # skips depth=0 container
        findings.append({
            "kind": "finding",
            "type": "object_metadata",
            "content": entry["text"],
            "object": {
                "kind": "file",
                "id":   f"{obj.url}!{entry['path']}",   # unique per inner file
                "name": entry["filename"],
                "contentType": entry["contentType"],
            },
        })
    return findings
```

The package ships with this CLI repo at `sdk/python/`. Install in your image:

```dockerfile
COPY --from=framework /sdk/python /opt/aa26-sdk
RUN pip install /opt/aa26-sdk[extraction]
```

The `[extraction]` extra pulls `requests` only — the SDK's other modules don't need it.

## Bash SDK

```bash
source /opt/aa26-sdk/extraction.sh

if text=$(aa26_extract_text "$file" "$content_type" "$filename"); then
  # Compose envelope with content
  jq -n --arg t "$text" \
        --arg url "$url" \
        '{kind:"finding",type:"object_metadata",content:$t,object:{url:$url}}'
else
  # aa26_extract_error is set; emit without content
  echo "[warn] extraction: $aa26_extract_error" >&2
  jq -n --arg url "$url" \
        '{kind:"finding",type:"object_metadata",object:{url:$url}}'
fi | curl -sS -X POST http://127.0.0.1:8089/v1/findings \
       -H 'Content-Type: application/x-ndjson' --data-binary @-
```

Requires `curl` and `jq` on your image. The script ships with this CLI repo at `sdk/bash/extraction.sh`.

## Go SDK

```go
import "github.com/netwrix/connector-sdk-go/extraction"

client := extraction.NewClient() // reads EXTRACTION_URL from env

ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
defer cancel()

res, err := client.Extract(ctx, fileBytes, "application/pdf",
    extraction.WithFilename("report.pdf"))
switch {
case errors.Is(err, extraction.ErrUnavailable):
    // Sidecar not attached — proceed without content
case err != nil:
    log.Printf("extraction failed: %v", err)
default:
    finding["content"] = res.Text
}
```

The module ships with this CLI repo at `sdk/go/extraction/`. Until it's published as its own Go module, vendor the directory or add a `replace` directive in your connector's `go.mod`:

```
replace github.com/netwrix/connector-sdk-go => /path/to/aa26-connector-cli/sdk/go
```

## Local testing — `aa26-connector test`

The framework's test harness includes a **mock extraction sidecar** so you can iterate on your worker without Docker, Tika, or 700 MB of language packs. When your manifest declares `sidecars: [extraction]`:

```bash
$ aa26-connector test ./connector.yaml --op=scan
→ emulator listening on 127.0.0.1:8089
→ extraction emulator listening on 127.0.0.1:8087 (mock — Tika+OCR are not really running)
→ running my-connector:dev (op=scan, function_type=scan)
[FINDING] {"kind":"finding",...,"content":"EXTRACTED:report.pdf"}
```

The mock returns a synthetic `EXTRACTED:<filename or first 32 bytes>` string for any non-image MIME and 415 for `image/*`. It's intentionally trivial — enough to verify your code wires the call correctly, not enough to test parser fidelity. For real extraction validation, run against the dev cluster pod where Tika is actually executing.

## Resource cost

Per Pod, when you opt in:

| | Request | Limit |
| --- | --- | --- |
| CPU | 200m | 1 |
| Memory | 768 Mi | 1.5 Gi |

Plus the image itself (~350-400 MB), pulled once and cached on the node. Startup probe gives Tika 60 seconds to warm up; on subsequent Pod launches the JVM image is in the kernel cache and warmup drops to ~10 seconds.

If your scans are short and many (lots of small access-scan Pods), the 768 MiB-per-Pod reservation adds up. Don't list `extraction` on a connector that doesn't actually call `/v1/extract`.

## Reference

* [Manifest reference — `spec.capabilities.sidecars`](./manifest-reference.md#speccapabilities) — how to declare the opt-in
* [Manifest reference — `spec.extraction`](./manifest-reference.md#specextraction) — how to set `maxDepth`
* [CLI — `aa26-connector test`](./cli.md#aa26-connector-test) — local-test flow including the extraction emulator mock
* [Finding schema](./finding-schema.md) — where the extracted `text` lands on your `content`-bearing finding envelope
