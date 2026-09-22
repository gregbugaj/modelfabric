#!/bin/bash
# Creates the pinned Python environment (mini-swe-agent, swebench, and the
# litellm client library mini-swe-agent calls models through).
set -euo pipefail
source "$(dirname "$0")/env.sh"
mkdir -p "$WORK"
uv venv -q --python 3.12 "$WORK/.venv"
uv pip sync -q --python "$WORK/.venv/bin/python" "$BENCH_DIR/requirements.lock"
# mini-swe-agent writes a global config dir on import unless told where.
MSWEA_GLOBAL_CONFIG_DIR=$WORK/mswea-config "$WORK/.venv/bin/python" -c 'import minisweagent, swebench; print("mini-swe-agent", minisweagent.__version__)'
