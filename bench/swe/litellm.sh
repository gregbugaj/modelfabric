#!/bin/bash
# LiteLLM baseline using least-busy placement across the same engine processes.
#
#   litellm.sh start RUNDIR   print BASE and KEY for the proxy
#   litellm.sh stop [RUNDIR]  record memory and OOM status, then remove the proxy
#
# Dial engine ports directly; launch settings apply unchanged. Agent requests
# supply max_tokens, so ModelFabric's missing-value default is irrelevant.
# Disable retries and cooldowns so errors remain visible. Match the agent timeout.
# Run on the entrypoint when configured to preserve network hops. Listen on its
# tailnet address with a per-run key, then remove the proxy after the run.
# Backend engine ports have no authentication.
set -euo pipefail
source "$(dirname "$0")/env.sh"

IMAGE=${SWE_LITELLM_IMAGE:-ghcr.io/berriai/litellm:v1.83.14-stable}
PORT=${SWE_LITELLM_PORT:-4000}
NAME=mfsh-bench-litellm
# Cap proxy memory to protect the entrypoint. LiteLLM v1.83 used 923 MiB idle, so a 1 GiB cap was insufficient.
MEMORY=${SWE_LITELLM_MEMORY:-1536m}

# on runs a command where LiteLLM lives: the entrypoint, or this machine.
on() {
  if [ -n "$ENTRYPOINT" ]; then
    timeout 600 tailscale ssh "$ENTRYPOINT" "$1"
  else
    bash -c "$1"
  fi
}

# Remove the stopped proxy's config because it contains the run key.
stop() {
  local rundir=${1:-}
  # Record OOM failures so resource exhaustion is not attributed to routing.
  if [ -n "$rundir" ]; then
    mkdir -p "$rundir"
    on "docker inspect -f 'oom_killed={{.State.OOMKilled}} exit={{.State.ExitCode}} restarts={{.RestartCount}}' $NAME 2>/dev/null; \
      docker exec $NAME sh -c 'echo peak_mib=\$((\$(cat /sys/fs/cgroup/memory.peak) / 1048576))' 2>/dev/null" \
      > "$rundir/litellm.health.txt" || true
  fi
  on "docker rm -f $NAME >/dev/null 2>&1 || true; rm -rf ~/mfsh-bench-litellm"
}

start() {
  local rundir=${1:?usage: litellm.sh start RUNDIR}
  mkdir -p "$rundir"
  stop

  # The engines serving the model, read from the mesh after the reload, with
  # their engine ports. MLX engines are refused: they need ModelFabric to
  # rewrite the model id, so LiteLLM cannot reach them as they are.
  local mesh bind engines
  mesh=$(curl -fsS --max-time 10 "${CONTROL_ADDR:-http://127.0.0.1:1234}/z/mesh")
  read -r bind engines < <(MESH="$mesh" python3 - "$MODEL" "$ENTRYPOINT" <<'PY'
import json, os, sys
model, entry = sys.argv[1], sys.argv[2]
m = json.loads(os.environ["MESH"])
nodes = [m.get("self") or {}] + (m.get("peers") or [])
bind, out = "127.0.0.1", []
for n in nodes:
    if entry and n.get("node") == entry:
        bind = (n.get("addr") or "").split(":")[0] or bind
    for i in n.get("instances") or []:
        if i.get("model") != model or i.get("state") != "ready":
            continue
        if (i.get("engine") or "llama.cpp") != "llama.cpp":
            sys.exit(f"{n.get('node')} serves {model} on {i.get('engine')}, which LiteLLM cannot reach without ModelFabric")
        out.append(f"{n.get('node')}={i['address']}:{i['port']}")
if not out:
    sys.exit(f"no ready engine serves {model}")
print(bind, " ".join(out))
PY
  )
  [ -n "$engines" ] || { echo "litellm: no engines found" >&2; exit 1; }

  local key
  key="sk-bench-$(head -c 18 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  local config
  config=$(python3 - "$MODEL" "$key" $engines <<'PY'
import sys
model, key, engines = sys.argv[1], sys.argv[2], sys.argv[3:]
lines = ["model_list:"]
for e in engines:
    node, addr = e.split("=", 1)
    lines += [
        f"  - model_name: \"{model}\"",
        "    litellm_params:",
        f"      model: \"openai/{model}\"",
        f"      api_base: \"http://{addr}/v1\"",
        "      api_key: \"none\"",
        "      timeout: 1800",
        "      stream_timeout: 1800",
        "    model_info:",
        f"      node: \"{node}\"",
    ]
lines += [
    "router_settings:",
    "  routing_strategy: least-busy",
    "  num_retries: 0",
    "  disable_cooldowns: true",
    "litellm_settings:",
    "  request_timeout: 1800",
    "  drop_params: false",
    "general_settings:",
    f"  master_key: \"{key}\"",
]
print("\n".join(lines))
PY
  )
  # The run keeps the config without its key, so a reader can see exactly
  # what the baseline was.
  printf '%s\n' "$config" | grep -v master_key > "$rundir/litellm.config.yaml"

  echo "  litellm: $IMAGE on ${bind}:$PORT -> $engines" >&2
  on "docker pull -q $IMAGE >/dev/null"
  # The config arrives on stdin and is written owner-only: it carries the key.
  printf '%s\n' "$config" | on "umask 077; mkdir -p ~/mfsh-bench-litellm && cat > ~/mfsh-bench-litellm/config.yaml && \
    docker run -d --name $NAME --memory $MEMORY -p ${bind}:$PORT:4000 \
      -v \$HOME/mfsh-bench-litellm/config.yaml:/app/config.yaml:ro \
      $IMAGE --config /app/config.yaml --port 4000 >/dev/null"

  local i=0
  until curl -fs --max-time 3 -o /dev/null "http://${bind}:$PORT/health/liveliness"; do
    i=$((i + 1))
    if [ $i -ge 90 ]; then
      echo "litellm did not come up in 180s; its log:" >&2
      on "docker logs --tail 30 $NAME" >&2 || true
      stop
      exit 1
    fi
    sleep 2
  done
  # The exact image that ran, not the tag, which can be moved.
  on "docker inspect --format '{{index .RepoDigests 0}}' $IMAGE" > "$rundir/litellm.image.txt"
  echo "BASE=http://${bind}:$PORT/v1"
  echo "KEY=$key"
}

case "${1:-}" in
  start) shift; start "$@" ;;
  stop) shift; stop "$@" ;;
  *) echo "usage: litellm.sh start RUNDIR | stop" >&2; exit 2 ;;
esac
