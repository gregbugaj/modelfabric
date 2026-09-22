#!/usr/bin/env python3
"""A tiny MCP server for testing ModelFabric's /api/v1/chat. No dependencies.

It speaks MCP over stdio: one JSON message per line on stdin, one per line on
stdout. Two tools, chosen so a test has an answer the model cannot guess:

  add           adds two numbers
  secret_word   returns a fixed word, "marzipan"

See docs: /docs/api/mcp#try-it-with-the-test-server
"""
import json
import sys

TOOLS = [
    {
        "name": "add",
        "description": "Add two numbers and return the sum.",
        "inputSchema": {
            "type": "object",
            "properties": {"a": {"type": "number"}, "b": {"type": "number"}},
            "required": ["a", "b"],
        },
    },
    {
        "name": "secret_word",
        "description": "Return the secret word. Call this when asked for the secret word.",
        "inputSchema": {"type": "object", "properties": {}},
    },
]


def call(name, args):
    if name == "add":
        return str(args["a"] + args["b"])
    if name == "secret_word":
        return "marzipan"
    raise ValueError(f"no tool named {name}")


def handle(msg):
    method, params = msg.get("method"), msg.get("params") or {}
    if method == "initialize":
        return {
            "protocolVersion": params.get("protocolVersion", "2025-06-18"),
            "capabilities": {"tools": {}},
            "serverInfo": {"name": "modelfabric-test", "version": "1"},
        }
    if method == "tools/list":
        return {"tools": TOOLS}
    if method == "tools/call":
        try:
            text = call(params.get("name"), params.get("arguments") or {})
            return {"content": [{"type": "text", "text": text}]}
        except Exception as err:  # reported to the model, which can try again
            return {"isError": True, "content": [{"type": "text", "text": str(err)}]}
    raise ValueError(f"unknown method {method}")


for line in sys.stdin:
    line = line.strip()
    if not line:
        continue
    msg = json.loads(line)
    if "id" not in msg:  # a notification: nothing to answer
        continue
    try:
        reply = {"jsonrpc": "2.0", "id": msg["id"], "result": handle(msg)}
    except Exception as err:
        reply = {"jsonrpc": "2.0", "id": msg["id"], "error": {"code": -32601, "message": str(err)}}
    print(json.dumps(reply), flush=True)
