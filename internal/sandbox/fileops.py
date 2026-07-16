#!/usr/bin/env python3
# fileops.py — the in-container executor for the sandboxed file-operation seam
# (#784). Fleet runs this via `podman exec -i <ctr> python3 -c <this>`, one shot
# per file operation, with a single JSON request on stdin and a single JSON
# response on stdout. It deliberately does NOT touch the run_python IPython
# bridge: no kernel boot, no serialization against run_python, and it works with
# the network sealed (lockdown).
#
# Semantics:
#   read  -> stat size, seek(offset), read(limit); returns the window + the
#            FULL-file sha256 (the version handle for edit_file, #787).
#   write -> makedirs(dir, 0o750), write to same-dir temp, os.replace,
#            preserving an existing file's mode (0600 on create); returns sha256.
#   edit  -> read bytes; #787 safety: reject an ambiguous match (>1 and not
#            replace_all) and a no-op, honor an optional expected_sha256 stale
#            guard, then atomic replace; returns old/new sha256, replaced count,
#            +/- line counts, and a bounded unified diff.
#
# Response shape (fields absent-as-zero on the Go side):
#   {"ok": bool, "err_kind": "not_found"|"is_dir"|"old_absent"|"ambiguous"|
#                            "stale"|"noop"|"", "err": str, "data_b64": str,
#    "size": int, "count": int, "sha256": str, "old_sha256": str,
#    "match_count": int, "added": int, "removed": int, "diff": str, "hint": str}
import sys, os, json, base64, tempfile, hashlib, difflib

DIFF_MAX_BYTES = 4096


def fail(kind, msg, **extra):
    r = {"ok": False, "err_kind": kind, "err": msg, "data_b64": "", "size": 0, "count": 0}
    r.update(extra)
    return r


def _sha256_file(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


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
        digest = _sha256_file(path)
    except OSError as e:
        return fail("", "read failed: %s" % e)
    return {"ok": True, "err_kind": "", "err": "",
            "data_b64": base64.b64encode(data).decode("ascii"),
            "size": total, "count": 0, "sha256": digest}


def _atomic_write(path, data):
    # Preserve an existing file's mode on overwrite/edit; default 0600 only when
    # creating a new file. os.replace() gives the target a NEW inode, so without
    # this the file's mode would reset to the mkstemp default (0600) on every
    # overwrite — dropping e.g. a +x bit an agent had chmod'd.
    mode = 0o600
    try:
        mode = os.stat(path).st_mode & 0o777
    except FileNotFoundError:
        pass
    d = os.path.dirname(path) or "."
    os.makedirs(d, 0o750, exist_ok=True)
    fd, tmp = tempfile.mkstemp(dir=d, prefix=".fleet-fileop-")
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(data)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp, mode)
        os.replace(tmp, path)
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
    return {"ok": True, "err_kind": "", "err": "", "data_b64": "", "size": len(data),
            "count": 0, "sha256": hashlib.sha256(data).hexdigest()}


def _bounded_diff(path, before, after):
    # Unified diff of the decoded text (lossy-safe: decode utf-8 with replacement
    # only for the human-readable diff; the actual edit is byte-exact). Bounded so
    # a huge edit can't blow the model-visible response.
    b = before.decode("utf-8", "replace").splitlines(keepends=True)
    a = after.decode("utf-8", "replace").splitlines(keepends=True)
    name = os.path.basename(path)
    lines = list(difflib.unified_diff(b, a, fromfile=name + " (before)", tofile=name + " (after)", n=3))
    added = sum(1 for ln in lines if ln.startswith("+") and not ln.startswith("+++"))
    removed = sum(1 for ln in lines if ln.startswith("-") and not ln.startswith("---"))
    text = "".join(lines)
    if len(text.encode("utf-8")) > DIFF_MAX_BYTES:
        text = text.encode("utf-8")[:DIFF_MAX_BYTES].decode("utf-8", "ignore")
        text += "\n... diff truncated (edit applied in full) ..."
    return text, added, removed


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

    old_digest = hashlib.sha256(content).hexdigest()
    expected = (req.get("expected_sha256") or "").strip().lower()
    if expected:
        if expected.startswith("sha256:"):
            expected = expected[len("sha256:"):]
        if expected != old_digest:
            return fail("stale",
                        "file content has changed since it was last read "
                        "(expected sha256 %s, current %s); re-read the file and retry" % (expected, old_digest))

    old = base64.b64decode(req.get("old_b64", ""))
    new = base64.b64decode(req.get("new_b64", ""))
    count = content.count(old)
    if count == 0:
        r = fail("old_absent", "old_text not found in file")
        # CRLF diagnostic: matches after normalizing line endings? Tell the model.
        if b"\r\n" in content and old and old.replace(b"\r\n", b"\n") in content.replace(b"\r\n", b"\n"):
            r["hint"] = ("the file uses CRLF line endings — include \\r\\n in old_text exactly "
                         "as view_file returned it, or re-read the file")
        return r
    if count > 1 and not req.get("replace_all"):
        return fail("ambiguous",
                    "old_text matches %d locations; edit_file replaces exactly one — add surrounding "
                    "context to make the match unique, or set replace_all=true" % count,
                    match_count=count)

    if req.get("replace_all"):
        updated = content.replace(old, new)
    else:
        updated = content.replace(old, new, 1)
        count = 1
    if updated == content:
        return fail("noop", "edit is a no-op (old_text and new_text produce identical content)")

    try:
        _atomic_write(path, updated)
    except OSError as e:
        return fail("", "write failed: %s" % e)
    diff, added, removed = _bounded_diff(path, content, updated)
    return {"ok": True, "err_kind": "", "err": "", "data_b64": "", "size": len(updated),
            "count": count, "sha256": hashlib.sha256(updated).hexdigest(), "old_sha256": old_digest,
            "match_count": count, "added": added, "removed": removed, "diff": diff}


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
