#!/bin/bash
# Run the benchmark through the configured routing into $WORK/runs/NAME.
# Set the routing mode first. Usage: run.sh NAME
set -euo pipefail
source "$(dirname "$0")/env.sh"
NAME=${1:?usage: run.sh NAME}
cd "$WORK"
# Use the ModelFabric entrypoint so request placement is recorded. Direct
# engine requests bypass the router log. Fetch the entrypoint's key over SSH;
# a local key cannot authenticate another node and keys are not API-accessible.
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
# Send sampling parameters only when explicitly pinned in the environment.
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
