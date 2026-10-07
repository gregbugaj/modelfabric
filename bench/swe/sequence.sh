#!/bin/bash
# Runs the benchmark once per routing mode, reloading every engine before each
# run. Modes: "litellm" (a stock LiteLLM proxy choosing an engine at random:
# the off-the-shelf baseline, see litellm.sh), "router" (ModelFabric's own
# router: prefix affinity with a load veto, and what a node does when nothing
# else is configured), or an llm-d profile (mfsh llmd profiles).
# Usage: sequence.sh PREFIX MODE...   e.g. sequence.sh 1006 litellm router tuned
#
# ModelFabric routes, or llm-d schedules the one model it owns. Runs before
# 2026-09-25 used an arm named "direct" on an older chain and are not
# comparable to "router" on placement or wall clock: see README, "Runs so far".
set -uo pipefail
source "$(dirname "$0")/env.sh"
# Which benchmark to run for each mode. bench/swe and bench/aider drive the
# same fleet through the same routing modes; only the workload differs.
RUNNER=${RUNNER:-$BENCH_DIR/run.sh}
PREFIX=${1:?usage: sequence.sh PREFIX MODE...}; shift
# With an entrypoint, the llm-d being driven is that node's: it is the one
# requests arrive at, so it is the one whose routing is measured. Driving the
# local node's instead would leave the entrypoint on whatever it already had,
# and every mode would report a profile nothing was using.
#
# MFSH_ADDR rather than -addr on each call: one variable covers every call in
# the loop, where a flag has to be repeated and can be forgotten on one of
# them — which would silently drive this machine for that step.
[ -n "${CONTROL_ADDR:-}" ] && export MFSH_ADDR="$CONTROL_ADDR"
# Stopping the sequence must not advance it to the next mode: without this,
# killing the running agent just starts the next profile. Bash runs the trap
# once the current foreground command returns.
# A preferred node sends everything to one machine first, which is not the
# routing under test. Refuse rather than silently clear the operator's choice.
#
# Asked per node over ssh, by check-prefer.sh, because the check that used to
# live inline here read `mfsh prefer | grep -q` — which reports "no preference"
# for an error message, and `prefer` errors whenever MFSH_ADDR points at
# another node, as the line above just made it do.
"$BENCH_DIR/check-prefer.sh" || exit 1
# Saved defaults are the other thing that quietly changes what is measured,
# and unlike a preferred node they are per node, so two nodes can disagree
# while every run reports the same fleet.
"$BENCH_DIR/check-defaults.sh" || exit 1
# The mode whose counters are still on the engines. The save below runs
# before each reload, so it always belongs to the *previous* pass; empty on
# the first, when the engines carry whatever ran before this sequence.
PREV_MODE=""
trap 'echo "=== $(date +%T) stopped"; "$MFSH" llmd disable >/dev/null 2>&1; "$BENCH_DIR/litellm.sh" stop >/dev/null 2>&1; exit 130' INT TERM
for mode in "$@"; do
  echo "=== $(date +%T) $mode"
  "$MFSH" llmd disable >/dev/null 2>&1
  # The reload below restarts every engine, which zeroes its lifetime token
  # counters. Save them first: they are the only record of what share of the
  # fleet's work each engine was given, and nothing else keeps it.
  if [ -n "${MFSH_ADDR:-}" ] && [ -n "$PREV_MODE" ]; then
    "$BENCH_DIR/counters.py" "$MFSH_ADDR" save \
      "$WORK/runs/$PREFIX-$PREV_MODE.counters.txt" >/dev/null 2>&1 || true
  fi
  "$BENCH_DIR/reload.sh" || { echo "=== reload failed; stopping"; exit 1; }
  sleep 10
  # ...and check they actually went back to zero. A reload that quietly did
  # nothing on one node leaves it carrying the previous mode's totals into
  # this one, and nothing else in the run would look wrong.
  if [ -n "${MFSH_ADDR:-}" ]; then
    "$BENCH_DIR/counters.py" "$MFSH_ADDR" check || {
      echo "=== engine counters did not reset; stopping" >&2; exit 1; }
  fi
  # Nothing to configure for "router": the `llmd disable` above is the whole
  # setup, because ModelFabric's router is what a node does when llm-d is not
  # scheduling. An unknown mode name is an llm-d profile, and `llmd enable`
  # rejects it — which is the check, rather than a list repeated here.
  # The litellm arm bypasses ModelFabric's routing entirely: the agent talks to
  # a LiteLLM proxy that dials the engines itself. Started after the reload,
  # because it is configured with the engines the reload produced.
  AGENT_ENV=()
  if [ "$mode" = litellm ]; then
    lout=$("$BENCH_DIR/litellm.sh" start "$WORK/runs/$PREFIX-$mode") \
      || { echo "=== litellm did not start; stopping" >&2; exit 1; }
    AGENT_ENV=("SWE_AGENT_BASE=$(sed -n 's/^BASE=//p' <<<"$lout")" "SWE_AGENT_KEY=$(sed -n 's/^KEY=//p' <<<"$lout")")
  elif [ "$mode" != router ]; then
    # The calibration file is read by whichever node runs llm-d, which with an
    # entrypoint is not this one. Writing it locally looked like it worked and
    # did nothing: the two nodes held different values (655 here, 678 there)
    # while the run reported the remote one. Pinning has to land where llm-d
    # will read it, or the guarantee the pin exists for is false.
    if [ -n "$PEAK_PREFILL" ]; then
      cal=$(printf '{\n  "%s": %s\n}\n' "$MODEL" "$PEAK_PREFILL")
      if [ -n "$ENTRYPOINT" ]; then
        printf '%s\n' "$cal" | timeout 60 tailscale ssh "$ENTRYPOINT" \
          'mkdir -p ~/.modelfabric/llmd && cat > ~/.modelfabric/llmd/prefill-calibration.json' \
          || { echo "=== could not pin peak prefill on $ENTRYPOINT; stopping" >&2; exit 1; }
      else
        mkdir -p "$LLMD_DIR"
        printf '%s\n' "$cal" > "$LLMD_DIR/prefill-calibration.json"
      fi
      echo "  peak prefill  pinned at $PEAK_PREFILL tok/s${ENTRYPOINT:+ on $ENTRYPOINT}"
    fi
    "$MFSH" llmd enable "$MODEL" -profile "$mode" -room-filter="$ROOM_FILTER" | tail -1
    "$MFSH" llmd status | grep -E "profile|peak|engines|room filter"
  fi
  # Which engines are about to serve this mode. Verified again when it ends:
  # an engine that restarts mid-run — a node redeployed, a daemon bounced —
  # means half the results came from a different fleet, and nothing else in
  # the output would say so. That has cost two runs.
  if [ -n "${MFSH_ADDR:-}" ]; then
    "$BENCH_DIR/counters.py" "$MFSH_ADDR" fingerprint "$WORK/runs/$PREFIX-$mode.engines.txt" || true
  fi
  # A failed run must not be reported as a finished mode: the sequence would
  # carry on and the comparison would include a partial or empty run.
  if ! env "${AGENT_ENV[@]}" "$RUNNER" "$PREFIX-$mode" > "$WORK/runs/$PREFIX-$mode.log" 2>&1; then
    echo "=== $(date +%T) $mode FAILED (see $WORK/runs/$PREFIX-$mode.log); stopping" >&2
    "$MFSH" llmd disable >/dev/null 2>&1 || true
    [ "$mode" = litellm ] && "$BENCH_DIR/litellm.sh" stop "$WORK/runs/$PREFIX-$mode" >/dev/null 2>&1
    exit 1
  fi
  [ "$mode" = litellm ] && "$BENCH_DIR/litellm.sh" stop "$WORK/runs/$PREFIX-$mode"
  if [ -n "${MFSH_ADDR:-}" ] && ! "$BENCH_DIR/counters.py" "$MFSH_ADDR" verify "$WORK/runs/$PREFIX-$mode.engines.txt"; then
    echo "=== $(date +%T) $mode is not comparable; stopping" >&2
    "$MFSH" llmd disable >/dev/null 2>&1 || true
    exit 1
  fi
  echo "=== $(date +%T) $mode done: $(ls "$WORK/runs/$PREFIX-$mode"/*/*.traj.json 2>/dev/null | wc -l) trajectories"
  PREV_MODE="$mode"
done
# The last mode's counters are never followed by another reload, so save them
# here or they are lost to whatever reloads the fleet next.
if [ -n "${MFSH_ADDR:-}" ] && [ -n "$PREV_MODE" ]; then
  "$BENCH_DIR/counters.py" "$MFSH_ADDR" save "$WORK/runs/$PREFIX-$PREV_MODE.counters.txt" || true
fi
# Leave the fleet on ModelFabric's own router, which is where it started.
"$MFSH" llmd disable >/dev/null 2>&1
