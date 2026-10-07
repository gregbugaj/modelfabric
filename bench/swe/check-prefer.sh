#!/bin/bash
# Reject preferred-node settings on every routing and model-resolution node.
# Query loopback over SSH because prefer is unavailable through the peer API.
# Treat query failures as failures; matching output alone previously accepted API errors.
set -uo pipefail
source "$(dirname "$0")/env.sh"

# The control node first when there is one: it is the entrypoint, so its
# preference is the one that steers the run.
nodes=""
[ -n "${ENTRYPOINT:-}" ] && nodes="$ENTRYPOINT"
for p in $PEERS; do
  [ "$p" = "${ENTRYPOINT:-}" ] || nodes="$nodes $p"
done

bad=0
unreachable=0

check() { # name, output
  case "$2" in
    "Preferred node:"*)
      echo "$1 has a preferred node set: ${2#Preferred node: }" >&2
      bad=1 ;;
    "No preferred node"*) ;;
    *)
      # Reject unexpected output; it does not establish that the check passed.
      echo "$1: could not read the preferred node — ${2:-no answer}" >&2
      unreachable=1 ;;
  esac
}

# This machine, on its own loopback address rather than MFSH_ADDR, which
# sequence.sh points at the entrypoint.
check "$(hostname -s 2>/dev/null || echo "this node")" \
  "$(MFSH_ADDR=http://127.0.0.1:1234 "$MFSH" prefer 2>&1 | head -1)"

for node in $nodes; do
  out=$(timeout 30 tailscale ssh "$node" '~/.local/bin/mfsh prefer 2>&1 | head -1' </dev/null 2>&1 | tr -d '\r')
  check "$node" "$out"
done

if [ "$unreachable" != 0 ]; then
  echo >&2
  echo "A node that cannot be asked is not a node that passed. Fix the access" >&2
  echo "(tailscale ssh usually wants a browser approval) and run again." >&2
  [ "${SWE_ALLOW_UNCHECKED_PREFER:-}" = 1 ] || exit 1
fi

if [ "$bad" != 0 ]; then
  cat >&2 <<MSG

Clear it before a run, or the modes are not comparable:
  mfsh prefer -none                              # this node
  ssh <peer> '~/.local/bin/mfsh prefer -none'    # each peer
MSG
  exit 1
fi

echo "✓ no preferred node on any node in the fleet"
