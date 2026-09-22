#!/bin/bash
# Refuses to run when any node in the fleet has a preferred node set.
#
# A preference sends every request to one machine first, which is not the
# routing under test — so a run with one set measures the preference, not the
# mode, and says nothing about either.
#
# This exists because the check it replaces failed open, on every run for a
# week. sequence.sh did:
#
#     if "$MFSH" prefer | grep -q '^Preferred node:'
#
# Two faults, and the second hides the first. `prefer` is not in the peer
# allowlist, so with an entrypoint (MFSH_ADDR pointing at another node) it
# answers "not available over the mesh" and exits 1 — and grep, finding no
# match in an error message, reports success. A check that cannot run reads
# exactly like a check that passed.
#
# It was also asking the wrong machine. What matters is the preference on the
# node doing the routing, and on every node that resolves a model — so each is
# asked on its own loopback interface, over ssh, where the answer is real.
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
      # Anything else is a check that did not run. Saying so is the whole
      # point: silence here is what cost the earlier runs.
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
