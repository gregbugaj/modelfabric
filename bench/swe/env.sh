# Shared SWE-bench settings; override them in the environment.
# WORK holds environments, trajectories and reports outside the repository.
# Docker images remain in Docker's data root.
BENCH_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$BENCH_DIR/../.." && pwd)
WORK=${SWE_WORK:-$HOME/.local/share/modelfabric/bench/swe}
MFSH=${MFSH:-$REPO/mfsh}
MODEL=${SWE_MODEL:-qwen/qwen3.8-27b}
# Per-request context and slots multiply to the engine KV allocation.
# More slots can exceed GPU memory; reducing context can truncate benchmark tasks.
# Explicit -spec mtp overrides automatic disabling for vision models.
# Host-RAM prompt caching reduces rereads when conversations exceed slot count;
# size it within available RAM alongside weights, KV and context checkpoints.
LOAD_ARGS=${SWE_LOAD_ARGS:-"-vision on -spec mtp -context 65536 -parallel 2 -cache-ram 32768"}

# Per-node overrides use SWE_LOAD_ARGS_<NODE>, with uppercase names and
# underscores for dashes. Unset nodes use LOAD_ARGS; each run records its values.
# Tune slot counts for each node rather than assuming uniform capacity.
# The minion override caps host-RAM prompt caching at 6144 MiB: 8192 MiB
# left about 1.1 GB available and triggered unloading. Increasing it requires
# more RAM; weights and context checkpoints also consume host memory.
SWE_LOAD_ARGS_MINION=${SWE_LOAD_ARGS_MINION:-"-vision on -spec mtp -context 88000 -parallel 4 -cache-ram 6144"}
# On the 48 GB unified-memory node, a 16384 MiB cache leaves about 8 GB
# for the OS after the measured 24.6 GB loaded engine allocation.
SWE_LOAD_ARGS_HELION=${SWE_LOAD_ARGS_HELION:-"-vision on -spec mtp -context 65536 -parallel 1 -cache-ram 16384"}
loadArgsFor() {
  local var="SWE_LOAD_ARGS_$(printf '%s' "$1" | tr '[:lower:]-' '[:upper:]_')"
  printf '%s' "${!var:-$LOAD_ARGS}"
}
SUBSET=${SWE_SUBSET:-verified}
SLICE=${SWE_SLICE:-0:20}
WORKERS=${SWE_WORKERS:-4}
# Space-separated peers serving the model; reload every peer to clear prior-run caches. Empty selects only the local node.
PEERS=${SWE_PEERS:-${SWE_PEER:-minion helion}}
PEER_MFSH=${SWE_PEER_MFSH:-'~/.local/bin/mfsh'}
# swebench >= 5 needs this dataset name (the princeton-nlp copy lacks the
# `image` column the harness reads).
DATASET=${SWE_DATASET:-SWE-bench/SWE-bench_Verified}
# Pin llm-d's prefill estimate in tok/s to avoid inheriting prior-run calibration;
# empty retains the saved or measured value.
# Temperature and seed reduce sampling variation; empty uses engine defaults.
# Different hardware and builds can still produce different answers to the same prompt.
TEMPERATURE=${SWE_TEMPERATURE:-}
SEED=${SWE_SEED:-}
PEAK_PREFILL=${SWE_PEAK_PREFILL:-}
LLMD_DIR=${SWE_LLMD_DIR:-$HOME/.modelfabric/llmd}
# Agent entrypoint and routing control node; empty selects this machine.
# Remote entrypoints add a network hop and should be compared with other runs
# using the same path.
ENTRYPOINT=${SWE_ENTRYPOINT:-}
AGENT_BASE=${SWE_AGENT_BASE:-http://127.0.0.1:1234/v1}
# How to reach the entrypoint's own ModelFabric for llm-d control. Its key is read
# over ssh, because a node's key lives on its own disk and is deliberately not
# served over the network.
CONTROL_ADDR=${SWE_CONTROL_ADDR:-}
# ModelFabric's room filter in front of the llm-d profile (never schedule onto an
# engine with no free slot while another has room). false gives llm-d's own
# profile, as in the 2026-09-19 runs.
ROOM_FILTER=${SWE_ROOM_FILTER:-true}
