# Shared settings for the SWE-bench routing benchmark. Sourced by the other
# scripts; override any of these in the environment.
#
# WORK holds the Python env, trajectories and grading output — tens of GB of
# Docker images live in Docker's own data root, not here. Keep it out of the
# repo.
BENCH_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$BENCH_DIR/../.." && pwd)
WORK=${SWE_WORK:-$HOME/.local/share/modelfabric/bench/swe}
MFSH=${MFSH:-$REPO/mfsh}
MODEL=${SWE_MODEL:-qwen/qwen3.8-27b}
# Per-request context and slots per engine. -c becomes CONTEXT x PARALLEL.
#
# 65536 x 2 is a pair, not two numbers. -c is context x parallel, so four
# slots at this context is 262144 tokens of KV and OOMs a 5090 ("allocating
# 16.0GB"); dropping to 32768 to fit four slots truncates the 30-90k
# conversations these tasks reach, which measures truncation, not routing.
#
# Speculation is asked for rather than left to "auto". ModelFabric turns drafting off
# for any model carrying an image projector, because llama.cpp could not draft
# a prompt containing an image — which is why this line used to read
# `-vision off`, trading the projector for the 2x decode MTP gives (134 tok/s
# against 66 on the 5090).
#
# That trade is gone. Measured 2026-09-24 at these exact settings with the
# projector loaded: two concurrent image requests, 986KB and 827KB, both served
# with drafting live throughout, and helion's Metal build the same. So the
# fleet keeps its speculation and can read an image while a run is on. An
# explicit -spec mtp is honoured over the automatic rule; without it, loading
# with vision would silently halve decode again.
LOAD_ARGS=${SWE_LOAD_ARGS:-"-vision on -spec mtp -context 65536 -parallel 2"}

# Per-node overrides, because the machines are not alike and a uniform slot
# count is the thing `mfsh tune` exists to disprove. Measured on this fleet,
# two slots each gave the Mac 49% of the fleet's prefill for a twelfth of the
# output: it was advertising capacity it could not honour.
#
# Set SWE_LOAD_ARGS_<NODE> with the node's name upper-cased and any dash
# turned into an underscore — SWE_LOAD_ARGS_HELION, SWE_LOAD_ARGS_SITES_01.
# Unset means that node uses LOAD_ARGS, so the common case is unchanged.
#
# Whatever is used is printed by reload.sh and recorded with the run: a fleet
# loaded three different ways is only comparable to itself if the three ways
# are written down.
#
# The values below are measured, not chosen. `mfsh tune` on each node,
# 2026-09-25, at this context with ~3000-token prompts (total tok/s, all
# requests in flight; the recommendation is the last count that bought at least
# a tenth more):
#
#   node       1     2     4     8      recommended
#   xpredator  60    77    OOM   —      2 slots
#   minion     32    42    53    OOM    4 slots
#   helion     10     9    —     —      1 slot
#
# So the flat two-per-node this benchmark used through 2026-09-24 was wrong on
# two machines of three: it starved minion of half its throughput and gave
# helion a second slot that made it slower, which is how a 24 tok/s Mac came to
# absorb 49% of the fleet's prefill. Re-run `mfsh tune` on new hardware rather
# than copying these numbers.
SWE_LOAD_ARGS_MINION=${SWE_LOAD_ARGS_MINION:-"-vision on -spec mtp -context 65536 -parallel 4"}
SWE_LOAD_ARGS_HELION=${SWE_LOAD_ARGS_HELION:-"-vision on -spec mtp -context 65536 -parallel 1"}
loadArgsFor() {
  local var="SWE_LOAD_ARGS_$(printf '%s' "$1" | tr '[:lower:]-' '[:upper:]_')"
  printf '%s' "${!var:-$LOAD_ARGS}"
}
SUBSET=${SWE_SUBSET:-verified}
SLICE=${SWE_SLICE:-0:20}
WORKERS=${SWE_WORKERS:-4}
# The second node, reached with `tailscale ssh`; empty for one node.
# Every peer that serves the model, space separated. The pilot ran two nodes
# with a single PEER; the fleet is three now, and a run that reloads only one
# of them measures a mesh half of which is serving stale caches.
PEERS=${SWE_PEERS:-${SWE_PEER:-minion helion}}
PEER_MFSH=${SWE_PEER_MFSH:-'~/.local/bin/mfsh'}
# swebench >= 5 needs this dataset name (the princeton-nlp copy lacks the
# `image` column the harness reads).
DATASET=${SWE_DATASET:-SWE-bench/SWE-bench_Verified}
# llm-d's prefill estimate (tok/s) for the affinity filter. ModelFabric re-measures
# it while llm-d runs and saves it, so the next enable would otherwise pick up
# whatever the previous run measured. Set it to pin a run; empty keeps the
# saved/measured value.
PEAK_PREFILL=${SWE_PEAK_PREFILL:-}
LLMD_DIR=${SWE_LLMD_DIR:-$HOME/.modelfabric/llmd}
# The node the agent talks to, and the node whose routing the run drives.
# Empty means this machine, which is how the two-node pilot ran.
#
# An entrypoint changes what is being measured, for the better: requests reach
# a node with no GPUs of its own, which schedules them onto the mesh. That is
# the deployment ModelFabric is for, and it exercises routing rather than local-first
# placement. It also adds a real network hop to every call, so a run through it
# is comparable to other runs through it and not to a local one.
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
