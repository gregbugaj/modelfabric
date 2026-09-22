#!/bin/bash
# Runs the Aider polyglot benchmark across routing modes, on the same fleet
# and through the same reload-and-configure steps bench/swe uses.
#
#   bench/aider/sequence.sh PREFIX MODE...
#
# The mode loop itself lives in bench/swe/sequence.sh: disabling llm-d,
# reloading every engine cold so no mode inherits another's caches, setting
# the routing, then running. A second copy would drift from it, and the whole
# point is that both benchmarks see the same fleet in the same state.
set -euo pipefail
BENCH_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$BENCH_DIR/../.." && pwd)
source "$BENCH_DIR/env.sh"

# $WORK is aider's, but sequence.sh writes its per-mode logs under $WORK/runs.
mkdir -p "$WORK/runs"

# AIDER_DIR, not BENCH_DIR: sourcing env.sh above pulls in bench/swe/env.sh,
# which resets BENCH_DIR to bench/swe. Using it here handed the mode loop
# bench/swe's runner, so this script ran SWE-bench and wrote the result under
# an aider prefix. It only failed loudly because the two benchmarks keep their
# Python environments in different places.
RUNNER="$AIDER_DIR/run.sh"
[ -x "$RUNNER" ] || { echo "sequence.sh: no runner at $RUNNER" >&2; exit 1; }
RUNNER="$RUNNER" SWE_WORK="$WORK" exec bash "$REPO/bench/swe/sequence.sh" "$@"
