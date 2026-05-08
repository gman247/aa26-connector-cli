# aa26 connector SDK

Thin client libraries for talking to the framework's in-Pod sidecars from a connector's worker code. Three implementations — pick the one that matches your connector's language. They all wrap the same set of HTTP calls.

| Directory | Language | What it covers | When to use |
| --- | --- | --- | --- |
| `python/` | Python ≥ 3.8 | Extraction sidecar (`extract_text`, `iter_entries`, `extraction_ready`) | Most common — Python is the default scaffold for `aa26-connector new` |
| `bash/` | bash + curl + jq | Extraction sidecar (`aa26_extract_text`, `aa26_extract_json`, `aa26_extraction_ready`) | Shell-based connectors, quick prototypes |
| `go/` | Go ≥ 1.22 | Extraction sidecar (`extraction.NewClient`, `Client.Extract`, `Result.IterInner`) | Go-based connectors |

What the SDKs explicitly do **not** wrap: the runtime sidecar's `/v1/findings` endpoint. The author-side contract for that is the NDJSON envelope shape itself, which authors compose directly. See [docs/finding-schema.md](../docs/finding-schema.md) and [docs/runtime-contract.md](../docs/runtime-contract.md).

## Python — install in your image

```dockerfile
COPY --from=aa26-cli /sdk/python /opt/aa26-sdk
RUN pip install /opt/aa26-sdk[extraction]
```

(Substituting `aa26-cli` for whatever Docker context contains the SDK — typically the cloned `aa26-connector-cli` repo mounted in your build.)

The `[extraction]` extra pulls only `requests` — no transitive deps for connectors that don't use the extraction sidecar.

```python
from aa26_connector_sdk import extract_text, iter_entries, ExtractionError

try:
    result = extract_text(file_bytes,
                          content_type="application/pdf",
                          filename="report.pdf")
    finding["content"] = result["text"]
except ExtractionError as e:
    log.warning("extraction failed: %s", e)
```

## Bash

```bash
COPY ../sdk/bash/extraction.sh /opt/aa26-sdk/extraction.sh
RUN apk add --no-cache curl jq

# In your worker entrypoint:
source /opt/aa26-sdk/extraction.sh
text=$(aa26_extract_text "$file" "$content_type" "$filename") || {
  echo "extraction failed: $aa26_extract_error" >&2
}
```

## Go

```go
import "github.com/netwrix/connector-sdk-go/extraction"

c := extraction.NewClient()  // reads EXTRACTION_URL from env
res, err := c.Extract(ctx, fileBytes, "application/pdf",
    extraction.WithFilename("report.pdf"))
```

The Go module's import path is `github.com/netwrix/connector-sdk-go/extraction`. Until that's published as its own repo, vendor the directory or use a `replace` directive in your connector's `go.mod`:

```
replace github.com/netwrix/connector-sdk-go => /path/to/aa26-connector-cli/sdk/go
```

## Defaults vs the framework

These default values live in the SDK and match the framework's defaults, so a worker that uses the SDK without overrides will behave correctly out of the box:

| Setting | SDK default | Sidecar default | Notes |
| --- | --- | --- | --- |
| Request timeout | 300 s | 300 s (`EXTRACT_TIMEOUT_S`) | Tika+OCR on a ~30 MB archive can take 1-3 min — 60 s is too low for real archive workloads. |
| Max content bytes (server side) | n/a | 50 MiB (`MAX_EXTRACT_BYTES`) | If you POST a body larger than this, you'll get 413 `too-large`. |
| OCR languages (server side) | n/a | `eng` (`EXTRACTION_LANGUAGES`) | Set via the framework's adapter — connector authors don't control it directly. |

If your connector overrides any of these, document why; the framework defaults are what V2 / Evidence AI is calibrated against.

## See also

* [docs/extraction.md](../docs/extraction.md) — full integration guide for connectors using the extraction sidecar.
* [docs/manifest-reference.md](../docs/manifest-reference.md#speccapabilities) — `spec.capabilities.sidecars` opt-in.
* [docs/manifest-reference.md](../docs/manifest-reference.md#specextraction) — `spec.extraction.maxDepth`.
