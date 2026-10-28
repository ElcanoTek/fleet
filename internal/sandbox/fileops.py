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
# unified diff. #1745 gives write the same guard and makes an overwrite
# visible (the replaced file's hash, size and a bounded diff). "stream" lands
# a large download chunk by chunk in a hidden partial file and renames it into
# place without replacing anything. Those semantics must never regress the
# #784 confinement.
import base64
import contextlib
import difflib
import errno
import hashlib
import json
import os
import re
import secrets
import stat
import sys
import time


DIFF_MAX_BYTES = 4096
# A write is diffed against the file it replaces only when both are at most
# this size: the old file is held in memory for the diff, and difflib slows
# down sharply on large inputs. Larger files still report their hashes. An
# edit is diffed only when the lines around its replacements are at most this
# size a side.
DIFF_SOURCE_MAX_BYTES = 256 * 1024
# A write's diff covers only the span from the first changed line to the last,
# and an edit's only the lines around its replacements, each with
# DIFF_CONTEXT lines of context; either is skipped when those lines are over
# DIFF_MAX_LINES a side: difflib's cost grows with the square of the line
# count, and a diff that long would be cut to DIFF_MAX_BYTES anyway.
DIFF_MAX_LINES = 2000
DIFF_CONTEXT = 3
# A plain write (no expected_hash) hashes the file it replaces only up to this
# size. The hash is a courtesy report, and a file op that outlives the
# executor's timeout retires the sandbox, so a multi-GB file on slow storage
# must not turn a write that used to be instant into a failed turn. A write
# guarded by a hash always reads the file: the caller asked for the check.
REPORT_HASH_MAX_BYTES = 64 * 1024 * 1024
# expected_sha256 on a write may name no hash at all but this: the file must
# not exist yet (create-only).
EXPECT_ABSENT = "absent"


class UnsafePath(Exception):
    pass


class StaleContent(Exception):
    pass


class NotRegularFile(OSError):
    """The path names a FIFO, socket or device, not a regular file. Files are
    opened non-blocking and this is refused, so such an entry cannot stall
    the helper until the file-op timeout retires the sandbox."""

    def __init__(self):
        super().__init__(errno.EINVAL, "path is not a regular file")


APPEARED = (
    "a file appeared at this path while it was being written; view it, then retry"
)


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
        # O_NONBLOCK: a FIFO would otherwise block the open until a writer
        # came; on a regular file it changes nothing.
        fd = os.open(
            name,
            os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK,
            dir_fd=parent_fd,
        )
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
    if not stat.S_ISREG(info.st_mode):
        os.close(fd)
        raise NotRegularFile()
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


def _atomic_write(
    parent_fd, name, data, expected=None, expected_digest=None, expect_absent=False
):
    before = _lstat_at(parent_fd, name)
    if expected is not None and not _same_file(before, expected):
        raise UnsafePath("file changed while it was being edited")
    if expect_absent and before is not None:
        # The caller saw no file here and reports the write as a create; an
        # entry that appeared since must not be replaced unreported.
        raise StaleContent(APPEARED)
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
                # Only a write starts with no file (#1745); a file created
                # meanwhile is a concurrent change, not a path attack.
                raise StaleContent(APPEARED)
        elif not _same_file(before, current):
            raise UnsafePath("destination changed during write")
        if expected_digest is not None:
            try:
                current_digest = _sha256_open_at(parent_fd, name)
            except (FileNotFoundError, NotRegularFile) as exc:
                raise StaleContent("file changed before it was replaced") from exc
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


class FileTooLarge(OSError):
    """The file holds more than the request's max_size bytes."""

    def __init__(self, size, max_size):
        super().__init__(
            errno.EFBIG,
            "file is %s bytes, exceeding the %d-byte cap" % (size, max_size),
        )
        self.size = size


_READ_CHUNK = 3 << 18  # 768 KiB: a multiple of 3, so each chunk's base64 has no padding


class _ReadStream:
    """A read that has passed its open-time checks and streams its response.

    The helper never holds the file or its encoding: it reads fixed-size
    chunks, hashes each, base64-encodes the part inside the window (carrying at
    most 2 bytes between chunks so only the final piece is padded), and writes
    it straight to stdout. The response puts data_b64 FIRST and the status,
    size and hash after it, because they are only known at the end; a failure
    found mid-read (the file outgrew max_size, or changed) becomes an ok=false
    trailer, and Fleet ignores the partial data. Peak memory is O(chunk), so a
    64 MiB read fits a 128 MiB sandbox (ADR-0088).
    """

    def __init__(self, parent_fd, fd, before, offset, limit, max_size):
        self.parent_fd = parent_fd
        self.fd = fd
        self.before = before
        self.offset = offset
        self.limit = limit
        self.max_size = max_size

    def close(self):
        if self.fd >= 0:
            os.close(self.fd)
            self.fd = -1
        if self.parent_fd >= 0:
            os.close(self.parent_fd)
            self.parent_fd = -1

    def emit(self, out):
        try:
            out.write(b'{"data_b64":"')
            trailer = self._stream(out)
        except FileTooLarge as exc:
            trailer = fail("too_large", exc.strerror)
        except StaleContent as exc:
            trailer = fail("stale", str(exc))
        except OSError as exc:
            trailer = fail("", "read failed: %s" % exc)
        except Exception as exc:  # noqa: BLE001 - the trailer must still be valid JSON
            trailer = fail("", "fileop failed: %s" % exc)
        finally:
            self.close()
        trailer.pop("data_b64", None)
        # json.dumps of the trailer is '{...}'; splice its members after the
        # data string.
        body = json.dumps(trailer)
        out.write(b'",' + body[1:].encode("ascii") + b"\n")
        out.flush()

    def _stream(self, out):
        offset, limit, max_size = self.offset, self.limit, self.max_size
        if offset < 0 or limit < 0 or max_size < 0:
            raise OSError("offset, limit and max_size must be non-negative")
        digest = hashlib.sha256()
        carry = b""
        position = 0
        end = offset + limit if limit > 0 else None
        while True:
            want = _READ_CHUNK
            if max_size > 0:
                # The fstat size check before the read is not enough: a
                # process in the sandbox can grow the file while it is read
                # and restore its size and mtime before the after-read check.
                # Count what is actually read; never read past max_size + 1.
                want = min(want, max_size + 1 - position)
            chunk = os.read(self.fd, want)
            if not chunk:
                break
            digest.update(chunk)
            chunk_end = position + len(chunk)
            if max_size > 0 and chunk_end > max_size:
                raise FileTooLarge("more than %d" % max_size, max_size)
            wanted_start = max(position, offset)
            wanted_end = chunk_end if end is None else min(chunk_end, end)
            if wanted_start < wanted_end:
                piece = carry + chunk[wanted_start - position : wanted_end - position]
                whole = len(piece) - len(piece) % 3
                out.write(base64.b64encode(piece[:whole]))
                carry = piece[whole:]
            position = chunk_end
        if carry:
            out.write(base64.b64encode(carry))
        after = os.fstat(self.fd)
        before = self.before
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
            "size": before.st_size,
            "count": 0,
            "sha256": digest.hexdigest(),
        }


def do_read(req):
    parent_fd = -1
    fd = -1
    try:
        parent_fd, name = _open_parent(req, create=False)
        _test_pause(req, parent_fd)
        fd, before = _open_file(parent_fd, name)
        offset = int(req.get("offset", 0) or 0)
        limit = int(req.get("limit", 0) or 0)
        max_size = int(req.get("max_size", 0) or 0)
        if max_size > 0 and before.st_size > max_size:
            return fail(
                "too_large",
                "file is %d bytes, exceeding the %d-byte cap"
                % (before.st_size, max_size),
                size=before.st_size,
            )
        stream = _ReadStream(parent_fd, fd, before, offset, limit, max_size)
        parent_fd = fd = -1  # owned by the stream now
        return stream
    except FileNotFoundError:
        return fail("not_found", "file not found")
    except IsADirectoryError:
        return fail("is_dir", "path is a directory")
    except UnsafePath as exc:
        return fail("unsafe_path", str(exc))
    except StaleContent as exc:
        return fail("stale", str(exc))
    except NotRegularFile as exc:
        return fail("", exc.strerror)
    except OSError as exc:
        return fail("", "read failed: %s" % exc)
    finally:
        if fd >= 0:
            os.close(fd)
        if parent_fd >= 0:
            os.close(parent_fd)


def _expected_digest(req):
    expected = (req.get("expected_sha256") or "").strip().lower()
    if expected.startswith("sha256:"):
        expected = expected[len("sha256:") :]
    return expected


def _read_replaced(parent_fd, name, seen):
    """Hash the regular file a write is about to replace: (sha256, its bytes
    when small enough to diff or None, its size).

    Opened through the held parent dirfd, no-follow and non-blocking, and
    checked to be the entry lstat saw, so a symlink, FIFO or other file
    swapped in since cannot redirect the read, stall the helper, or be
    described in its place.
    """
    changed = "file changed while it was being written; retry"
    try:
        fd = os.open(
            name,
            os.O_RDONLY | os.O_NOFOLLOW | os.O_CLOEXEC | os.O_NONBLOCK,
            dir_fd=parent_fd,
        )
    except FileNotFoundError as exc:
        raise StaleContent(changed) from exc
    except OSError as exc:
        if exc.errno == errno.ELOOP:
            raise UnsafePath("file changed to a symlink") from exc
        raise
    try:
        info = os.fstat(fd)
        if not stat.S_ISREG(info.st_mode) or not _same_file(info, seen):
            raise StaleContent(changed)
        digest = hashlib.sha256()
        kept = []
        kept_size = 0
        while True:
            chunk = os.read(fd, 1 << 20)
            if not chunk:
                break
            digest.update(chunk)
            if kept is not None:
                kept_size += len(chunk)
                if kept_size > DIFF_SOURCE_MAX_BYTES:
                    kept = None
                else:
                    kept.append(chunk)
        after = os.fstat(fd)
        if (info.st_size, info.st_mtime_ns) != (after.st_size, after.st_mtime_ns):
            raise StaleContent("file changed while its hash was computed; retry")
        kept = b"".join(kept) if kept is not None else None
        return digest.hexdigest(), kept, info.st_size
    finally:
        os.close(fd)


def _write_diff(path, old, new):
    """The bounded diff of an overwrite: (diff, added, removed, note), where
    note says why there is no diff."""
    if old is None or len(new) > DIFF_SOURCE_MAX_BYTES:
        note = "no diff: a file is over %d KB" % (DIFF_SOURCE_MAX_BYTES // 1024)
        return "", 0, 0, note
    try:
        old.decode("utf-8")
        new.decode("utf-8")
    except UnicodeDecodeError:
        return "", 0, 0, "no diff: binary content"
    return _bounded_diff(path, old, new, "write", DIFF_MAX_LINES)


def _reason(exc):
    if isinstance(exc, OSError) and exc.strerror:
        return exc.strerror
    return str(exc)


def do_write(req):
    parent_fd = -1
    expected = _expected_digest(req)
    # guard: a hash the file must have now. "absent" refuses any existing
    # entry; no expected_hash replaces whatever is there, as before #1745.
    guard = expected not in ("", EXPECT_ABSENT)
    try:
        try:
            # A hash names a file that must already exist: a missing directory
            # means it is gone, and creating directories to say so would be a
            # write the caller did not ask for.
            parent_fd, name = _open_parent(req, create=not guard)
        except FileNotFoundError:
            if guard:
                return _write_stale_missing(expected)
            raise
        _test_pause(req, parent_fd)
        data = base64.b64decode(req.get("data_b64", ""))

        before = _lstat_at(parent_fd, name)
        old = None  # (sha256, bytes or None, size) of the file being replaced
        unread = None  # why an existing entry could not be described
        if before is not None:
            if not stat.S_ISREG(before.st_mode):
                unread = "the old entry was not a regular file"
            elif not guard and before.st_size > REPORT_HASH_MAX_BYTES:
                unread = (
                    "the old file is over %d MB, too large to hash for a report"
                    % (REPORT_HASH_MAX_BYTES // (1024 * 1024))
                )
            else:
                try:
                    old = _read_replaced(parent_fd, name, before)
                except (StaleContent, OSError) as exc:
                    if guard:
                        raise
                    reason = _reason(exc)
                    if reason.endswith("; retry"):  # the write goes ahead
                        reason = reason[: -len("; retry")]
                    unread = "the old file could not be read (%s)" % reason

        if expected == EXPECT_ABSENT and before is not None:
            if old is not None:
                what = "file already exists (sha256 %s, %d bytes)" % (old[0], old[2])
                how = "view it, then pass its sha256 as expected_hash to replace it"
            elif stat.S_ISREG(before.st_mode):
                what = "file already exists (%d bytes; %s)" % (before.st_size, unread)
                how = "write without expected_hash to replace it"
            else:
                what = "something other than a regular file (a FIFO, socket or device) exists at this path"
                how = "write without expected_hash to replace it"
            return fail(
                "stale",
                '%s; expected_hash "absent" only creates a new file — %s' % (what, how),
            )
        if guard:
            if before is None:
                return _write_stale_missing(expected)
            if old is None:
                return _stale(expected, "not a regular file")
            if old[0] != expected:
                return _stale(expected, old[0])

        # Describe the overwrite before committing it, so a slow diff can
        # never leave a write that landed reported as failed.
        response = {
            "ok": True,
            "err_kind": "",
            "err": "",
            "data_b64": "",
            "size": len(data),
            "count": 0,
            "sha256": hashlib.sha256(data).hexdigest(),
            "replaced": before is not None,
        }
        if old is not None:
            diff, added, removed, note = _write_diff(req["path"], old[1], data)
            response.update(
                old_sha256=old[0],
                old_size=old[2],
                diff=diff,
                added=added,
                removed=removed,
                diff_note=note,
            )
        elif before is not None:
            response.update(
                old_sha256="",
                old_size=before.st_size,
                diff_note="no hash or diff: " + unread,
            )

        try:
            _atomic_write(
                parent_fd,
                name,
                data,
                expected_digest=old[0] if guard else None,
                expect_absent=before is None,
            )
        except StaleContent as exc:
            if not guard or before is None:
                return fail("stale", str(exc))  # a file appeared meanwhile
            try:
                current = _sha256_open_at(parent_fd, name)
            except (OSError, UnsafePath, StaleContent):
                current = "unknown"
            return _stale(expected, current)
        return response
    except IsADirectoryError:
        return fail("is_dir", "path is a directory")
    except UnsafePath as exc:
        return fail("unsafe_path", str(exc))
    except StaleContent as exc:
        return fail("stale", str(exc))
    except OSError as exc:
        return fail("", "write failed: %s" % exc)
    finally:
        if parent_fd >= 0:
            os.close(parent_fd)


# A streamed write lands chunk by chunk in a hidden partial file and is then
# renamed into place without replacing anything. Only names of this exact form
# can be streamed, appended to, committed or aborted: the op can never extend
# or delete a file the agent made.
STREAM_PARTIAL_RE = re.compile(r"^\.fleet-download-[0-9a-f]{24}\.partial$")


# A partial file untouched this long belongs to no running download (a
# download's whole timeout is at most 30 minutes): the first chunk of a new
# streamed write removes such leftovers from its directory, e.g. after a
# cancelled turn retired the sandbox before the abort could run.
STREAM_PARTIAL_STALE_SECONDS = 3600


def _sweep_stale_partials(parent_fd, keep):
    now = time.time()
    with os.scandir(parent_fd) as entries:
        for entry in entries:
            if entry.name == keep or not STREAM_PARTIAL_RE.match(entry.name):
                continue
            try:
                info = os.stat(entry.name, dir_fd=parent_fd, follow_symlinks=False)
            except OSError:
                continue
            if (
                stat.S_ISREG(info.st_mode)
                and now - info.st_mtime > STREAM_PARTIAL_STALE_SECONDS
            ):
                with contextlib.suppress(OSError):
                    os.unlink(entry.name, dir_fd=parent_fd)


# linkat fails with these where the filesystem has no hard links (some RWX
# volumes); the commit then falls back to renameat2(RENAME_NOREPLACE).
_NO_HARDLINK_ERRNOS = {
    errno.EPERM,
    errno.EOPNOTSUPP,
    getattr(errno, "ENOTSUP", errno.EOPNOTSUPP),
    errno.EXDEV,
    errno.EMLINK,
    errno.ENOSYS,
}


_RENAME_NOREPLACE = 1  # linux/fs.h


def _renameat2_noreplace(parent_fd, name, final):
    """renameat2(RENAME_NOREPLACE): an atomic rename that fails with EEXIST
    instead of replacing. Raises OSError(ENOSYS) where libc or the kernel
    lacks it."""
    try:
        import ctypes
    except ImportError as exc:
        raise OSError(errno.ENOSYS, "ctypes not available") from exc

    libc = ctypes.CDLL(None, use_errno=True)
    fn = getattr(libc, "renameat2", None)
    if fn is None:
        raise OSError(errno.ENOSYS, "renameat2 not available")
    fn.argtypes = [
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_int,
        ctypes.c_char_p,
        ctypes.c_uint,
    ]
    if (
        fn(
            parent_fd,
            os.fsencode(name),
            parent_fd,
            os.fsencode(final),
            _RENAME_NOREPLACE,
        )
        != 0
    ):
        err = ctypes.get_errno()
        raise OSError(err, os.strerror(err))


def _commit_no_replace(parent_fd, name, final):
    """Rename name to final without ever replacing an existing entry; False
    when final exists. link + unlink where the filesystem has hard links,
    else renameat2(RENAME_NOREPLACE). Both are atomic about the existence of
    final; with neither available the commit fails rather than risk a
    replacing rename."""
    try:
        os.link(
            name,
            final,
            src_dir_fd=parent_fd,
            dst_dir_fd=parent_fd,
            follow_symlinks=False,
        )
    except FileExistsError:
        return False
    except OSError as exc:
        if exc.errno not in _NO_HARDLINK_ERRNOS:
            raise
        try:
            _renameat2_noreplace(parent_fd, name, final)
        except FileExistsError:
            return False
        except OSError as exc2:
            if exc2.errno in (errno.ENOSYS, errno.EINVAL, errno.EOPNOTSUPP):
                raise OSError(
                    exc2.errno,
                    "the workspace filesystem supports neither hard links nor "
                    "no-replace renames, so the download cannot be committed safely",
                ) from exc2
            raise
        return True
    os.unlink(name, dir_fd=parent_fd)
    return True


def _plain_name(name):
    return (
        bool(name) and name not in (".", "..") and "/" not in name and "\0" not in name
    )


def do_stream(req):
    """One step of a streamed write: append data_b64 at offset (0 creates the
    partial file), then optionally commit it to final or abort it."""
    parent_fd = -1
    fd = -1
    try:
        # Only the first chunk may create directories; an abort never does.
        # commit_only renames an already complete partial file (the caller
        # chose the final name after the last chunk landed). Only the first
        # chunk creates the partial file and its directories.
        first = (
            int(req.get("offset", 0)) == 0
            and not req.get("abort")
            and not req.get("commit_only")
        )
        parent_fd, name = _open_parent(req, create=first)
        if not STREAM_PARTIAL_RE.match(name):
            raise UnsafePath(
                "streamed writes only touch .fleet-download-*.partial files"
            )
        if req.get("abort"):
            if _lstat_at(parent_fd, name) is not None:
                os.unlink(name, dir_fd=parent_fd)
            return {"ok": True, "err_kind": "", "err": "", "data_b64": "", "size": 0}
        _test_pause(req, parent_fd)
        offset = int(req.get("offset", 0))
        data = base64.b64decode(req.get("data_b64", ""))
        if req.get("commit_only") and (data or not req.get("final")):
            return fail("", "commit_only takes a final name and no data")
        if first:
            fd = os.open(
                name,
                os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW | os.O_CLOEXEC,
                0o600,
                dir_fd=parent_fd,
            )
            _sweep_stale_partials(parent_fd, name)
        else:
            before = _lstat_at(parent_fd, name)
            if before is None:
                return fail("stale", "partial file is gone")
            fd = os.open(
                name,
                os.O_WRONLY
                | os.O_APPEND
                | os.O_NOFOLLOW
                | os.O_CLOEXEC
                | os.O_NONBLOCK,
                dir_fd=parent_fd,
            )
            info = os.fstat(fd)
            if not stat.S_ISREG(info.st_mode) or not _same_file(before, info):
                raise UnsafePath("partial file changed while it was being written")
            if info.st_size != offset:
                return fail(
                    "stale",
                    "partial file holds %d bytes, expected %d" % (info.st_size, offset),
                )
            # Fleet passes back the ctime the previous step reported: any
            # write, truncate, chmod or rename since then moves it, so bytes
            # changed between steps (even an overwrite of the same length)
            # are caught without hashing the file again.
            expected_ctime = int(req.get("expected_ctime_ns") or 0)
            if expected_ctime and info.st_ctime_ns != expected_ctime:
                return fail("stale", "partial file was changed by something else")
        view = memoryview(data)
        while view:
            written = os.write(fd, view)
            view = view[written:]
        written_info = os.fstat(fd)
        size = written_info.st_size
        if size != offset + len(data):
            # Something else wrote to the partial file: never commit it.
            return fail(
                "stale",
                "partial file holds %d bytes, expected %d" % (size, offset + len(data)),
            )
        final = req.get("final") or ""
        if final:
            os.fsync(fd)
        os.close(fd)
        fd = -1
        if final:
            if not _plain_name(final) or final.startswith(".fleet-"):
                raise UnsafePath("final name must be a plain file name")
            # The entry committed must still be the file just written, unchanged:
            # linkat never follows, so a swapped-in symlink would otherwise be
            # linked, and bytes appended since would be committed.
            current = _lstat_at(parent_fd, name)
            if not _same_file(current, written_info) or current.st_size != size:
                raise UnsafePath("partial file changed before it was committed")
            if current.st_ctime_ns != written_info.st_ctime_ns:
                return fail("stale", "partial file was changed by something else")
            # Optionally the bytes themselves: a full re-hash, which Fleet
            # does not ask for on large downloads (the ctime chain above
            # covers changes between steps without reading the file again).
            expected = _expected_digest(req)
            if expected:
                try:
                    staged = _sha256_open_at(parent_fd, name)
                except StaleContent as exc:
                    return fail("stale", str(exc))
                if staged != expected:
                    return fail(
                        "stale", "partial file content differs from the streamed body"
                    )
            if not _commit_no_replace(parent_fd, name, final):
                return fail("stale", "a file named %s appeared meanwhile" % final)
            committed = _lstat_at(parent_fd, final)
            if not _same_file(committed, written_info) or committed.st_size != size:
                raise UnsafePath("committed file changed during the rename")
            os.fsync(parent_fd)
        return {
            "ok": True,
            "err_kind": "",
            "err": "",
            "data_b64": "",
            "size": size,
            "ctime_ns": written_info.st_ctime_ns,
        }
    except FileExistsError:
        return fail("stale", "partial file already exists")
    except IsADirectoryError:
        return fail("is_dir", "path is a directory")
    except UnsafePath as exc:
        return fail("unsafe_path", str(exc))
    except OSError as exc:
        if exc.errno == errno.ELOOP:
            return fail("unsafe_path", "partial file changed to a symlink")
        return fail("", "stream write failed: %s" % exc)
    finally:
        if fd >= 0:
            os.close(fd)
        if parent_fd >= 0:
            os.close(parent_fd)


def _changed_window(old, new, context):
    """Drop the lines old and new share at the start and the end, keeping
    context lines around the change: (offset of the window, old, new)."""
    n = min(len(old), len(new))
    head = 0
    while head < n and old[head] == new[head]:
        head += 1
    tail = 0
    while tail < n - head and old[-1 - tail] == new[-1 - tail]:
        tail += 1
    start = max(0, head - context)
    cut = max(0, tail - context)
    return start, old[start : len(old) - cut], new[start : len(new) - cut]


def _lines(text):
    """text split after each "\n", so lines are numbered as grep -n and sed
    number them; str.splitlines would also split at \f, \v, a lone \r and
    more."""
    lines = text.split("\n")
    last = lines.pop()
    lines = [line + "\n" for line in lines]
    if last:
        lines.append(last)
    return lines


HUNK = re.compile(r"^@@ -(\d+)((?:,\d+)?) \+(\d+)((?:,\d+)?) @@")


def _shift_hunk(line, old_offset, new_offset):
    """Move a hunk header from window line numbers to the files'."""
    return HUNK.sub(
        lambda m: (
            "@@ -%d%s +%d%s @@"
            % (
                int(m.group(1)) + old_offset,
                m.group(2),
                int(m.group(3)) + new_offset,
                m.group(4),
            )
        ),
        line,
    )


def _hunks(before, after, old_offset, new_offset):
    """difflib's hunks between two runs of lines, numbered as lines of the
    files they were cut from, old_offset and new_offset lines in. A last line
    without a newline gets diff's marker, where difflib would run it into the
    next line of the diff."""
    lines = []
    for line in list(difflib.unified_diff(before, after, n=DIFF_CONTEXT))[2:]:
        if line.startswith("@@") and (old_offset or new_offset):
            line = _shift_hunk(line, old_offset, new_offset)
        elif not line.endswith("\n"):
            line += "\n\\ No newline at end of file\n"
        lines.append(line)
    return lines


def _diff_text(path, hunks, verb):
    """hunks as one unified diff cut to DIFF_MAX_BYTES: (diff, added, removed,
    note)."""
    if not hunks:
        return "", 0, 0, ""
    added = sum(1 for line in hunks if line.startswith("+"))
    removed = sum(1 for line in hunks if line.startswith("-"))
    name = os.path.basename(path)
    text = "--- %s (before)\n+++ %s (after)\n" % (name, name) + "".join(hunks)
    encoded = text.encode("utf-8")
    if len(encoded) > DIFF_MAX_BYTES:
        text = encoded[:DIFF_MAX_BYTES].decode("utf-8", "ignore")
        text += "\n... diff truncated (%s applied in full) ..." % verb
    return text, added, removed, ""


def _bounded_diff(path, before, after, verb="edit", max_lines=None):
    """A unified diff cut to DIFF_MAX_BYTES: (diff, added, removed, note).

    With max_lines, only the changed window is diffed, and a window longer
    than max_lines a side gets no diff, with a note saying so.
    """
    old_lines = _lines(before.decode("utf-8", "replace"))
    new_lines = _lines(after.decode("utf-8", "replace"))
    offset = 0
    if max_lines is not None:
        offset, old_lines, new_lines = _changed_window(
            old_lines, new_lines, DIFF_CONTEXT
        )
        if max(len(old_lines), len(new_lines)) > max_lines:
            return "", 0, 0, "no diff: the change spans over %d lines" % max_lines
    return _diff_text(path, _hunks(old_lines, new_lines, offset, offset), verb)


def _line_start(buf, pos, lines):
    """Where the line holding buf[pos] starts, moved back `lines` lines."""
    start = buf.rfind(b"\n", 0, pos) + 1
    for _ in range(lines):
        if start == 0:
            break
        start = buf.rfind(b"\n", 0, start - 1) + 1
    return start


def _line_end(buf, pos, lines):
    """Where the line holding buf[pos] ends, after its "\n", moved on `lines`
    lines."""
    for _ in range(lines + 1):
        pos = buf.find(b"\n", pos)
        if pos < 0:
            return len(buf)
        pos += 1
    return pos


def _line_count(buf, start, end):
    """The lines in buf[start:end], counted as _lines counts them."""
    return buf.count(b"\n", start, end) + (end > start and buf[end - 1] != 0x0A)


def _edit_diff(path, before, after, old, new, count):
    """edit_file's bounded diff: (diff, added, removed, note).

    The edit knows where it replaced old with new, so it diffs only the lines
    around its replacements and never decodes the whole file: a replace_all
    rename through a long file keeps its diff, and a large file costs little
    more memory than the edit itself. When those lines come to over
    DIFF_SOURCE_MAX_BYTES or DIFF_MAX_LINES a side, or are not UTF-8, there
    is a note instead.
    """
    if not old:
        return _bounded_diff(path, before, after, "edit", DIFF_MAX_LINES)
    grow = len(new) - len(old)
    # [start, end, replacements before it] of each window, in before's bytes.
    # A window holds whole lines: its replacements, with DIFF_CONTEXT lines
    # around them. Windows that touch are merged, so each window maps to
    # after by shifting it by grow for every replacement ahead of it.
    # The search for old runs once through the file, as count and replace
    # already did. The line scans touch each line a bounded number of times,
    # however many replacements it holds, and only bytes that join a window,
    # so the size check below bounds them too.
    windows = []
    size = 0
    line_end = 0  # the end of the line where the last replacement ends
    at = -len(old)
    for i in range(count):
        at = before.find(old, at + len(old))
        last = at + len(old) - 1
        if windows and at < windows[-1][1]:
            start = windows[-1][0]
        else:
            start = _line_start(before, at, DIFF_CONTEXT)
        if last < line_end:
            end = windows[-1][1]  # it ends on the same line as the last one
        else:
            line_end = _line_end(before, last, 0)
            end = _line_end(before, last, DIFF_CONTEXT)
        if windows and start <= windows[-1][1]:
            size += end - windows[-1][1]
            windows[-1][1] = end
        else:
            size += end - start
            windows.append([start, end, i])
        if max(size, size + (i + 1) * grow) > DIFF_SOURCE_MAX_BYTES:
            note = "no diff: the changes, with context, are over %d KB" % (
                DIFF_SOURCE_MAX_BYTES // 1024
            )
            return "", 0, 0, note

    spans = []  # (start, end) of each window in before, then in after
    for n, (start, end, ahead) in enumerate(windows):
        through = windows[n + 1][2] if n + 1 < len(windows) else count
        spans.append((start, end, start + ahead * grow, end + through * grow))
    old_lines = sum(_line_count(before, a, b) for a, b, _, _ in spans)
    new_lines = sum(_line_count(after, c, d) for _, _, c, d in spans)
    if max(old_lines, new_lines) > DIFF_MAX_LINES:
        note = "no diff: the changes, with context, are over %d lines" % DIFF_MAX_LINES
        return "", 0, 0, note

    hunks = []
    old_line = new_line = old_at = new_at = 0
    try:
        for old_start, old_end, new_start, new_end in spans:
            old_line += before.count(b"\n", old_at, old_start)
            new_line += after.count(b"\n", new_at, new_start)
            old_at, new_at = old_start, new_start
            hunks += _hunks(
                _lines(before[old_start:old_end].decode("utf-8")),
                _lines(after[new_start:new_end].decode("utf-8")),
                old_line,
                new_line,
            )
    except UnicodeDecodeError:
        return "", 0, 0, "no diff: binary content"
    return _diff_text(path, hunks, "edit")


def _stale(expected, current):
    return fail(
        "stale",
        "file content has changed since it was last read "
        "(expected sha256 %s, current %s); re-read the file and retry"
        % (expected, current),
    )


def _write_stale_missing(expected):
    return fail(
        "stale",
        "file does not exist (expected sha256 %s): it was moved or deleted since "
        'it was last read — check the path, or pass expected_hash "absent" to '
        "create it" % expected,
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
        expected = _expected_digest(req)
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

        # Describe the edit before committing it, as a write does: a
        # replace_all through a long file would otherwise diff past the
        # file-op timeout after the edit had landed.
        diff, added, removed, note = _edit_diff(
            req["path"], content, updated, old, new, count
        )
        response = {
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
            "diff_note": note,
        }

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
        return response
    except FileNotFoundError:
        return fail("not_found", "file not found")
    except IsADirectoryError:
        return fail("is_dir", "path is a directory")
    except UnsafePath as exc:
        return fail("unsafe_path", str(exc))
    except NotRegularFile as exc:
        return fail("", exc.strerror)
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
        "stream": do_stream,
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
    _emit(response)


def _emit(response):
    """Write the response: a streamed read, or one JSON document."""
    if isinstance(response, _ReadStream):
        response.emit(sys.stdout.buffer)
        return
    print(json.dumps(response))


if __name__ == "__main__":
    main()
