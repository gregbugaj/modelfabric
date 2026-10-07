#!/bin/bash
# Reject saved model defaults that could change unpinned settings such as reasoning effort or sampling across nodes.
set -uo pipefail
source "$(dirname "$0")/env.sh"

bad=0
for node in $("$MFSH" status --json 2>/dev/null | python3 -c '
import json, sys
try:
    d = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for n in [d.get("self", {})] + (d.get("peers") or []):
    name = n.get("node")
    if name:
        print(name)
' 2>/dev/null); do
  saved=$(curl -fsS --max-time 10 \
    "http://127.0.0.1:1234/api/v1/nodes/$node/model-defaults?model=$(python3 -c "import urllib.parse,sys; print(urllib.parse.quote(sys.argv[1], safe=''))" "$MODEL")" \
    2>/dev/null | python3 -c '
import json, sys
try:
    s = (json.load(sys.stdin).get("defaults") or {}).get("settings") or {}
except Exception:
    sys.exit(0)
if s:
    print(json.dumps(s, sort_keys=True))
' 2>/dev/null)
  if [ -n "$saved" ]; then
    echo "$node has saved defaults for $MODEL: $saved" >&2
    bad=1
  fi
done

if [ "$bad" != 0 ]; then
  cat >&2 <<MSG

Clear them on every node before a run, or the modes are not comparable:
  mfsh defaults $MODEL -clear                       # this node
  ssh <peer> '~/.local/bin/mfsh defaults $MODEL -clear'   # each peer
Set SWE_ALLOW_DEFAULTS=1 to run anyway.
MSG
  [ "${SWE_ALLOW_DEFAULTS:-}" = 1 ] || exit 1
fi
