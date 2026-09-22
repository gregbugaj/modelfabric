#!/bin/sh
# Stage this build onto the other machines in the mesh.
#
# Copies and verifies. By default it restarts nothing: replacing the file under
# a running node is safe on Linux and macOS — the kernel keeps the open inode —
# so each node picks the new build up at its next restart, on your schedule
# rather than in the middle of somebody's request.
#
#   scripts/deploy.sh                 every alive peer this node can see
#   scripts/deploy.sh minion helion   only these
#   scripts/deploy.sh -restart        and restart each node after staging it
#   scripts/deploy.sh -restart -force restart even nodes with models loaded
#
# -restart goes one node at a time and waits for each to answer again before
# the next, so the mesh never loses every node at once. A node with a model
# loaded is skipped unless -force is given: a restart unloads its models, and
# a 27B takes ~20s to load again. After the restart it checks that the node is
# running the binary it just staged, because a restart that brought back the
# old build has happened here before and looked like success.
#
# Nodes are addressed by the tailnet address the mesh reports, not by machine
# name: a name can resolve somewhere else entirely. A node that will not answer
# is reported and skipped, never guessed at — a partial rollout you know about
# beats a silent one.
#
# Environment:
#   MFSH_REMOTE_BIN   where to place it (default ~/.local/bin/mfsh)
#   SSH_OPTS          extra ssh/scp options
set -eu

DIST=${DIST:-dist}
REMOTE_BIN=${MFSH_REMOTE_BIN:-.local/bin/mfsh}
# accept-new, not "no": an address first seen today is recorded, while a host
# key that has *changed* still stops the copy, which is the case worth stopping.
SSH_OPTS=${SSH_OPTS:-"-o BatchMode=yes -o ConnectTimeout=8 -o StrictHostKeyChecking=accept-new"}
SELF=$(hostname -s 2>/dev/null || hostname)

die() { echo "deploy: $*" >&2; exit 1; }

RESTART=0 FORCE=0
while [ $# -gt 0 ]; do
  case "$1" in
    -restart) RESTART=1 ;;
    -force) FORCE=1 ;;
    -h | -help | --help) sed -n '2,/^set -eu/p' "$0" | sed '$d; s/^# \{0,1\}//'; exit 0 ;;
    -*) die "unknown flag $1 (flags: -restart, -force)" ;;
    *) break ;;
  esac
  shift
done
[ "$FORCE" = 0 ] || [ "$RESTART" = 1 ] || die "-force only means something with -restart"
PORT=${MFSH_PORT:-1234}

# ssh writes "Warning: Permanently added ..." to stderr on a first contact, so
# stderr is kept out of command output rather than merged into it: folded in,
# that warning became the machine's operating system.
errf=$(mktemp "${TMPDIR:-/tmp}/mfsh-deploy.XXXXXX")
trap 'rm -f "$errf"' EXIT INT TERM

# ssh_why explains an ssh failure. Tailscale SSH in "check" mode prints a login
# link and then waits, and ssh reports that as a timeout; reading only the last
# line said "port 22 timed out" about a machine that was up and answering.
ssh_why() {
  link=$(grep -o 'https://login.tailscale.com/[^ ]*' "$errf" | head -1)
  if [ -n "$link" ]; then
    echo "Tailscale SSH wants you to re-authenticate: open $link (or run 'ssh $1 true' once), then run this again"
  else
    tail -1 "$errf"
  fi
}

[ -d "$DIST" ] || die "no $DIST directory; run 'make dist' first"

# The mesh as the local node currently sees it: name, tailnet address, alive.
#
# Nodes are addressed by their tailnet address rather than their name. A bare
# machine name can resolve to something else entirely — helion's resolved to a
# LAN IPv6 address that refused the key, while its tailnet address answered
# straight away — and the mesh already knows the address that works.
mesh=""
if command -v python3 >/dev/null 2>&1; then
  mesh=$(curl -fsS --max-time 5 "http://127.0.0.1:1234/z/mesh" 2>/dev/null | python3 -c '
import json, sys
try:
    m = json.load(sys.stdin)
except Exception:
    sys.exit(0)
for p in m.get("peers", []):
    if p.get("node") and p.get("alive"):
        print(p["node"], p.get("addr", ""))
' 2>/dev/null) || mesh=""
fi

addr_of() {
  [ -n "$mesh" ] || return 0
  echo "$mesh" | awk -v n="$1" '$1 == n { print $2; exit }'
}

# Which machines. Asking the local node keeps this honest about what the mesh
# actually is right now, rather than a list in a file that drifts.
nodes=$*
if [ -z "$nodes" ]; then
  [ -n "$mesh" ] || die "could not read the mesh; name the nodes explicitly, e.g. $0 minion"
  nodes=$(echo "$mesh" | awk '{print $1}' | tr '\n' ' ')
  [ -n "$nodes" ] || die "no live peers found; is this node running? name them explicitly"
  echo "mesh peers: $nodes"
fi

staged=0 skipped=0 restarted=0 failed=0

# restart_node restarts one node and checks it came back on the staged build.
# The node's own loopback API is asked over ssh, so this works whatever the
# node's tailnet listener allows.
restart_node() {
  target=$1 where=$2 want=$3
  out=$(ssh $SSH_OPTS "$target" "
    bin=\"\$HOME/$REMOTE_BIN\"
    api=http://127.0.0.1:$PORT
    state=\$(curl -fsS --max-time 3 \$api/z/state 2>/dev/null || true)
    if [ -n \"\$state\" ] && ! echo \"\$state\" | grep -Eq '\"engines\":(null|\\[\\])' && [ '$FORCE' = 0 ]; then
      echo BUSY; exit 0
    fi
    # A unit from before the rename starts the old llmz binary, and systemd
    # would start it again after anything here stopped it.
    if systemctl --user is-active --quiet llmz 2>/dev/null; then
      echo \"FAILED it runs under the old llmz.service unit; on that machine run: systemctl --user disable --now llmz && ~/$REMOTE_BIN service install -enable\"; exit 1
    fi
    if systemctl --user is-active --quiet mfsh 2>/dev/null; then
      how=systemd
      unit_bin=\$(systemctl --user show -p ExecStart --value mfsh 2>/dev/null | sed -n 's/.*path=\\([^ ;]*\\).*/\\1/p')
      systemctl --user restart mfsh || { echo FAILED systemctl restart; exit 1; }
    else
      how=up
      unit_bin=
      upargs=
      pid=\$(curl -fsS --max-time 3 \$api/z/version 2>/dev/null | sed -n 's/.*\"pid\":\\([0-9]*\\).*/\\1/p')
      \"\$bin\" down >/dev/null 2>&1 </dev/null || true
      # A node started by hand with 'mfsh serve' is not one 'mfsh down' stops,
      # and 'mfsh up' then refuses the busy port. That is how minion ran, and
      # the first -restart left it on the old build. Restart it through
      # 'mfsh up' with the same flags, and refuse before stopping anything if
      # it was started with a flag 'up' cannot carry: a node left down is
      # worse than one left on the old build.
      if curl -fsS --max-time 2 \$api/healthz >/dev/null 2>&1; then
        [ -n \"\$pid\" ] || { echo FAILED a node answers on \$api but did not report its pid; exit 1; }
        cmd=\$(ps -o args= -p \"\$pid\" 2>/dev/null)
        set -- \$cmd
        [ \"\${2:-}\" = serve ] || { echo \"FAILED \$api is held by pid \$pid (\$cmd), which is not mfsh serve\"; exit 1; }
        shift 2
        while [ \$# -gt 0 ]; do
          case \"\$1\" in
            -config | --config) [ \$# -ge 2 ] || { echo FAILED \$1 has no value; exit 1; }
              # Carried forward only if it is there: the new build refuses a
              # named config that is missing, and one started on a path that
              # never existed (a capitalised ModelFabric directory where the real one is modelfabric) would stay down.
              [ -f \"\$2\" ] || { echo \"FAILED pid \$pid runs with -config \$2, which does not exist on this node; restart it yourself with the right path\"; exit 1; }
              upargs=\"\$upargs \$1 \$2\"; shift 2 ;;
            -config=* | --config=*) [ -f \"\${1#*=}\" ] || { echo \"FAILED pid \$pid runs with \$1, which does not exist on this node; restart it yourself with the right path\"; exit 1; }
              upargs=\"\$upargs \$1\"; shift ;;
            -listen | --listen) [ \$# -ge 2 ] || { echo FAILED \$1 has no value; exit 1; }; upargs=\"\$upargs \$1 \$2\"; shift 2 ;;
            -listen=* | --listen=* | -v | --v) upargs=\"\$upargs \$1\"; shift ;;
            *) echo \"FAILED pid \$pid was started by hand as '\$cmd', and mfsh up cannot carry \$1; restart it yourself\"; exit 1 ;;
          esac
        done
        kill -TERM \"\$pid\" || { echo FAILED could not stop pid \$pid; exit 1; }
        i=0
        while curl -fsS --max-time 1 \$api/healthz >/dev/null 2>&1; do
          i=\$((i + 1)); [ \$i -lt 30 ] || { echo FAILED pid \$pid did not stop within 30s; exit 1; }
          sleep 1
        done
        how=up-was-serve
      fi
      \"\$bin\" up \$upargs >/dev/null 2>&1 </dev/null || { echo FAILED mfsh up\$upargs; exit 1; }
    fi
    i=0
    until curl -fsS --max-time 2 \$api/healthz >/dev/null 2>&1; do
      i=\$((i + 1)); [ \$i -lt 60 ] || { echo FAILED no answer on \$api after 60s; exit 1; }
      sleep 1
    done
    ver=\$(curl -fsS --max-time 3 \$api/z/version 2>/dev/null)
    exe=\$(echo \"\$ver\" | sed -n 's/.*\"executable\":\"\\([^\"]*\\)\".*/\\1/p')
    pid=\$(echo \"\$ver\" | sed -n 's/.*\"pid\":\\([0-9]*\\).*/\\1/p')
    run=
    if [ -n \"\$pid\" ] && [ -e /proc/\$pid/exe ]; then
      run=\$(sha256sum /proc/\$pid/exe 2>/dev/null | cut -d' ' -f1)
    fi
    echo \"OK \$how \${exe:-unknown} \${run:-unknown} \${unit_bin:-}\"
  " 2>"$errf" </dev/null) || { echo "    restart FAILED — $out$(tail -1 "$errf")"; return 1; }

  case "$out" in
    BUSY)
      echo "    not restarted: it has models loaded, and a restart unloads them (-force to do it anyway)"
      return 0 ;;
    OK*) ;;
    *) echo "    restart FAILED — $out"; return 1 ;;
  esac
  set -- $out
  how=$2 exe=$3 run=$4 unit_bin=${5:-}
  if [ "$run" = "$want" ]; then
    echo "    restarted ($how), running the staged build"
  elif [ "$run" = unknown ]; then
    # macOS has no /proc to hash the running image from; say what is known.
    echo "    restarted ($how), running $exe; its build could not be checked on this system"
  else
    echo "    restarted ($how), but it is NOT running the staged build: $exe"
    [ -z "$unit_bin" ] || echo "    the systemd unit starts $unit_bin, not ~/$REMOTE_BIN; point the unit at the staged binary"
    return 1
  fi
  restarted=$((restarted + 1))
}

for node in $nodes; do
  if [ "$node" = "$SELF" ]; then
    echo "  $node: skipped (this machine — it is where the build came from)"
    skipped=$((skipped + 1))
    continue
  fi

  # The tailnet address where the mesh knows one; the bare name otherwise, so
  # a machine that is not in the mesh yet can still be staged by name.
  target=$(addr_of "$node")
  where="$node"
  if [ -n "$target" ]; then
    where="$node ($target)"
  else
    target="$node"
  fi

  # uname over ssh rather than the node's HTTP API: it answers even when mfsh
  # is down, which is exactly when you may be redeploying.
  if ! uname_out=$(ssh $SSH_OPTS "$target" 'uname -sm' 2>"$errf" </dev/null); then
    echo "  $where: SKIPPED — ssh failed: $(ssh_why "$target")"
    skipped=$((skipped + 1))
    continue
  fi

  os=$(echo "$uname_out" | cut -d' ' -f1)
  arch=$(echo "$uname_out" | cut -d' ' -f2)
  case "$os" in
    Linux) os=linux ;;
    Darwin) os=darwin ;;
    *) echo "  $where: SKIPPED — unsupported system $os"; skipped=$((skipped + 1)); continue ;;
  esac
  case "$arch" in
    x86_64 | amd64) arch=amd64 ;;
    aarch64 | arm64) arch=arm64 ;;
    *) echo "  $where: SKIPPED — unsupported architecture $arch"; skipped=$((skipped + 1)); continue ;;
  esac

  bin="$DIST/mfsh-$os-$arch"
  [ -f "$bin" ] || { echo "  $where: SKIPPED — $bin not built"; skipped=$((skipped + 1)); continue; }
  want=$( (sha256sum "$bin" 2>/dev/null || shasum -a 256 "$bin") | cut -d' ' -f1)

  # To a temp name first, then rename into place: writing directly to a
  # running executable fails with ETXTBSY, while rename() over it does not.
  tmp=".mfsh-staged.$$"
  if ! scp $SSH_OPTS -q "$bin" "$target:$tmp" </dev/null; then
    echo "  $where: SKIPPED — copy failed"
    skipped=$((skipped + 1))
    continue
  fi

  # Verified on the node, against the hash computed here: a copy that arrived
  # wrong must not be renamed over a working binary.
  out=$(ssh $SSH_OPTS "$target" "
    got=\$( (sha256sum '$tmp' 2>/dev/null || shasum -a 256 '$tmp') | cut -d' ' -f1)
    if [ \"\$got\" != '$want' ]; then rm -f '$tmp'; echo \"MISMATCH \$got\"; exit 1; fi
    mkdir -p \"\$(dirname \"\$HOME/$REMOTE_BIN\")\"
    old=''
    if [ -f \"\$HOME/$REMOTE_BIN\" ]; then
      old=\$( (sha256sum \"\$HOME/$REMOTE_BIN\" 2>/dev/null || shasum -a 256 \"\$HOME/$REMOTE_BIN\") | cut -d' ' -f1)
    fi
    chmod 755 '$tmp' && mv -f '$tmp' \"\$HOME/$REMOTE_BIN\"
    echo \"OK \${old:-none}\"
  " 2>"$errf" </dev/null) || { echo "  $where: FAILED — $out$(tail -1 "$errf")"; skipped=$((skipped + 1)); continue; }

  old=$(echo "$out" | awk '/^OK/ {print $2}')
  if [ "$old" = "$want" ]; then
    echo "  $where: already this build ($os/$arch)"
  else
    echo "  $where: staged $os/$arch  ${old%"${old#????????}"}… -> ${want%"${want#????????}"}…"
  fi
  staged=$((staged + 1))

  [ "$RESTART" = 1 ] || continue
  restart_node "$target" "$where" "$want" || failed=$((failed + 1))
done

echo
echo "staged on $staged node(s), skipped $skipped."
if [ "$RESTART" = 1 ]; then
  echo "restarted $restarted node(s)$([ "$failed" = 0 ] || echo ", $failed failed")."
  [ "$failed" = 0 ] || exit 1
else
  echo "Nothing was restarted: each node keeps running its old build until you restart it (or pass -restart)."
fi
