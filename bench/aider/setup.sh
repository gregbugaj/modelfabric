#!/bin/bash
# Clones aider and the polyglot exercises, and builds the benchmark image.
# Idempotent: re-running updates the checkouts and rebuilds only if needed.
set -euo pipefail
source "$(dirname "$0")/env.sh"

mkdir -p "$WORK"
cd "$WORK"

if [ ! -d aider/.git ]; then
  echo "cloning aider..."
  git clone --depth 1 "$AIDER_REPO" aider
else
  echo "aider already cloned"
fi

# The harness requires exercises under tmp.benchmarks/ in its checkout.
mkdir -p aider/tmp.benchmarks
if [ ! -d aider/tmp.benchmarks/polyglot-benchmark/.git ]; then
  echo "cloning polyglot exercises..."
  git clone --depth 1 "$POLYGLOT_REPO" aider/tmp.benchmarks/polyglot-benchmark
else
  echo "exercises already cloned"
fi

cd aider
if ! docker image inspect "$IMAGE" >/dev/null 2>&1; then
  echo "building the benchmark image (a few minutes the first time)..."
  # Aider ships the Dockerfile the harness expects; docker_build.sh names the
  # image itself, so the tag is applied after rather than passed in.
  ./benchmark/docker_build.sh
else
  echo "image $IMAGE already built"
fi

echo
echo "ready. Exercises: $(find tmp.benchmarks/polyglot-benchmark -maxdepth 2 -name exercises -type d | wc -l) language trees"
echo "run one mode with: bench/aider/run.sh NAME"
