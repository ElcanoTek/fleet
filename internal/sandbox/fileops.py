#!/usr/bin/env python3
# fileops.py — one-shot in-container executor for Sandbox.RunFileOp (#784).
#
# Security boundary: the host supplies a narrow policy root and the Go backend
# supplies the trusted bind-mount anchor containing it. The helper opens that
# anchor, verifies the turn-bound root identity, then traverses every component
# with directory descriptors plus O_NOFOLLOW. Reads and atomic replacements are
# descriptor-relative, so a symlink or directory exchange cannot redirect the
# operation outside the declared capability.
#
# #787 layers ambiguity-safe and stale-safe edit semantics on this same seam:
# full-file hashes, optional expected_sha256, no-op rejection, and a bounded
# unified diff. Those semantics must never regress the #784 confinement.
import base64
import contextlib
import difflib
import errno
import hashlib
import json
import os
import secrets
import stat
import sys
import time


DIFF_MAX_BYTES = 4096


class UnsafePath(Exception):
    pass


class StaleContent(Exception):
    pass


def fail(kind, msg, **extra):
    response = {
        "ok": False,
        "err_kind": kind,
        "err": msg,
        "data_b64": "",
        "size": 0,
        "count": 0,
    }
    response.update(extra)
    return response


def _parts_beneath(base, child):
    if not isinstance(base, str) or not isinstance(child, str):
        raise UnsafePath("path and root must be strings")
    if not os.path.isabs(base) or not os.path.isabs(child):
        raise UnsafePath("path and root must be absolute")
    base = os.path.normpath(base)
    child = os.path.normpath(child)
    rel = os.path.relpath(child, base)
    if rel == ".":
        return []
    parts = rel.split(os.sep)
    if os.path.isabs(rel) or any(part in ("", ".", "..") for part in parts):
        raise UnsafePath("path escapes its declared root")
    return parts


def _open_dir_at(parent_fd, name, create):
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
    try:
        return os.open(name, flags, dir_fd=parent_fd)
    except FileNotFoundError:
        if not create:
            raise
        created = False
        try:
            os.mkdir(name, 0o750, dir_fd=parent_fd)
            created = True
        except FileExistsError:
            pass
        try:
            fd = os.open(name, flags, dir_fd=parent_fd)
            if created:
                # mkdir is umask-filtered; the file-tool contract is exact 0750.
                # nosemgrep: python.lang.security.audit.insecure-file-permissions.insecure-file-permissions -- the rule advises 0o644, i.e. WORLD-READABLE, for a sandbox directory. Following it would be a security regression. 0750 is the file-tool contract and is deliberately tighter than the suggestion.
                os.fchmod(fd, 0o750)
            return fd
        except OSError as exc:
            if exc.errno in (errno.ELOOP, errno.ENOTDIR):
                raise UnsafePath("directory component changed or is a symlink") from exc
            raise
    except OSError as exc:
        if exc.errno in (errno.ELOOP, errno.ENOTDIR):
            raise UnsafePath(
                "directory component is a symlink or not a directory"
            ) from exc
        raise


def _walk_dirs(start_fd, parts, create=False):
    current = os.dup(start_fd)
    try:
        for component in parts:
            nxt = _open_dir_at(current, component, create)
            os.close(current)
            current = nxt
        return current
    except BaseException:
        os.close(current)
        raise


def _open_root(req):
    anchor = os.path.normpath(req["anchor"])
    root = os.path.normpath(req["root"])
    root_parts = _parts_beneath(anchor, root)
    flags = os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC
    try:
        anchor_fd = os.open(anchor, flags)
    except OSError as exc:
        if exc.errno in (errno.ELOOP, errno.ENOTDIR):
            raise UnsafePath("trusted mount anchor is not a directory") from exc
        raise
    try:
        root_fd = _walk_dirs(anchor_fd, root_parts, create=False)
    finally:
        os.close(anchor_fd)

    expected_dev = req.get("expected_dev")
    expected_ino = req.get("expected_ino")
    if expected_dev is not None or expected_ino is not None:
        if expected_dev is None or expected_ino is None:
            os.close(root_fd)
            raise UnsafePath("bound workspace root identity is incomplete")
        info = os.fstat(root_fd)
        if info.st_dev != int(expected_dev) or info.st_ino != int(expected_ino):
            os.close(root_fd)
            raise UnsafePath("bound workspace root identity changed")
    return root_fd


def _open_parent(req, create=False):
    root = os.path.normpath(req["root"])
    path = os.path.normpath(req["path"])
    path_parts = _parts_beneath(root, path)
    if not path_parts:
        raise UnsafePath("operation path must name a file below its root")

    root_fd = _open_root(req)
    try:
        parent_fd = _walk_dirs(root_fd, path_parts[:-1], create=create)
    finally:
        os.close(root_fd)
    return parent_fd, path_parts[-1]


def do_bind_root(req):
    root_fd = -1
    try:
        root_fd = _open_root(req)
        info = os.fstat(root_fd)
        return {
            "ok": True,
            "err_kind": "",
            "err": "",
            "data_b64": "",
            "size": 0,
            "count": 0,
            "dev": info.st_dev,
            "ino": info.st_ino,
        }
    except UnsafePath as exc:
        return fail("unsafe_path", str(exc))
    except OSError as exc:
        return fail("", "bind root failed: %s" % exc)
    finally:
        if root_fd >= 0:
            os.close(root_fd)


def _test_pause(req, parent_fd):
    """Deterministic package-private rendezvous, unreachable by tool input."""
    pause_ms = int(req.get("test_pause_ms", 0) or 0)
    if pause_ms <= 0:
        return
    name = req.get("test_ready_name", "")
    if (
        not isinstance(name, str)
        or not name.startswith(".fleet-fileop-test-")
        or os.path.basename(name) != name
    ):
        raise UnsafePath("invalid test rendezvous name")
    fd = os.open(
        name,
        os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC,
        0o600,
        dir_fd=parent_fd,
    )
    os.close(fd)
    os.fsync(parent_fd)
    time.sleep(min(pause_ms, 10000) / 1000.0)


def _open_file(parent_fd, name):
    try:
        fd = os.open(name, os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC, dir_fd=parent_fd)
    except FileNotFoundError:
        raise
    except IsADirectoryError:
        raise
    except OSError as exc:
        if exc.errno == errno.ELOOP:
            raise UnsafePath("file changed to a symlink") from exc
        raise
    info = os.fstat(fd)
    if stat.S_ISDIR(info.st_mode):
        os.close(fd)
        raise IsADirectoryError(name)
    return fd, info


def _lstat_at(parent_fd, name):
    try:
        info = os.stat(name, dir_fd=parent_fd, follow_symlinks=False)
    except FileNotFoundError:
        return None
    if stat.S_ISLNK(info.st_mode):
        raise UnsafePath("destination changed to a symlink")
    if stat.S_ISDIR(info.st_mode):
        raise IsADirectoryError(name)
    return info


def _same_file(left, right):
    return (
        left is not None
        and right is not None
        and (left.st_dev, left.st_ino) == (right.st_dev, right.st_ino)
    )


def _sha256_open_at(parent_fd, name):
    fd = -1
    try:
        fd, info = _open_file(parent_fd, name)
        digest = hashlib.sha256()
        while True:
            chunk = os.read(fd, 1 << 20)
            if not chunk:
                break
            digest.update(chunk)
        after = os.fstat(fd)
        if (info.st_dev, info.st_ino, info.st_size, info.st_mtime_ns) != (
            after.st_dev,
            after.st_ino,
            after.st_size,
            after.st_mtime_ns,
        ):
            raise StaleContent("file changed while its hash was computed")
        return digest.hexdigest()
    finally:
        if fd >= 0:
            os.close(fd)


def _atomic_write(parent_fd, name, data, expected=None, expected_digest=None):
    before = _lstat_at(parent_fd, name)
    if expected is not None and not _same_file(before, expected):
        raise UnsafePath("file changed while it was being edited")
    mode = stat.S_IMODE(before.st_mode) if before is not None else 0o600
    owner = (before.st_uid, before.st_gid) if before is not None else None

    tmp = ".fleet-fileop-%s" % secrets.token_hex(12)
    fd = -1
    try:
        fd = os.open(
            tmp,
            os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC,
            0o600,
            dir_fd=parent_fd,
        )
        view = memoryview(data)
        while view:
            written = os.write(fd, view)
            view = view[written:]
        if owner is not None:
            current_owner = os.fstat(fd)
            if (current_owner.st_uid, current_owner.st_gid) != owner:
                # Best effort: preserving the destination's ownership on an
                # overwrite is a nicety, not a safety property. The executor
                # runs unprivileged and cannot chown to a foreign uid (e.g.
                # a host-seeded workspace file mapping to container-root), so
                # keep the executor-owned replacement rather than aborting a
                # legitimate edit.
                with contextlib.suppress(PermissionError):
                    os.fchown(fd, owner[0], owner[1])
        os.fchmod(fd, mode)
        os.fsync(fd)
        os.close(fd)
        fd = -1

        # Re-check the entry immediately before commit. Descriptor-relative
        # replace never follows the destination, even if a final swap races it.
        current = _lstat_at(parent_fd, name)
        if before is None:
            if current is not None:
                raise UnsafePath("destination appeared during write")
        elif not _same_file(before, current):
            raise UnsafePath("destination changed during write")
        if expected_digest is not None:
            current_digest = _sha256_open_at(parent_fd, name)
            if current_digest != expected_digest:
                raise StaleContent("file content changed before the edit was committed")
        os.replace(tmp, name, src_dir_fd=parent_fd, dst_dir_fd=parent_fd)
        os.fsync(parent_fd)
    except BaseException:
        if fd >= 0:
            os.close(fd)
        with contextlib.suppress(OSError):
            os.unlink(tmp, dir_fd=parent_fd)
        raise


def _hash_and_window(fd, offset, limit):
    if offset < 0 or limit < 0:
        raise OSError("offset and limit must be non-negative")
    digest = hashlib.sha256()
    chunks = []
    position = 0
    end = offset + limit if limit > 0 else None
    while True:
        chunk = os.read(fd, 1 << 20)
        if not chunk:
            break
        digest.update(chunk)
        chunk_end = position + len(chunk)
        wanted_start = max(position, offset)
        wanted_end = chunk_end if end is None else min(chunk_end, end)
        if wanted_start < wanted_end:
            chunks.append(chunk[wanted_start - position : wanted_end - position])
        position = chunk_end
    return digest.hexdigest(), b"".join(chunks)


def do_read(req):
    parent_fd = -1
    fd = -1
    try:
        parent_fd, name = _open_parent(req, create=False)
        _test_pause(req, parent_fd)
        fd, before = _open_file(parent_fd, name)
        offset = int(req.get("offset", 0) or 0)
        limit = int(req.get("limit", 0) or 0)
        digest, data = _hash_and_window(fd, offset, limit)
        after = os.fstat(fd)
        if (before.st_dev, before.st_ino, before.st_size, before.st_mtime_ns) != (
            after.st_dev,
            after.st_ino,
            after.st_size,
            after.st_mtime_ns,
        ):
            raise StaleContent("file changed while it was being read")
        return {
            "ok": True,
            "err_kind": "",
            "err": "",
            "data_b64": base64.b64encode(data).decode("ascii"),
            "size": before.st_size,
            "count": 0,
            "sha256": digest,
        }
    except FileNotFoundError:
        return fail("not_found", "file not found")
    except IsADirectoryError:
        return fail("is_dir", "path is a directory")
    except UnsafePath as exc:
        return fail("unsafe_path", str(exc))
    except StaleContent as exc:
        return fail("stale", str(exc))
    except OSError as exc:
        return fail("", "read failed: %s" % exc)
    finally:
        if fd >= 0:
            os.close(fd)
        if parent_fd >= 0:
            os.close(parent_fd)


def do_write(req):
    parent_fd = -1
    try:
        parent_fd, name = _open_parent(req, create=True)
        _test_pause(req, parent_fd)
        data = base64.b64decode(req.get("data_b64", ""))
        _atomic_write(parent_fd, name, data)
        return {
            "ok": True,
            "err_kind": "",
            "err": "",
            "data_b64": "",
            "size": len(data),
            "count": 0,
            "sha256": hashlib.sha256(data).hexdigest(),
        }
    except IsADirectoryError:
        return fail("is_dir", "path is a directory")
    except UnsafePath as exc:
        return fail("unsafe_path", str(exc))
    except OSError as exc:
        return fail("", "write failed: %s" % exc)
    finally:
        if parent_fd >= 0:
            os.close(parent_fd)


def _bounded_diff(path, before, after):
    old_lines = before.decode("utf-8", "replace").splitlines(keepends=True)
    new_lines = after.decode("utf-8", "replace").splitlines(keepends=True)
    name = os.path.basename(path)
    lines = list(
        difflib.unified_diff(
            old_lines,
            new_lines,
            fromfile=name + " (before)",
            tofile=name + " (after)",
            n=3,
        )
    )
    added = sum(
        1 for line in lines if line.startswith("+") and not line.startswith("+++")
    )
    removed = sum(
        1 for line in lines if line.startswith("-") and not line.startswith("---")
    )
    text = "".join(lines)
    encoded = text.encode("utf-8")
    if len(encoded) > DIFF_MAX_BYTES:
        text = encoded[:DIFF_MAX_BYTES].decode("utf-8", "ignore")
        text += "\n... diff truncated (edit applied in full) ..."
    return text, added, removed


def _stale(expected, current):
    return fail(
        "stale",
        "file content has changed since it was last read "
        "(expected sha256 %s, current %s); re-read the file and retry"
        % (expected, current),
    )


def do_edit(req):
    parent_fd = -1
    fd = -1
    try:
        parent_fd, name = _open_parent(req, create=False)
        _test_pause(req, parent_fd)
        fd, info = _open_file(parent_fd, name)
        with os.fdopen(fd, "rb") as stream:
            fd = -1
            content = stream.read()

        old_digest = hashlib.sha256(content).hexdigest()
        expected = (req.get("expected_sha256") or "").strip().lower()
        if expected.startswith("sha256:"):
            expected = expected[len("sha256:") :]
        if expected and expected != old_digest:
            return _stale(expected, old_digest)

        old = base64.b64decode(req.get("old_b64", ""))
        new = base64.b64decode(req.get("new_b64", ""))
        count = content.count(old)
        if count == 0:
            response = fail("old_absent", "old_text not found in file")
            if (
                b"\r\n" in content
                and old
                and old.replace(b"\r\n", b"\n") in content.replace(b"\r\n", b"\n")
            ):
                response["hint"] = (
                    "the file uses CRLF line endings — include \\r\\n in "
                    "old_text exactly as view_file returned it, or re-read "
                    "the file"
                )
            return response
        if count > 1 and not req.get("replace_all"):
            return fail(
                "ambiguous",
                "old_text matches %d locations; edit_file "
                "replaces exactly one — add surrounding context to make the "
                "match unique, or set replace_all=true" % count,
                match_count=count,
            )

        if req.get("replace_all"):
            updated = content.replace(old, new)
        else:
            updated = content.replace(old, new, 1)
            count = 1
        if updated == content:
            return fail(
                "noop",
                "edit is a no-op (old_text and new_text produce identical content)",
            )

        try:
            _atomic_write(
                parent_fd,
                name,
                updated,
                expected=info,
                expected_digest=old_digest if expected else None,
            )
        except StaleContent:
            try:
                current = _sha256_open_at(parent_fd, name)
            except (OSError, UnsafePath, StaleContent):
                current = "unknown"
            return _stale(expected, current)

        diff, added, removed = _bounded_diff(req["path"], content, updated)
        return {
            "ok": True,
            "err_kind": "",
            "err": "",
            "data_b64": "",
            "size": len(updated),
            "count": count,
            "sha256": hashlib.sha256(updated).hexdigest(),
            "old_sha256": old_digest,
            "match_count": count,
            "added": added,
            "removed": removed,
            "diff": diff,
        }
    except FileNotFoundError:
        return fail("not_found", "file not found")
    except IsADirectoryError:
        return fail("is_dir", "path is a directory")
    except UnsafePath as exc:
        return fail("unsafe_path", str(exc))
    except OSError as exc:
        return fail("", "edit failed: %s" % exc)
    finally:
        if fd >= 0:
            os.close(fd)
        if parent_fd >= 0:
            os.close(parent_fd)


def main():
    try:
        req = json.loads(sys.stdin.read())
    except Exception as exc:
        print(json.dumps(fail("", "bad request: %s" % exc)))
        return
    handler = {
        "read": do_read,
        "write": do_write,
        "edit": do_edit,
        "bind_root": do_bind_root,
    }.get(req.get("op"))
    if handler is None:
        print(json.dumps(fail("", "unknown op: %r" % req.get("op"))))
        return
    try:
        response = handler(req)
    except UnsafePath as exc:
        response = fail("unsafe_path", str(exc))
    except Exception as exc:
        response = fail("", "fileop failed: %s" % exc)
    print(json.dumps(response))


if __name__ == "__main__":
    main()
