"""Minimal stdio MCP server for `fleet mcp test --deep` tests.

Advertises one auth-status tool. Set AUTH_FAIL=1 in the env to make the
tools/call result carry isError=true (a failed upstream credential check).
"""
import json
import os
import sys

sys.stdout.reconfigure(line_buffering=True)

FAIL = os.environ.get("AUTH_FAIL") == "1"


def main():
    while True:
        line = sys.stdin.readline()
        if not line:
            break
        req = json.loads(line)
        rid, method = req.get("id"), req.get("method")
        if method == "notifications/initialized":
            continue
        resp = {"jsonrpc": "2.0", "id": rid}
        if method == "initialize":
            resp["result"] = {
                "protocolVersion": "2024-11-05",
                "serverInfo": {"name": "authstatus", "version": "1.0"},
                "capabilities": {},
            }
        elif method == "tools/list":
            resp["result"] = {
                "tools": [
                    {
                        "name": "demo_auth_status",
                        "description": "reports upstream credential validity",
                        "inputSchema": {"type": "object", "properties": {}},
                    }
                ]
            }
        elif method == "tools/call":
            if FAIL:
                resp["result"] = {
                    "content": [{"type": "text", "text": "401 Unauthorized: key revoked"}],
                    "isError": True,
                }
            else:
                resp["result"] = {
                    "content": [{"type": "text", "text": "authenticated: seat 12345 ok"}],
                    "isError": False,
                }
        else:
            resp["error"] = {"code": -32601, "message": f"unknown method {method}"}
        print(json.dumps(resp))


if __name__ == "__main__":
    main()
