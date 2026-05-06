#!/usr/bin/env bash
# demo-shell connector — entire connector in bash + curl + jq.
# Talks to the runtime sidecar over HTTP. No SDK required.
set -euo pipefail

SIDECAR="${SIDECAR_URL:-http://127.0.0.1:8089}"

# Tiny helpers: turn args into the four envelope fields the sidecar wants.
now()      { date -u +"%Y-%m-%dT%H:%M:%SZ"; }
post()     { curl -sf -X POST -H 'Content-Type: application/json' -d "$2" "$SIDECAR$1" >/dev/null; }
post_ndj() { curl -sf -X POST -H 'Content-Type: application/x-ndjson' --data-binary @- "$SIDECAR$1" >/dev/null; }

# 1. Read invocation.
inv="$(curl -sf "$SIDECAR/v1/invocation")"
op="$(jq -r '.operation' <<<"$inv")"
exec_id="$(jq -r '.executionId' <<<"$inv")"
src_id="$(jq -r '.sourceId // ""' <<<"$inv")"
root="$(jq -r '.source.rootPath' <<<"$inv")"

echo "demo-shell: op=$op root=$root" >&2

# 2. Dispatch.
case "$op" in
  test_connection)
    if [[ -d "$root" ]]; then
      post /v1/complete "$(jq -nc --arg msg "ok" '{status:"completed",summary:{reachable:true,message:$msg}}')"
    else
      post /v1/complete "$(jq -nc --arg root "$root" '{status:"failed",summary:{reason:("rootPath missing: "+$root)}}')"
      exit 1
    fi
    ;;

  scan)
    [[ -d "$root" ]] || { post /v1/complete '{"status":"failed","summary":{"reason":"rootPath not a directory"}}'; exit 1; }

    count=0
    # Stream findings as NDJSON in batches of 50. find -print0 handles
    # paths with newlines/spaces; jq -Rsc reads the whole stdin string.
    find "$root" -type f -print0 | while IFS= read -r -d '' f; do
      sz="$(stat -c %s "$f" 2>/dev/null || echo 0)"
      jq -nc \
        --arg eid "$exec_id" --arg sid "$src_id" --arg ts "$(now)" \
        --arg p "$f" --argjson sz "$sz" '{
          schemaVersion:"1.0", kind:"finding",
          executionId:$eid, sourceId:$sid, occurredAt:$ts,
          type:"object_metadata",
          object:{kind:"file", id:$p, path:$p, size:$sz}
        }'
      count=$((count+1))
      if (( count % 50 == 0 )); then
        # Flush this batch by sending an EOF to a fresh post_ndj.
        # Easier: just emit, sidecar buffers the request body itself.
        :
      fi
    done | post_ndj /v1/findings

    # Final progress + complete.
    post /v1/progress "$(jq -nc --arg eid "$exec_id" --arg ts "$(now)" '{schemaVersion:"1.0",kind:"progress",executionId:$eid,occurredAt:$ts,processed:0,message:"done"}')"
    post /v1/complete '{"status":"completed","summary":{"language":"bash"}}'
    ;;

  *)
    post /v1/complete "$(jq -nc --arg op "$op" '{status:"failed",summary:{reason:("unknown op: "+$op)}}')"
    exit 2
    ;;
esac
