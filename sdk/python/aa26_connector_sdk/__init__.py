"""
aa26_connector_sdk — thin client library for connector authors.

The framework runs each connector container in a Pod alongside a
connector-runtime sidecar (always) and an optional extraction sidecar
(when the manifest declares `spec.capabilities.sidecars: [extraction]`).

Connector code talks to the sidecars over localhost; the SDK wraps the
HTTP calls so authors don't have to reimplement them. There is no
authentication: the Pod boundary IS the trust boundary.

Modules:

* `extraction` — `extract_text(bytes, content_type)` — calls the
  extraction sidecar's POST /v1/extract endpoint.

The runtime sidecar (POST /v1/findings) is intentionally NOT wrapped:
the connector-author contract for that endpoint is the NDJSON envelope
shape itself, which authors compose directly. See
docs/runtime-contract.md and docs/finding-schema.md.
"""

from .extraction import (
    extract_text,
    iter_entries,
    extraction_ready,
    ExtractionError,
    ExtractionUnavailable,
)

__all__ = [
    "extract_text",
    "iter_entries",
    "extraction_ready",
    "ExtractionError",
    "ExtractionUnavailable",
]

__version__ = "0.1.0"
