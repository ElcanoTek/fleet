import atexit
import datetime
import json
import math
import os
import queue
import signal
import subprocess
import sys
import tempfile
import time

from jupyter_client import BlockingKernelClient

# Global state
kernel_process = None
client = None

IOPUB_POLL_SECONDS = 0.25
DEFAULT_EXECUTION_TIMEOUT_SECONDS = 300
MAX_CAPTURE_BYTES = int(os.environ.get("CUTLASS_PYTHON_CAPTURE_BYTES", "131072"))

# Module-level so cleanup() can delete it on exit/signal. Only the bridge
# process itself ever reads this file — the kernel writes it, the client
# in the same process reads it, then we delete it in cleanup().
connection_file = None


def start_kernel():
    """Starts a new IPython kernel and returns the connection file path.

    The connection file (kernel-<pid>.json) goes into the OS temp dir so
    it doesn't clutter whatever cwd the bridge was launched from. The
    Go side's `run_python` tool holds one bridge per turn and calls
    `terminateBridge()` at turn end, which SIGTERMs us → cleanup() runs
    → the file is deleted. If we crash hard (OOM, kill -9), the OS
    reaps /tmp on reboot anyway.
    """
    global kernel_process, connection_file

    # tempfile.mkstemp creates a unique empty file and returns (fd, path).
    # We close the fd immediately since ipykernel will overwrite the
    # content — we only needed the unique name.
    fd, connection_file = tempfile.mkstemp(prefix=f"kernel-{os.getpid()}-", suffix=".json")
    os.close(fd)
    # ipykernel creates the file itself with its own content. Our mkstemp
    # empty placeholder would make the while-loop below think the file
    # "exists" before the kernel has actually written to it, so we remove
    # our placeholder and let ipykernel create it fresh.
    try:
        os.unlink(connection_file)
    except OSError:
        pass

    # Start the kernel
    cmd = [
        sys.executable,
        "-m", "ipykernel_launcher",
        "-f", connection_file
    ]

    # Start process detached to avoid signal interference
    kernel_process = subprocess.Popen(
        cmd,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        start_new_session=True
    )

    # Wait for connection file to exist
    retries = 50
    while not os.path.exists(connection_file):
        if retries == 0:
            raise RuntimeError("Timed out waiting for kernel connection file")
        time.sleep(0.1)
        retries -= 1

    return connection_file

def cleanup():
    """Kills the kernel and deletes the connection file."""
    global kernel_process, connection_file
    if kernel_process:
        try:
            os.killpg(os.getpgid(kernel_process.pid), signal.SIGTERM)
        except Exception:
            pass
        kernel_process = None
    if connection_file:
        try:
            os.unlink(connection_file)
        except OSError:
            pass
        connection_file = None


class OutputCollector:
    """Bound output capture to avoid unbounded bridge memory growth."""

    def __init__(self, limit_bytes):
        self.limit_bytes = limit_bytes
        self.total_bytes = 0
        self.captured_bytes = 0
        self.parts = []
        self.truncated = False

    def append(self, value):
        if value is None:
            return
        if not isinstance(value, str):
            value = str(value)
        raw = value.encode("utf-8", "replace")
        self.total_bytes += len(raw)
        if self.captured_bytes >= self.limit_bytes:
            self.truncated = True
            return
        remaining = self.limit_bytes - self.captured_bytes
        chunk = raw[:remaining]
        if chunk:
            self.parts.append(chunk.decode("utf-8", "replace"))
            self.captured_bytes += len(chunk)
        if len(raw) > remaining:
            self.truncated = True

    def render(self):
        text = "".join(self.parts)
        if self.truncated:
            text += f"\n[TRUNCATED: captured first {self.captured_bytes} of {self.total_bytes} bytes]"
        return text

    def info(self):
        return {
            "truncated": self.truncated,
            "captured_bytes": self.captured_bytes,
            "total_bytes": self.total_bytes,
        }


def execution_timeout_error(prefix, timeout=DEFAULT_EXECUTION_TIMEOUT_SECONDS):
    return {
        "status": "error",
        "stdout": "",
        "stderr": "",
        "result": None,
        "error": f"{prefix} timed out after {timeout} seconds",
        "bridge_truncation": {},
    }


def remaining_time(deadline):
    return max(0.01, deadline - time.time())


def run_code_on_kernel(code, client, timeout_seconds=None):
    """Helper to run code and capture output."""
    timeout = timeout_seconds if timeout_seconds else DEFAULT_EXECUTION_TIMEOUT_SECONDS
    msg_id = client.execute(code)

    stdout_content = OutputCollector(MAX_CAPTURE_BYTES)
    stderr_content = OutputCollector(MAX_CAPTURE_BYTES)
    result_content = OutputCollector(MAX_CAPTURE_BYTES)
    error_content = None
    status = "success"
    shell_reply_seen = False
    idle_seen = False
    deadline = time.time() + timeout

    while time.time() < deadline:
        if not shell_reply_seen:
            try:
                shell_msg = client.get_shell_msg(timeout=min(0.05, remaining_time(deadline)))
                if shell_msg["parent_header"].get("msg_id") == msg_id:
                    shell_reply_seen = True
                    shell_content = shell_msg.get("content", {})
                    if shell_content.get("status") == "error":
                        traceback = shell_content.get("traceback", [])
                        error_content = "\n".join(traceback) or error_content
                        status = "error"
            except queue.Empty:
                pass

        try:
            # Get IOPub messages (streams, display_data, etc)
            msg = client.get_iopub_msg(timeout=min(IOPUB_POLL_SECONDS, remaining_time(deadline)))
            msg_type = msg['header']['msg_type']
            content = msg['content']

            if msg['parent_header'].get('msg_id') != msg_id:
                continue

            if msg_type == 'stream':
                if content['name'] == 'stdout':
                    stdout_content.append(content['text'])
                elif content['name'] == 'stderr':
                    stderr_content.append(content['text'])
            elif msg_type == 'execute_result':
                result_content.append(content['data'].get('text/plain', ''))
            elif msg_type == 'display_data':
                result_content.append(content['data'].get('text/plain', ''))
            elif msg_type == 'error':
                traceback = content.get('traceback', [])
                error_content = '\n'.join(traceback)
                status = "error"
            elif msg_type == 'status':
                if content['execution_state'] == 'idle':
                    idle_seen = True

        except queue.Empty:
            pass

        if shell_reply_seen and idle_seen:
            break

    if not shell_reply_seen:
        return execution_timeout_error("python shell reply", timeout)
    if not idle_seen:
        return execution_timeout_error("python iopub idle wait", timeout)

    bridge_truncation = {}
    for key, collector in {
        "stdout": stdout_content,
        "stderr": stderr_content,
        "result": result_content,
    }.items():
        if collector.truncated:
            bridge_truncation[key] = collector.info()

    return {
        "status": status,
        "stdout": stdout_content.render(),
        "stderr": stderr_content.render(),
        "result": result_content.render(),
        "error": error_content
        if error_content is None or isinstance(error_content, str)
        else str(error_content),
        "bridge_truncation": bridge_truncation,
    }

# Patterns that indicate agent confusion about MCP tools
MCP_IMPORT_PATTERNS = [
    "from mcp",
    "import mcp",
    "from ses_s3_email",
    "import ses_s3_email",
    "from sendgrid_server",
    "import sendgrid_server",
    "from mailbux",
    "import mailbux",
    "from internal.tools",
    "import internal.tools",
]

def check_mcp_confusion(code):
    """Check if code appears to be trying to import MCP tools incorrectly."""
    code_lower = code.lower()
    for pattern in MCP_IMPORT_PATTERNS:
        if pattern.lower() in code_lower:
            return (
                "⚠️ WARNING: You appear to be trying to import MCP tools. "
                "MCP tools (search_emails, send_email, etc.) are ALREADY AVAILABLE "
                "as callable tools - just call them directly without importing. "
                "Use run_python only for data analysis (pandas, numpy, etc.).\n\n"
            )
    return None


def normalize_json_value(value):
    """Convert non-JSON-safe values into a serializable shape."""
    if value is None or isinstance(value, (bool, int, str)):
        return value
    if isinstance(value, bytes):
        return value.decode("utf-8", "replace")
    if isinstance(value, float) and not math.isfinite(value):
        return None
    if isinstance(value, float):
        return value
    if isinstance(value, (datetime.datetime, datetime.date, datetime.time)):
        return value.isoformat()
    if isinstance(value, dict):
        return {str(k): normalize_json_value(v) for k, v in value.items()}
    if isinstance(value, (list, tuple, set, frozenset)):
        return [normalize_json_value(v) for v in value]
    value_type = type(value)
    module_name = value_type.__module__
    type_name = value_type.__name__
    if module_name.startswith("pandas") and type_name in {"NAType", "NaTType"}:
        return None
    if str(value) in {"<NA>", "NaT"}:
        return None
    to_python = getattr(value, "to_pydatetime", None)
    if callable(to_python):
        try:
            return normalize_json_value(to_python())
        except Exception:
            pass
    item = getattr(value, "item", None)
    if callable(item):
        try:
            extracted = item()
            if extracted is not value:
                return normalize_json_value(extracted)
        except Exception:
            pass
    to_list = getattr(value, "tolist", None)
    if callable(to_list):
        try:
            return normalize_json_value(to_list())
        except Exception:
            pass
    isoformat = getattr(value, "isoformat", None)
    if callable(isoformat):
        try:
            return isoformat()
        except Exception:
            pass
    return value


def dump_json_line(value):
    """Serialize one JSON object line with non-finite floats normalized."""
    return json.dumps(normalize_json_value(value), allow_nan=False)


_kernel_cwd = None  # last cwd applied inside the kernel process


def execute_code(code, return_vars=None, timeout_seconds=None, workspace_dir=None):
    """Executes code on the kernel and returns the result."""
    global client, _kernel_cwd

    # Check for MCP import confusion
    confusion_warning = check_mcp_confusion(code)

    if client is None:
        try:
            cf = start_kernel()
            client = BlockingKernelClient(connection_file=cf)
            client.load_connection_file()
            client.start_channels()
        except Exception as e:
            return {
                "status": "error",
                "output": f"Failed to start kernel: {str(e)}",
                "error": str(e)
            }

    # Apply per-conversation workspace cwd INSIDE the kernel process.
    # The bridge's own cwd is already set by main(); the kernel is a
    # separate subprocess so we chdir it via a one-shot exec when the
    # workspace_dir differs from what we applied last.
    if workspace_dir and workspace_dir != _kernel_cwd:
        try:
            escaped = workspace_dir.replace("\\", "\\\\").replace("'", "\\'")
            chdir_code = (
                "import os as _os\n"
                f"_os.chdir('{escaped}')\n"
            )
            chdir_res = run_code_on_kernel(chdir_code, client)
            if chdir_res["status"] == "success":
                _kernel_cwd = workspace_dir
            else:
                sys.stderr.write(f"kernel chdir failed: {chdir_res.get('error')}\n")
        except Exception as e:
            sys.stderr.write(f"kernel chdir exception: {e}\n")

    try:
        # Execute the main code
        res = run_code_on_kernel(code, client, timeout_seconds=timeout_seconds)

        vars_data = {}
        if res["status"] == "success" and return_vars:
            # Round-trip extracted vars through a tempfile, not stdout: the
            # kernel's stdout is wrapped by OutputCollector (MAX_CAPTURE_BYTES,
            # 128KB) so a large JSON dump would truncate mid-stream and silently
            # drop every requested var. json.dumps() is a safe Python string
            # literal for the embedded path (any valid JSON string is a valid
            # Python string).
            fd, tmp_vars_file = tempfile.mkstemp(prefix="bridge-vars-", suffix=".json")
            os.close(fd)
            try:
                var_list_json = json.dumps(return_vars)
                tmp_path_literal = json.dumps(tmp_vars_file)
                extract_script = (
                    "import json\n"
                    "import math\n"
                    "import datetime\n"
                    f"_req_vars = {var_list_json}\n"
                    "def _normalize(_value):\n"
                    "    if _value is None or isinstance(_value, (bool, int, str)):\n"
                    "        return _value\n"
                    "    if isinstance(_value, bytes):\n"
                    "        return _value.decode('utf-8', 'replace')\n"
                    "    if isinstance(_value, float) and not math.isfinite(_value):\n"
                    "        return None\n"
                    "    if isinstance(_value, float):\n"
                    "        return _value\n"
                    "    if isinstance(_value, (datetime.datetime, datetime.date, datetime.time)):\n"
                    "        return _value.isoformat()\n"
                    "    if isinstance(_value, dict):\n"
                    "        return {str(_k): _normalize(_v) for _k, _v in _value.items()}\n"
                    "    if isinstance(_value, (list, tuple, set, frozenset)):\n"
                    "        return [_normalize(_v) for _v in _value]\n"
                    "    _module = type(_value).__module__\n"
                    "    _name = type(_value).__name__\n"
                    "    if _module.startswith('pandas') and _name in {'NAType', 'NaTType'}:\n"
                    "        return None\n"
                    "    if str(_value) in {'<NA>', 'NaT'}:\n"
                    "        return None\n"
                    "    _to_python = getattr(_value, 'to_pydatetime', None)\n"
                    "    if callable(_to_python):\n"
                    "        try:\n"
                    "            return _normalize(_to_python())\n"
                    "        except Exception:\n"
                    "            pass\n"
                    "    _item = getattr(_value, 'item', None)\n"
                    "    if callable(_item):\n"
                    "        try:\n"
                    "            _extracted = _item()\n"
                    "            if _extracted is not _value:\n"
                    "                return _normalize(_extracted)\n"
                    "        except Exception:\n"
                    "            pass\n"
                    "    _tolist = getattr(_value, 'tolist', None)\n"
                    "    if callable(_tolist):\n"
                    "        try:\n"
                    "            return _normalize(_tolist())\n"
                    "        except Exception:\n"
                    "            pass\n"
                    "    _iso = getattr(_value, 'isoformat', None)\n"
                    "    if callable(_iso):\n"
                    "        try:\n"
                    "            return _iso()\n"
                    "        except Exception:\n"
                    "            pass\n"
                    "    return str(_value)\n"
                    "_extracted = {}\n"
                    "for _v in _req_vars:\n"
                    "    if _v in locals(): _extracted[_v] = _normalize(locals()[_v])\n"
                    "    elif _v in globals(): _extracted[_v] = _normalize(globals()[_v])\n"
                    f"with open({tmp_path_literal}, 'w', encoding='utf-8') as _f:\n"
                    "    json.dump(_extracted, _f, default=str, allow_nan=False)\n"
                )

                var_res = run_code_on_kernel(extract_script, client)
                if var_res["status"] == "success":
                    try:
                        with open(tmp_vars_file, "r", encoding="utf-8") as f:
                            vars_data = json.load(f)
                    except Exception:
                        pass  # malformed/missing file → keep vars_data empty
            finally:
                try:
                    os.unlink(tmp_vars_file)
                except OSError:
                    pass

        # Consolidate legacy output field for backward compatibility
        final_output = ""
        # Prepend confusion warning if detected
        if confusion_warning:
            final_output += confusion_warning
        if res["stdout"]:
            final_output += res["stdout"]
        if res["stderr"]:
            final_output += "Stderr:\n" + res["stderr"] + "\n"
        if res["error"]:
            final_output += "Error:\n" + res["error"] + "\n"
        if res["result"]:
            final_output += "Result:\n" + res["result"]

        # Also prepend to stdout for structured output
        stdout_with_warning = (confusion_warning or "") + res["stdout"]

        return {
            "status": res["status"],
            "output": final_output.strip(), # Legacy field
            "stdout": stdout_with_warning,
            "stderr": res["stderr"],
            "vars": normalize_json_value(vars_data),
            "error": res["error"],
            "bridge_truncation": res.get("bridge_truncation", {}),
        }

    except Exception as e:
        return {
            "status": "error",
            "output": f"Execution error: {str(e)}",
            "error": str(e)
        }

def main():
    # Cleanup on SIGTERM/SIGINT (Go side's terminateBridge) AND on normal
    # exit (SystemExit, unhandled EOF on stdin). atexit fires even for
    # non-signal exits so the kernel connection file is always removed.
    signal.signal(signal.SIGINT, lambda s, f: cleanup())
    signal.signal(signal.SIGTERM, lambda s, f: cleanup())
    atexit.register(cleanup)

    try:
        # Read lines from stdin (each line is a JSON request)
        for line in sys.stdin:
            line = line.strip()
            if not line:
                continue

            try:
                req = json.loads(line)
                code = req.get("code", "")
                return_vars = req.get("return_vars", [])
                timeout_seconds = req.get("timeout_seconds") or None

                # Per-conversation cwd: Go side sends a scoped
                # workspace dir on every request. We ensure it exists,
                # chdir the bridge process itself (for bridge-side I/O
                # like open() in this script — not common, but safe),
                # and hand the path to execute_code which chdirs the
                # kernel subprocess on state change.
                workspace_dir = req.get("workspace_dir") or ""
                if workspace_dir:
                    try:
                        os.makedirs(workspace_dir, exist_ok=True)
                        os.chdir(workspace_dir)
                    except OSError as e:
                        sys.stderr.write(f"workspace_dir chdir failed: {e}\n")

                result = execute_code(code, return_vars, timeout_seconds=timeout_seconds, workspace_dir=workspace_dir)

                # Print result as JSON on one line
                print(dump_json_line(result), flush=True)

            except json.JSONDecodeError:
                print(dump_json_line({"status": "error", "output": "Invalid JSON input"}), flush=True)

    except KeyboardInterrupt:
        pass
    finally:
        cleanup()

if __name__ == "__main__":
    main()
