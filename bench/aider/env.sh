#!/bin/bash
# Settings for the Aider polyglot routing benchmark. Override with AIDER_*.
#
# This is the short-task counterpart to bench/swe. SWE-bench Verified runs
# 20-80 turn conversations at 98% cache reuse, which measures prefix affinity
# and almost nothing else; polyglot runs 225 small exercises that mostly start
# cold, so it measures placement under a high request rate. A profile that
# wins both is better; one that wins only SWE-bench is good at cache affinity.

BENCH_DIR_AIDER=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
BENCH_DIR=$BENCH_DIR_AIDER
REPO=$(cd "$BENCH_DIR_AIDER/../.." && pwd)

# The fleet — model, load arguments, peers, the ModelFabric binary — is the same
# fleet bench/swe drives, and a second copy of those settings would drift from
# it. Only what is specific to this benchmark is set below.
source "$REPO/bench/swe/env.sh"
# That reset BENCH_DIR to bench/swe (it sets it from its own BASH_SOURCE), and
# sequence.sh wants it there — reload.sh and counters.py live in bench/swe.
# This benchmark's own scripts need a name that survives the sourcing.
AIDER_DIR=$BENCH_DIR_AIDER

WORK=${AIDER_WORK:-$HOME/.local/share/modelfabric/bench/aider}
# Aider's harness and the exercises live in separate repositories; the
# benchmark script expects the exercises under tmp.benchmarks/.
AIDER_REPO=${AIDER_REPO_URL:-https://github.com/Aider-AI/aider.git}
POLYGLOT_REPO=${AIDER_POLYGLOT_URL:-https://github.com/Aider-AI/polyglot-benchmark.git}
# 225 exercises across C++, Go, Java, JavaScript, Python and Rust. A subset is
# the point of this harness, and a small one: what is being measured is where
# requests land, not whether the model can code. Twenty-four is four per
# language and about 150 placement decisions, which is as much as sixty gave in
# a third of the wall clock — the exercises are long enough that the count was
# buying hours rather than data.
NUM_TESTS=${AIDER_NUM_TESTS:-24}
# Aider's own name for concurrency, and the dial that matters here. This began
# at four to match bench/swe's workers, which made the two benchmarks put the
# same load on the fleet — but four against the fleet's slots never queues, and
# queueing is the thing this benchmark was added to measure: "placement under a
# high request rate". Eight leaves at least one request waiting at all times,
# which is what exercises the room filter and head-of-line blocking.
#
# The fleet is seven slots now, not the six this was reasoned against — the
# per-node counts in bench/swe/env.sh are measured rather than uniform. Raise
# this if a fleet grows past it; the number that matters is threads > slots.
THREADS=${AIDER_THREADS:-8}
EDIT_FORMAT=${AIDER_EDIT_FORMAT:-whole}
# Aider's own API timeout, in seconds. Its default is None, which leaves
# whatever litellm, aider's HTTP client, decides, and that is how run
# 0925-direct ended: a request
# timed out somewhere inside the client without the connection being closed, so
# the engine kept generating for an answer nobody would read, then backpressure
# stalled it mid-stream. One slot on the single-slot Mac stayed occupied for
# ninety minutes while the other two nodes sat idle, 22 of 24 exercises done and
# the run unable to finish.
#
# Stated here so the client's behaviour is part of the recorded configuration
# rather than a library default that can change under us. It is deliberately
# larger than any single exercise needs: eight threads against seven slots
# queue, and a request waiting its turn is not a request in trouble.
#
# This is the client half. The half that actually frees the slot is ModelFabric's own
# stall bound (config request_stall, 15m by default), which abandons a request
# that makes no progress in either direction — a client that stops reading
# without hanging up cannot be detected any other way.
TIMEOUT=${AIDER_TIMEOUT_S:-900}
# The exercise subset is chosen by bench/aider/subset.py, not by aider, which
# shuffles unseeded — so --num-tests alone hands each routing mode a
# different subset. The seed fixes which exercises, so two modes and two days
# compare the same work.
SUBSET_SEED=${AIDER_SUBSET_SEED:-20260924}
# The benchmark runs model-written code without review, so it runs in a
# container. That also means the endpoint must be reachable from inside it:
# the public entrypoint is, a loopback address is not.
IMAGE=${AIDER_IMAGE:-aider-benchmark}
