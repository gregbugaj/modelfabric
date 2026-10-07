#!/bin/bash
# Run each routing mode with freshly loaded engines. Modes are litellm
# (least-busy proxy), router (ModelFabric placement), or an llm-d profile.
# Usage: sequence.sh PREFIX MODE...
# Legacy direct runs used a different request path and are not comparable.
set -uo pipefail
source "$(dirname "$0")/env.sh"
# Which benchmark to run for each mode. bench/swe and bench/aider drive the
# same fleet through the same routing modes; only the workload differs.
RUNNER=${RUNNER:-$BENCH_DIR/run.sh}
PREFIX=${1:?usage: sequence.sh PREFIX MODE...}; shift
# Set MFSH_ADDR for all control commands so they configure the entrypoint
# that actually routes benchmark requests.
[ -n "${CONTROL_ADDR:-}" ] && export MFSH_ADDR="$CONTROL_ADDR"
# Reject preferred-node settings without clearing them. Query every node over
# SSH; peer API errors must not be mistaken for an absent preference.
"$BENCH_DIR/check-prefer.sh" || exit 1
"$BENCH_DIR/check-defaults.sh" || exit 1
# The mode whose counters are still on the engines. The save below runs
# before each reload, so it always belongs to the *previous* pass; empty on
# the first, when the engines carry whatever ran before this sequence.
PREV_MODE=""
trap 'echo "=== $(date +%T) stopped"; "$MFSH" llmd disable >/dev/null 2>&1; "$BENCH_DIR/litellm.sh" stop >/dev/null 2>&1; exit 130' INT TERM
for mode in "$@"; do
  echo "=== $(date +%T) $mode"
  "$MFSH" llmd disable >/dev/null 2>&1
  # Save lifetime counters before reload resets them; they record per-engine work distribution.
  if [ -n "${MFSH_ADDR:-}" ] && [ -n "$PREV_MODE" ]; then
    "$BENCH_DIR/counters.py" "$MFSH_ADDR" save \
      "$WORK/runs/$PREFIX-$PREV_MODE.counters.txt" >/dev/null 2>&1 || true
  fi
  "$BENCH_DIR/reload.sh" || { echo "=== reload failed; stopping"; exit 1; }
  sleep 10
  # Verify counters reset so a failed reload cannot carry prior-mode work into this run.
  if [ -n "${MFSH_ADDR:-}" ]; then
    "$BENCH_DIR/counters.py" "$MFSH_ADDR" check || {
      echo "=== engine counters did not reset; stopping" >&2; exit 1; }
  fi
  # Disabling llm-d selects ModelFabric routing. Other mode names are validated
  # by llmd enable. Start LiteLLM after reload so it uses the new engine endpoints.
  AGENT_ENV=()
  if [ "$mode" = litellm ]; then
    lout=$("$BENCH_DIR/litellm.sh" start "$WORK/runs/$PREFIX-$mode") \
      || { echo "=== litellm did not start; stopping" >&2; exit 1; }
    AGENT_ENV=("SWE_AGENT_BASE=$(sed -n 's/^BASE=//p' <<<"$lout")" "SWE_AGENT_KEY=$(sed -n 's/^KEY=//p' <<<"$lout")")
  elif [ "$mode" != router ]; then
    # Write calibration on the node running llm-d; a local file cannot pin a remote entrypoint.
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
  # Compare engine identities before and after the run to detect restarts that change the measured fleet.
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
"$MFSH" llmd disable >/dev/null 2>&1
