#!/bin/bash
# Runs one Aider polyglot benchmark against the current routing.
#
#   bench/aider/run.sh NAME
#
# NAME labels the run in aider's own results tree under $WORK/aider/tmp.*.
set -euo pipefail
source "$(dirname "$0")/env.sh"

NAME=${1:?usage: run.sh NAME}
cd "$WORK/aider"

# The key belongs to whichever node the agent talks to. With an entrypoint
# that is not this machine, and `mfsh key -addr` cannot fetch it: a key is read
# from its own node's disk and deliberately not served over the network, so it
# comes over ssh.
if [ -n "${ENTRYPOINT:-}" ]; then
  KEY=$(tailscale ssh "$ENTRYPOINT" '~/.local/bin/mfsh key' </dev/null | tr -d '\r\n')
  [ -n "$KEY" ] || { echo "could not read $ENTRYPOINT's API key over ssh" >&2; exit 1; }
else
  KEY=$("$MFSH" key)
fi

# AGENT_BASE is ModelFabric's, so every request is recorded with the node and engine
# that served it — the placement data this benchmark exists to produce. It
# must also be reachable from inside the container: the public entrypoint is,
# a loopback address is not.
case "$AGENT_BASE" in
  http://127.0.0.1*|http://localhost*)
    echo "warning: $AGENT_BASE is loopback and the benchmark runs in a container." >&2
    echo "         Set SWE_AGENT_BASE to the entrypoint's public URL, or the run will fail to connect." >&2
    ;;
esac

# Which exercises, decided here rather than by the harness. aider shuffles
# unseeded and takes the first N, so --num-tests alone gives every mode a
# different subset — see subset.py. Passing the list keeps the modes
# comparable and the split across the six languages even.
if [ "$NUM_TESTS" -ge 225 ]; then
  SELECT=()            # the whole set; nothing to choose
  WHICH="all 225 exercises"
else
  KEYWORDS=$("$AIDER_DIR/subset.py" "$WORK/aider/tmp.benchmarks/polyglot-benchmark" \
             "$NUM_TESTS" "$SUBSET_SEED") || exit 1
  SELECT=(--keywords "$KEYWORDS")
  WHICH="$NUM_TESTS of 225, seed $SUBSET_SEED"
fi

echo "=== $NAME: $WHICH, $THREADS threads, $EDIT_FORMAT edits"
echo "    model  openai/$MODEL"
echo "    api    $AGENT_BASE"
echo "    timeout ${TIMEOUT}s per API call"

# --new so a re-run does not resume the previous attempt's state and report a
# mixture of two routings as one result.
docker run --rm \
  -v "$PWD:/aider" \
  -v "$PWD/tmp.benchmarks:/benchmarks" \
  -w /aider \
  -e HISTFILE=/aider/.bash_history \
  -e AIDER_DOCKER=1 \
  -e AIDER_BENCHMARK_DIR=/benchmarks \
  -e OPENAI_API_BASE="$AGENT_BASE" \
  -e OPENAI_API_KEY="$KEY" \
  -e AIDER_TIMEOUT="$TIMEOUT" \
  "$IMAGE" \
  ./benchmark/benchmark.py "$NAME" \
    --model "openai/$MODEL" \
    --edit-format "$EDIT_FORMAT" \
    --threads "$THREADS" \
    "${SELECT[@]}" \
    --exercises-dir polyglot-benchmark \
    --new
