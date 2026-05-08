"""
Client wrapper for the connector framework's extraction sidecar.

The extraction sidecar (Tika + Tesseract) is attached to the connector
Pod when the manifest declares `spec.capabilities.sidecars: [extraction]`.
It exposes a single endpoint, `POST /v1/extract`, which takes raw file
bytes and returns extracted text.

Typical usage in a connector worker:

    from aa26_connector_sdk import extract_text, ExtractionError

    file_bytes = fetch_object_from_source(...)
    try:
        result = extract_text(file_bytes,
                              content_type="application/pdf",
                              filename="report.pdf")
        finding["content"] = result["text"]
    except ExtractionError as e:
        # Sidecar unreachable, oversized payload, unsupported MIME, etc.
        # Connector should still emit the finding without `content` so
        # the metadata still reaches data-ingestion — V2 just won't
        # classify this object.
        log.warning("extraction failed: %s", e)

The SDK reads `EXTRACTION_URL` from the env. The framework sets that to
`http://127.0.0.1:8087` automatically when the sidecar is attached. If
the env is unset, calls raise `ExtractionUnavailable` immediately so
the connector can branch on the absence cleanly.
"""

from __future__ import annotations

import os
from typing import Any, Dict, Optional

try:
    import requests  # type: ignore
except ImportError as _exc:  # pragma: no cover
    # The SDK depends on `requests` — connector authors using Python
    # already pull this transitively for almost any HTTP work, so we
    # don't ship a urllib fallback. If it's missing, surface the import
    # error with a clear remediation message.
    raise ImportError(
        "aa26_connector_sdk.extraction requires `requests`. "
        "Add `requests` to your connector image's pip requirements."
    ) from _exc


class ExtractionError(RuntimeError):
    """Sidecar returned a non-2xx response, or a transport error occurred.

    The string form carries the sidecar's `error` and `code` fields when
    available so log lines stay self-contained.
    """


class ExtractionUnavailable(ExtractionError):
    """Raised when EXTRACTION_URL is unset.

    This means the manifest didn't opt into the extraction sidecar (or
    the framework hasn't injected the env yet). Connectors that handle
    extraction themselves can branch on this exception explicitly.
    """


def _base_url() -> str:
    url = os.environ.get("EXTRACTION_URL", "").strip()
    if not url:
        raise ExtractionUnavailable(
            "EXTRACTION_URL is not set. Declare "
            "`spec.capabilities.sidecars: [extraction]` in your "
            "connector.yaml to attach the extraction sidecar."
        )
    return url.rstrip("/")


def extract_text(
    data: bytes,
    content_type: str,
    filename: Optional[str] = None,
    languages: Optional[str] = None,
    timeout: float = 300.0,
) -> Dict[str, Any]:
    """Extract text from `data` via the extraction sidecar.

    Parameters:
        data: raw file bytes.
        content_type: source MIME type (e.g. "application/pdf",
            "application/zip", "image/png").
            Drives sidecar tool selection (Tika vs Tesseract).
        filename: optional filename hint. Tika uses this for format
            detection when the MIME is generic ("application/octet-stream").
        languages: optional comma-separated tesseract language codes
            (e.g. "eng,spa"). Defaults to the sidecar's
            EXTRACTION_LANGUAGES env (typically "eng").
        timeout: request timeout in seconds. Default 300s matches the
            sidecar's default EXTRACT_TIMEOUT_S. Lower this only if you
            want clients to give up sooner than the sidecar would —
            raising it above 300s requires bumping EXTRACT_TIMEOUT_S on
            the sidecar too (via the helm chart / env). Tika+OCR on a
            ~30 MB archive can take 1-3 minutes, so 60s is too low for
            real archive workloads.

    Returns:
        Dict with keys:
          * `text` (str) — concatenation of every parsed entry's text.
            For a single PDF this is just the PDF's text; for a ZIP it's
            every inner document's text joined with double newlines.
          * `tool` (str) — "tika" | "tesseract".
          * `entries` (list) — one element per parsed item including the
            outer container. Each entry has `path`, `filename`,
            `contentType`, `depth` (0 = container, 1+ = nested), `text`,
            and `metadata`. Connectors that want per-file findings
            iterate this list rather than splitting `text`.
          * `metadata` (dict) — `originalContentType`, `filename`,
            `entryCount`, `maxDepth` (the sidecar's effective recursion
            limit, set per-connector via spec.extraction.maxDepth in
            connector.yaml).

    Archive handling: the sidecar uses Tika's recursive endpoint and
    unwraps ZIP/TAR/GZIP automatically. Depth is capped at the manifest's
    spec.extraction.maxDepth (default 2 — one level of unwrap).

    Raises:
        ExtractionUnavailable: EXTRACTION_URL is unset (sidecar not attached).
        ExtractionError: sidecar returned non-2xx, network error, or
            response wasn't valid JSON.
    """
    url = _base_url() + "/v1/extract"
    headers = {"Content-Type": content_type}
    if filename:
        headers["X-Filename"] = filename
    if languages:
        headers["X-Languages"] = languages
    try:
        r = requests.post(url, data=data, headers=headers, timeout=timeout)
    except requests.RequestException as exc:
        raise ExtractionError(f"extraction sidecar transport error: {exc}") from exc
    if r.status_code >= 400:
        try:
            payload = r.json()
            msg = payload.get("error") or r.text[:300]
            code = payload.get("code")
        except ValueError:
            msg = r.text[:300]
            code = None
        suffix = f" (code={code})" if code else ""
        raise ExtractionError(f"{r.status_code}: {msg}{suffix}")
    try:
        return r.json()
    except ValueError as exc:
        raise ExtractionError(f"sidecar returned non-JSON response: {r.text[:300]}") from exc


def iter_entries(result: Dict[str, Any], skip_container: bool = True):
    """Yield each entry in an extract_text() result.

    By default skips the outer container (depth=0) and yields only the
    parsed inner documents. Useful for emitting per-file findings from
    an archive:

        result = extract_text(zip_bytes, "application/zip", filename="archive.zip")
        for entry in iter_entries(result):
            emit_finding(content=entry["text"],
                         filename=entry["filename"],
                         path=entry["path"])

    Set skip_container=False to also see the container metadata (useful
    for non-archive inputs where the only entry IS the container).
    """
    for ent in result.get("entries") or []:
        if skip_container and ent.get("depth", 0) == 0:
            continue
        yield ent


def extraction_ready(timeout: float = 2.0) -> bool:
    """Return True iff the sidecar's /readyz reports ready.

    Useful for connector startup probes that want to wait on Tika
    warmup before issuing the first scan request. Returns False on
    any error (unset URL, sidecar absent, network error) — never raises.
    """
    try:
        url = _base_url() + "/readyz"
    except ExtractionUnavailable:
        return False
    try:
        r = requests.get(url, timeout=timeout)
        return r.status_code == 200
    except requests.RequestException:
        return False
