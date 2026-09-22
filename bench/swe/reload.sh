#!/bin/bash
# Reloads the model on this node and every peer, so a run starts with cold
# caches, then checks each engine really got the context asked for.
set -euo pipefail
source "$(dirname "$0")/env.sh"
# Engines are loaded on the machines that have GPUs, never on the entrypoint.
# sequence.sh exports MFSH_ADDR so llm-d control reaches the node requests
# arrive at; inheriting it here would send `mfsh load` to a node whose role is
# entrypoint and which holds no models at all.
unset MFSH_ADDR
"$MFSH" unload --all >/dev/null || true
for peer in $PEERS; do
  # The remote shell parses this string, so MODEL and LOAD_ARGS are quoted for
  # it rather than pasted in: a space or a metacharacter in either would
  # otherwise change the command that runs on the peer. LOAD_ARGS is a
  # deliberate word list, so it is expanded here and quoted word by word.
  #
  # PEER_MFSH is not quoted, and that is the point: its default starts with
  # "~", and %q escapes the tilde so the remote shell takes it literally —
  # "~/.local/bin/mfsh: No such file or directory", on every peer, before
  # anything had a chance to run. It is ModelFabric's own path, not a value from the
  # run, so the remote shell is allowed to expand it.
  peer_args=$(loadArgsFor "$peer")
  remote_cmd=$(printf '%s unload --all >/dev/null; %s load %q' "$PEER_MFSH" "$PEER_MFSH" "$MODEL")
  for arg in $peer_args; do remote_cmd+=$(printf ' %q' "$arg"); done
  remote_cmd+=$(printf " >/dev/null && echo %q loaded" "$peer")
  echo "  $peer: $peer_args"
  # 600s, not 180: a 27B is minutes of disk on a node whose page cache is cold,
  # and the Mac reads it over a slower path than either CUDA box. A timeout
  # here stops the sequence, which is right — but it must mean "the peer is
  # wedged", not "this machine is slower than the one the timeout was written
  # for".
  timeout 600 tailscale ssh "$peer" "$remote_cmd" \
    || { echo "$peer did not reload (Tailscale SSH may need a browser approval)" >&2; exit 1; }
done
local_args=$(loadArgsFor "$("$MFSH" status --json 2>/dev/null | python3 -c '
import json, sys
try: print((json.load(sys.stdin).get("self") or {}).get("node") or "")
except Exception: print("")
' 2>/dev/null)")
echo "  this node: $local_args"
"$MFSH" load $MODEL $local_args >/dev/null && echo "local loaded"
# Every engine serving the model, with its real context and runtime.
"$MFSH" endpoints | "$BENCH_DIR/check-engines.py"
