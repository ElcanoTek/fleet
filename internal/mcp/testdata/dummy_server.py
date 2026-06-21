import sys
import json
import os

# Ensure unbuffered output
sys.stdout.reconfigure(line_buffering=True)

def main():
    while True:
        try:
            line = sys.stdin.readline()
            if not line:
                break

            request = json.loads(line)
            req_id = request.get("id")
            method = request.get("method")
            params = request.get("params", {})

            response = {
                "jsonrpc": "2.0",
                "id": req_id,
            }

            if method == "initialize":
                response["result"] = {
                    "protocolVersion": "2024-11-05",
                    "serverInfo": {"name": "dummy", "version": "1.0"},
                    "capabilities": {}
                }
            elif method == "tools/list":
                response["result"] = {
                    "tools": [
                        {
                            "name": "echo",
                            "description": "Echoes input",
                            "inputSchema": {
                                "type": "object",
                                "properties": {
                                    "message": {"type": "string"}
                                }
                            }
                        }
                    ]
                }
            elif method == "tools/call":
                tool_name = params.get("name")
                args = params.get("arguments", {})

                if tool_name == "echo":
                    response["result"] = {
                        "content": [
                            {
                                "type": "text",
                                "text": f"Echo: {args.get('message')}"
                            }
                        ]
                    }
                else:
                    response["error"] = {"code": -32601, "message": "Method not found"}
            else:
                response["error"] = {"code": -32601, "message": "Method not found"}

            print(json.dumps(response))
        except Exception as e:
            # Write error to stderr for debugging
            sys.stderr.write(str(e) + "\n")
            break

if __name__ == "__main__":
    main()
