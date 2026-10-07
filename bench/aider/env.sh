#!/bin/bash
# Settings for the Aider polyglot routing benchmark. Override with AIDER_*.
# Short, mostly cold exercises measure placement under concurrent load.

BENCH_DIR_AIDER=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BENCH_DIR=$BENCH_DIR_AIDER
REPO=$(cd "$BENCH_DIR_AIDER/../.." && pwd)

# Reuse bench/swe fleet settings to keep model, load arguments and peers consistent.
source "$REPO/bench/swe/env.sh"
# Keep AIDER_DIR separate: sourcing bench/swe/env.sh resets BENCH_DIR for shared scripts.
AIDER_DIR=$BENCH_DIR_AIDER

WORK=${AIDER_WORK:-$HOME/.local/share/modelfabric/bench/aider}
# Aider's harness and the exercises live in separate repositories; the
# benchmark script expects the exercises under tmp.benchmarks/.
AIDER_REPO=${AIDER_REPO_URL:-https://github.com/Aider-AI/aider.git}
POLYGLOT_REPO=${AIDER_POLYGLOT_URL:-https://github.com/Aider-AI/polyglot-benchmark.git}
# Select 24 of 225 exercises, four per language, to limit runtime while exercising request placement.
NUM_TESTS=${AIDER_NUM_TESTS:-24}
# Keep threads greater than fleet slots to exercise queueing, room filtering and head-of-line blocking.
THREADS=${AIDER_THREADS:-8}
EDIT_FORMAT=${AIDER_EDIT_FORMAT:-whole}
# API timeout in seconds, set explicitly to avoid client-library defaults.
# Allow queueing across more threads than slots. ModelFabric's request_stall
# bound releases engine slots if a timed-out client stops reading without closing.
TIMEOUT=${AIDER_TIMEOUT_S:-900}
# Use a seeded subset: aider's unseeded shuffle would give each routing mode different exercises.
SUBSET_SEED=${AIDER_SUBSET_SEED:-20260924}
# The benchmark runs model-written code without review, so it runs in a
# container. That also means the endpoint must be reachable from inside it:
# the public entrypoint is, a loopback address is not.
IMAGE=${AIDER_IMAGE:-aider-benchmark}
