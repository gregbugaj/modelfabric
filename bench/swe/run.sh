#!/bin/bash
# One benchmark run through ModelFabric's front door, into $WORK/runs/NAME.
# Whatever routing is in force (ModelFabric's own router, or llm-d scheduling this
# model) is what gets measured — set it first (see README). Usage: run.sh NAME
set -euo pipefail
source "$(dirname "$0")/env.sh"
NAME=${1:?usage: run.sh NAME}
cd "$WORK"
# ModelFabric's front door, never an engine's own port. ModelFabric records what it carried
# — trace id, model, status, latency, and the node and engine that served it —
# and that per-request placement data is what this benchmark exists to produce.
# Pointed at anything downstream the agent bypasses ModelFabric, and a run of three
# engines over hours produced not one line in `mfsh log`: the requests were
# never ModelFabric's to see.
# The key belongs to whichever node the agent talks to, not to this one. With
# an entrypoint the two differ, and the local key authenticates against the
# wrong node — a 401 on every call, after the model had loaded and the run
# looked ready to go. A key is read from its own node's disk and is deliberately
# not served over the network, so it comes over ssh.
AGENT_KEY_NODE=${SWE_AGENT_KEY_NODE:-$ENTRYPOINT}

# The model config carries the node's API key, so it is written at run time,
# owner-only, and never committed.
umask 077
if [ -n "${SWE_AGENT_KEY:-}" ]; then
  # The litellm arm: the key litellm.sh generated for this run's proxy.
  KEY=$SWE_AGENT_KEY
elif [ -n "$AGENT_KEY_NODE" ]; then
  KEY=$(tailscale ssh "$AGENT_KEY_NODE" '~/.local/bin/mfsh key' </dev/null | tr -d '\r\n')
  [ -n "$KEY" ] || { echo "could not read $AGENT_KEY_NODE's API key over ssh" >&2; exit 1; }
else
  KEY=$("$MFSH" key)
fi
# Sampling, when pinned (see SWE_TEMPERATURE in env.sh). Left out entirely
# otherwise, so an unpinned run sends exactly what it always sent.
SAMPLING=""
[ -n "$TEMPERATURE" ] && SAMPLING="$SAMPLING
    temperature: $TEMPERATURE"
[ -n "$SEED" ] && SAMPLING="$SAMPLING
    seed: $SEED"
cat > model.yaml <<YAML
model:
  model_name: "openai/$MODEL"
  cost_tracking: "ignore_errors"
  model_kwargs:
    api_base: "$AGENT_BASE"
    api_key: "$KEY"
    max_tokens: 16384
    timeout: 1800
    drop_params: true
    parallel_tool_calls: true$SAMPLING
agent:
  step_limit: 100
  cost_limit: 0
YAML
umask 022
mkdir -p mswea-config runs
export MSWEA_GLOBAL_CONFIG_DIR=$WORK/mswea-config MSWEA_COST_TRACKING=ignore_errors
# Only containers created after this point belong to this run.
STARTED_AT=$(date '+%Y-%m-%d %H:%M:%S')
.venv/bin/mini-extra swebench --subset "$SUBSET" --split test --slice "$SLICE" --workers "$WORKERS" \
  -c swebench.yaml -c model.yaml -o "runs/$NAME"
# mini-swe-agent's sandboxes are `sleep 2h` containers; clear any this run left
# behind. Matching every minisweagent-* container on the host would also kill a
# concurrent benchmark's sandboxes, or another user's.
docker ps --format '{{.ID}} {{.Names}} {{.CreatedAt}}' \
  | awk -v since="$STARTED_AT" '$2 ~ /^minisweagent-/ { id=$1; $1=""; $2=""; if ($0 >= since) print id }' \
  | xargs -r docker rm -f >/dev/null
