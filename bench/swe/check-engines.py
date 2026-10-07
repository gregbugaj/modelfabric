#!/usr/bin/env python3
"""List engines from mfsh endpoints with their runtime and actual context.

Usage: mfsh endpoints | check-engines.py
Verify these values before comparing runs; stale binaries can change context.
"""
import json, re, sys, urllib.request

text = sys.stdin.read()
bad = 0
for block in text.split("  - name: ")[1:]:
    # Report missing endpoints and unreachable engines without aborting the remaining checks.
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
