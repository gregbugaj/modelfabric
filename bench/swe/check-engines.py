#!/usr/bin/env python3
"""Lists every engine in `mfsh endpoints` with its real context and runtime.

Usage: mfsh endpoints | check-engines.py
A two-node comparison is only fair if every engine has the same n_ctx and
runtime; a stale binary on one node once gave it half the context.
"""
import json, re, sys, urllib.request

text = sys.stdin.read()
bad = 0
for block in text.split("  - name: ")[1:]:
    # A block without an address or port, and an engine that will not answer,
    # both used to end this with a traceback part-way down the list — after
    # some engines had printed, so it read like a partial success.
    addr = re.search(r'address: "([^"]+)"', block)
    port = re.search(r'port: "([^"]+)"', block)
    if not addr or not port:
        print(f"?  could not read an address and port from an endpoint block", file=sys.stderr)
        bad += 1
        continue
    addr, port = addr.group(1), port.group(1)
    model = re.search(r"modelfabric.sh/model: (\S+)", block)
    runtime = re.search(r"modelfabric.sh/runtime: (\S+)", block)
    try:
        props = json.load(urllib.request.urlopen(f"http://{addr}:{port}/props", timeout=10))
        n_ctx = props["default_generation_settings"]["n_ctx"]
    except Exception as e:
        print(f"{addr}:{port}  unreachable or unreadable: {e}", file=sys.stderr)
        bad += 1
        continue
    print(f"{addr}:{port}  {model.group(1) if model else '?'}  n_ctx={n_ctx}  runtime={runtime.group(1) if runtime else '?'}")
sys.exit(1 if bad else 0)
