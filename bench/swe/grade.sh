#!/bin/bash
# Grades a run with the official SWE-bench harness. The report lands in
# $WORK/eval/<model>.NAME.json. Usage: grade.sh NAME
set -euo pipefail
source "$(dirname "$0")/env.sh"
NAME=${1:?usage: grade.sh NAME}
# NAME becomes part of several paths. A "/" or ".." in it would read
# predictions from outside runs/ and scatter reports into other directories.
case "$NAME" in
  ""|*/*|*..*) echo "grade.sh: NAME must be a plain run name, not a path: $NAME" >&2; exit 2 ;;
esac
mkdir -p "$WORK/eval" && cd "$WORK/eval"
cp "../runs/$NAME/preds.json" "preds-$NAME.json"
../.venv/bin/python -m swebench.harness.run_evaluation -d "$DATASET" -s test \
  -p "preds-$NAME.json" --max_workers 6 -id "$NAME" > "$NAME.log" 2>&1
grep -E "^Instances (submitted|completed|resolved|unresolved|with)" "$NAME.log"
ls "$WORK/eval/"*".$NAME.json"
