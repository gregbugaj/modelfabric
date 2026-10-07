#!/bin/bash
# Run the Aider benchmark across routing modes.
#
#   bench/aider/sequence.sh PREFIX MODE...
#
# Reuse bench/swe/sequence.sh so both workloads receive the same reload and routing setup.
set -euo pipefail
BENCH_DIR=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
REPO=$(cd "$BENCH_DIR/../.." && pwd)
source "$BENCH_DIR/env.sh"

# $WORK is aider's, but sequence.sh writes its per-mode logs under $WORK/runs.
mkdir -p "$WORK/runs"

# Use AIDER_DIR: the shared env resets BENCH_DIR to bench/swe, which would select the wrong runner.
RUNNER="$AIDER_DIR/run.sh"
[ -x "$RUNNER" ] || { echo "sequence.sh: no runner at $RUNNER" >&2; exit 1; }
RUNNER="$RUNNER" SWE_WORK="$WORK" exec bash "$REPO/bench/swe/sequence.sh" "$@"
