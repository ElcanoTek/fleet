#!/usr/bin/env python3
# fileops.py — the in-container executor for the sandboxed file-operation seam
# (#784). Fleet runs this via `podman exec -i <ctr> python3 -c <this>`, one shot
# per file operation, with a single JSON request on stdin and a single JSON
# response on stdout. It deliberately does NOT touch the run_python IPython
# bridge: no kernel boot, no serialization against run_python, and it works with
# the network sealed (lockdown).
#
# Byte-for-byte semantics match the pre-#784 host-side Go implementation so the
# tool layer's model-facing strings and limits are unchanged:
#   read  -> open('rb'), stat size, seek(offset), read(limit)
#   write -> makedirs(dir, 0o750), write to same-dir temp, os.replace, chmod 0o600
#   edit  -> read bytes, count occurrences, replace first|all, atomic replace, 0o600
#
# Response shape: {"ok": bool, "err_kind": "not_found"|"is_dir"|"old_absent"|"",
#                  "err": str, "data_b64": str, "size": int, "count": int}
import sys, os, json, base64, tempfile

def fail(kind, msg):
    return {"ok": False, "err_kind": kind, "err": msg, "data_b64": "", "size": 0, "count": 0}

def do_read(req):
    path = req["path"]
    if os.path.isdir(path):
        return fail("is_dir", "path is a directory")
    try:
        st = os.stat(path)
    except FileNotFoundError:
        return fail("not_found", "file not found")
    except OSError as e:
        return fail("", "stat failed: %s" % e)
    total = st.st_size
    offset = int(req.get("offset", 0) or 0)
    limit = int(req.get("limit", 0) or 0)
    try:
        with open(path, "rb") as f:
            if offset > 0:
                f.seek(offset)
            data = f.read(limit) if limit > 0 else f.read()
    except OSError as e:
        return fail("", "read failed: %s" % e)
    return {"ok": True, "err_kind": "", "err": "",
            "data_b64": base64.b64encode(data).decode("ascii"),
            "size": total, "count": 0}

def _atomic_write(path, data):
    d = os.path.dirname(path) or "."
    os.makedirs(d, 0o750, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=d, prefix=".fleet-fileop-")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
        os.chmod(path, 0o600)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise

def do_write(req):
    path = req["path"]
    if os.path.isdir(path):
        return fail("is_dir", "path is a directory")
    data = base64.b64decode(req.get("data_b64", ""))
    try:
        _atomic_write(path, data)
    except OSError as e:
        return fail("", "write failed: %s" % e)
    return {"ok": True, "err_kind": "", "err": "", "data_b64": "", "size": len(data), "count": 0}

def do_edit(req):
    path = req["path"]
    if os.path.isdir(path):
        return fail("is_dir", "path is a directory")
    try:
        with open(path, "rb") as f:
            content = f.read()
    except FileNotFoundError:
        return fail("not_found", "file not found")
    except OSError as e:
        return fail("", "read failed: %s" % e)
    old = base64.b64decode(req.get("old_b64", ""))
    new = base64.b64decode(req.get("new_b64", ""))
    count = content.count(old)
    if count == 0:
        return fail("old_absent", "old_text not found in file")
    if req.get("replace_all"):
        content = content.replace(old, new)
    else:
        content = content.replace(old, new, 1)
        count = 1
    try:
        _atomic_write(path, content)
    except OSError as e:
        return fail("", "write failed: %s" % e)
    return {"ok": True, "err_kind": "", "err": "", "data_b64": "", "size": 0, "count": count}

def main():
    try:
        req = json.loads(sys.stdin.read())
    except Exception as e:
        print(json.dumps(fail("", "bad request: %s" % e)))
        return
    op = req.get("op")
    handler = {"read": do_read, "write": do_write, "edit": do_edit}.get(op)
    if handler is None:
        print(json.dumps(fail("", "unknown op: %r" % op)))
        return
    try:
        print(json.dumps(handler(req)))
    except Exception as e:  # never crash without a JSON response the Go side can read
        print(json.dumps(fail("", "fileop failed: %s" % e)))

if __name__ == "__main__":
    main()
