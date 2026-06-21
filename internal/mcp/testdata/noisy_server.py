"""Test fixture: a JSON-RPC stdio server that pollutes stdout before answering.

Before each real response it emits (1) a non-JSON stray print, (2) a JSON-RPC
notification (method, no id), and (3) a stale response with a mismatched id.
StdioTransport.Call must skip all three and return the correctly-id'd result.
"""

import json
import sys

sys.stdout.reconfigure(line_buffering=True)


def main() -> None:
    while True:
        line = sys.stdin.readline()
        if not line:
            break
        request = json.loads(line)
        req_id = request.get("id")

        # 1. Stray non-JSON output (e.g. a library print).
        print("WARNING: something logged straight to stdout")
        # 2. Server-initiated notification.
        print(json.dumps({"jsonrpc": "2.0", "method": "notifications/progress", "params": {"progress": 1}}))
        # 3. Stale response to a request id that is not ours.
        print(json.dumps({"jsonrpc": "2.0", "id": 999999, "result": {"echoed": -1}}))
        # 4. The real response.
        value = request.get("params", {}).get("value", 0)
        print(json.dumps({"jsonrpc": "2.0", "id": req_id, "result": {"echoed": value}}))


if __name__ == "__main__":
    main()
