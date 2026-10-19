"""Test fixture: an MCP stdio server whose initialize waits at a barrier.

On initialize it records an arrival in $BARRIER_DIR (kept for the whole test,
so a server that is slow to be scheduled still counts the others) and an
"active" marker (removed before it answers). It then waits, up to 10 s, until
$BARRIER_N arrivals exist, and writes "<reached> <active>" to seen-<pid>:
reached is 1 when the barrier filled, active is how many servers were
initializing at once when it arrived. Concurrent binds fill the barrier;
one-after-another binds only ever see active == 1.
"""

import json
import os
import sys
import time


def count(barrier: str, prefix: str) -> int:
    return sum(1 for n in os.listdir(barrier) if n.startswith(prefix))


def initialize() -> None:
    barrier = os.environ["BARRIER_DIR"]
    want = int(os.environ["BARRIER_N"])
    pid = os.getpid()
    marker = os.path.join(barrier, f"active-{pid}")
    with open(marker, "w"):
        pass
    with open(os.path.join(barrier, f"arrived-{pid}"), "w"):
        pass
    active = count(barrier, "active-")
    deadline = time.monotonic() + 10
    reached = 0
    while time.monotonic() < deadline:
        if count(barrier, "arrived-") >= want:
            reached = 1
            break
        time.sleep(0.02)
    with open(os.path.join(barrier, f"seen-{pid}"), "w") as f:
        f.write(f"{reached} {active}")
    os.remove(marker)


def main() -> None:
    for line in sys.stdin:
        request = json.loads(line)
        req_id = request.get("id")
        if req_id is None:
            continue
        method = request.get("method")
        response = {"jsonrpc": "2.0", "id": req_id}
        if method == "initialize":
            initialize()
            response["result"] = {
                "protocolVersion": "2024-11-05",
                "serverInfo": {"name": "barrier", "version": "1.0"},
                "capabilities": {},
            }
        elif method == "tools/list":
            response["result"] = {
                "tools": [
                    {
                        "name": "ping",
                        "description": "Answers pong",
                        "inputSchema": {"type": "object", "properties": {}},
                    }
                ]
            }
        else:
            response["error"] = {"code": -32601, "message": "Method not found"}
        print(json.dumps(response), flush=True)


if __name__ == "__main__":
    main()
