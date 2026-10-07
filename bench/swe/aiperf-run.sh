#!/bin/bash
# Replay a fixed trace through the active routing mode using NVIDIA AIPerf.
#
#   RUNNER=bench/swe/aiperf-run.sh SWE_TRACE=trace.jsonl bench/swe/sequence.sh PREFIX router tuned litellm
#
# SWE_TRACE selects the required trace; SWE_AIPERF overrides the executable
# (default $WORK/aiperf-venv/bin/aiperf); SWE_WORKERS sets concurrent conversations.
set -euo pipefail
source "$(dirname "$0")/env.sh"
NAME=${1:?usage: aiperf-run.sh NAME}
TRACE=${SWE_TRACE:?set SWE_TRACE to a trace written by aiperf-trace.py}
AIPERF=${SWE_AIPERF:-$WORK/aiperf-venv/bin/aiperf}
[ -x "$AIPERF" ] || { echo "aiperf not found at $AIPERF; install it with: python3 -m venv $WORK/aiperf-venv && $WORK/aiperf-venv/bin/pip install aiperf" >&2; exit 1; }
[ -s "$TRACE" ] || { echo "trace $TRACE is missing or empty" >&2; exit 1; }

# The key belongs to whichever node the requests enter at; see run.sh.
AGENT_KEY_NODE=${SWE_AGENT_KEY_NODE:-$ENTRYPOINT}
if [ -n "${SWE_AGENT_KEY:-}" ]; then
  KEY=$SWE_AGENT_KEY
elif [ -n "$AGENT_KEY_NODE" ]; then
  KEY=$(tailscale ssh "$AGENT_KEY_NODE" '~/.local/bin/mfsh key' </dev/null | tr -d '\r\n')
  [ -n "$KEY" ] || { echo "could not read $AGENT_KEY_NODE's API key over ssh" >&2; exit 1; }
else
  KEY=$("$MFSH" key)
fi

# AIPerf appends /v1/chat/completions itself.
URL=${AGENT_BASE%/}
URL=${URL%/v1}
CALLS=$(wc -l < "$TRACE")
mkdir -p "$WORK/runs/$NAME"
cp "$TRACE" "$WORK/runs/$NAME/trace.jsonl"
# Set request-count to avoid AIPerf's default request limit. Use concurrency
# with no-fixed-schedule so each completed conversation admits the next,
# matching the agent harness rather than recorded wall-clock start times.
"$AIPERF" profile \
  --model "$MODEL" --endpoint-type chat --url "$URL" --api-key "$KEY" \
  --input-file "$TRACE" --custom-dataset-type mooncake-trace \
  --no-fixed-schedule --concurrency "$WORKERS" --request-count "$CALLS" \
  --request-timeout-seconds 1800 --use-server-token-count \
  --ui none --no-server-metrics --export-level raw \
  --artifact-dir "$WORK/runs/$NAME"
