# aa26 connector SDK — bash. Source this file to call the extraction
# sidecar from a shell-based connector.
#
# Usage:
#     source /opt/aa26-sdk/extraction.sh
#     text=$(aa26_extract_text /path/to/file.pdf application/pdf) || {
#       echo "extraction failed: $aa26_extract_error" >&2
#       # connector emits the finding without `content` and continues
#     }
#
# Three functions:
#   aa26_extract_text   <file>  <content-type>  [filename]  [languages]
#       echoes extracted text on stdout (the concatenation of every parsed
#       entry's text — for archives this is every inner doc joined). Sets
#       aa26_extract_error and returns non-zero on failure.
#   aa26_extract_json   <file>  <content-type>  [filename]  [languages]
#       echoes the full sidecar JSON response (same as text, plus
#       entries[] and metadata). Use this when you want per-entry
#       findings from a ZIP — pipe into `jq '.entries[] | select(.depth>0)'`.
#   aa26_extraction_ready
#       returns 0 iff /readyz reports ready, 1 otherwise.
#
# Requires `curl` and `jq` on PATH.

aa26_extract_error=""

aa26_extract_text() {
  aa26_extract_error=""
  local file="$1" content_type="$2" filename="${3:-}" languages="${4:-}"
  local base="${EXTRACTION_URL:-}"
  if [ -z "$base" ]; then
    aa26_extract_error="EXTRACTION_URL is not set; declare spec.capabilities.sidecars: [extraction] in connector.yaml"
    return 2
  fi
  if [ ! -r "$file" ]; then
    aa26_extract_error="cannot read file: $file"
    return 2
  fi
  local hdr_args=(-H "Content-Type: $content_type")
  [ -n "$filename" ]  && hdr_args+=(-H "X-Filename: $filename")
  [ -n "$languages" ] && hdr_args+=(-H "X-Languages: $languages")
  local body
  if ! body=$(curl -sS --max-time 60 -X POST "${base%/}/v1/extract" \
                "${hdr_args[@]}" --data-binary "@$file" 2>&1); then
    aa26_extract_error="transport error: $body"
    return 1
  fi
  # Detect 4xx/5xx — the sidecar's error envelope has top-level "error".
  if jq -e '.error' >/dev/null 2>&1 <<<"$body"; then
    aa26_extract_error=$(jq -r '.error + (if .code then " (code=" + .code + ")" else "" end)' <<<"$body")
    return 1
  fi
  jq -r '.text' <<<"$body"
}

aa26_extract_json() {
  aa26_extract_error=""
  local file="$1" content_type="$2" filename="${3:-}" languages="${4:-}"
  local base="${EXTRACTION_URL:-}"
  if [ -z "$base" ]; then
    aa26_extract_error="EXTRACTION_URL is not set; declare spec.capabilities.sidecars: [extraction] in connector.yaml"
    return 2
  fi
  if [ ! -r "$file" ]; then
    aa26_extract_error="cannot read file: $file"
    return 2
  fi
  local hdr_args=(-H "Content-Type: $content_type")
  [ -n "$filename" ]  && hdr_args+=(-H "X-Filename: $filename")
  [ -n "$languages" ] && hdr_args+=(-H "X-Languages: $languages")
  local body
  if ! body=$(curl -sS --max-time 60 -X POST "${base%/}/v1/extract" \
                "${hdr_args[@]}" --data-binary "@$file" 2>&1); then
    aa26_extract_error="transport error: $body"
    return 1
  fi
  if jq -e '.error' >/dev/null 2>&1 <<<"$body"; then
    aa26_extract_error=$(jq -r '.error + (if .code then " (code=" + .code + ")" else "" end)' <<<"$body")
    return 1
  fi
  printf '%s' "$body"
}

aa26_extraction_ready() {
  local base="${EXTRACTION_URL:-}"
  [ -z "$base" ] && return 1
  curl -sS -o /dev/null -w '%{http_code}' --max-time 2 "${base%/}/readyz" 2>/dev/null \
    | grep -q '^200$'
}
