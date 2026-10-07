#!/bin/sh
# Stage and verify builds on mesh peers. Restarts require -restart.
# Renaming the binary preserves the running executable inode on Linux and macOS.
#
#   scripts/deploy.sh                  all visible live peers
#   scripts/deploy.sh NODE...          selected peers
#   scripts/deploy.sh -restart         restart nodes sequentially
#   scripts/deploy.sh -restart -force  include nodes with loaded models
#
# Wait for each restarted node and verify its build before continuing. Skip
# loaded nodes unless forced; restarting unloads models. Use mesh tailnet
# addresses where available and report unreachable nodes.
#
# Environment:
#   MFSH_REMOTE_BIN  destination (default ~/.local/bin/mfsh)
#   SSH_OPTS         additional ssh/scp options
set -eu

DIST=${DIST:-dist}
REMOTE_BIN=${MFSH_REMOTE_BIN:-.local/bin/mfsh}
# accept-new records new host keys but rejects changed keys.
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

# Keep SSH stderr separate; first-contact host-key warnings must not enter parsed uname output.
errf=$(mktemp "${TMPDIR:-/tmp}/mfsh-deploy.XXXXXX")
trap 'rm -f "$errf"' EXIT INT TERM

# ssh_why includes Tailscale SSH check-mode login links that precede the final timeout message.
ssh_why() {
  link=$(grep -o 'https://login.tailscale.com/[^ ]*' "$errf" | head -1)
  if [ -n "$link" ]; then
    echo "Tailscale SSH wants you to re-authenticate: open $link (or run 'ssh $1 true' once), then run this again"
  else
    tail -1 "$errf"
  fi
}

[ -d "$DIST" ] || die "no $DIST directory; run 'make dist' first"

# Read node names, tailnet addresses and liveness from the mesh. Prefer
# tailnet addresses because hostnames can resolve to unrelated interfaces.
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

nodes=$*
if [ -z "$nodes" ]; then
  [ -n "$mesh" ] || die "could not read the mesh; name the nodes explicitly, e.g. $0 minion"
  nodes=$(echo "$mesh" | awk '{print $1}' | tr '\n' ' ')
  [ -n "$nodes" ] || die "no live peers found; is this node running? name them explicitly"
  echo "mesh peers: $nodes"
fi

staged=0 skipped=0 restarted=0 failed=0

# with_timeout bounds the whole command in seconds, including SSH check-mode
# waits after connection. Use POSIX sh because macOS lacks timeout. Redirect
# watcher output so command substitution does not wait for its sleep.
with_timeout() {
  secs=$1
  shift
  "$@" &
  cmd=$!
  (sleep "$secs" && kill "$cmd") >/dev/null 2>&1 &
  watch=$!
  wait "$cmd"
  rc=$?
  kill "$watch" 2>/dev/null
  return $rc
}

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
    # Reject the legacy unit because systemd would restart the old binary.
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
      # Manual serve processes are not managed by mfsh down. Preserve supported
      # flags when restarting through mfsh up; reject unsupported flags before
      # stopping the process to avoid leaving the node down.
      if curl -fsS --max-time 2 \$api/healthz >/dev/null 2>&1; then
        [ -n \"\$pid\" ] || { echo FAILED a node answers on \$api but did not report its pid; exit 1; }
        cmd=\$(ps -o args= -p \"\$pid\" 2>/dev/null)
        set -- \$cmd
        [ \"\${2:-}\" = serve ] || { echo \"FAILED \$api is held by pid \$pid (\$cmd), which is not mfsh serve\"; exit 1; }
        shift 2
        while [ \$# -gt 0 ]; do
          case \"\$1\" in
            -config | --config) [ \$# -ge 2 ] || { echo FAILED \$1 has no value; exit 1; }
              # Require the explicit config path to exist before stopping the node;
              # the replacement process rejects missing named configs.
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

  # Use uname over SSH so deployment can identify hosts while mfsh is down.
  if ! uname_out=$(with_timeout ${SSH_PROBE_SECS:-30} ssh $SSH_OPTS "$target" 'uname -sm' 2>"$errf" </dev/null); then
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
