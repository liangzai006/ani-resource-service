"""Sandbox workspace operations. The caller must supply a trusted /workspace mount.

All names are resolved by Linux openat2 relative to the workspace mount; no
symlink or nested mount is followed. Mutations publish a new inode rather than
truncating an existing one, and renameat2 prevents overwrite races.
"""
import base64
import ctypes
import errno
import json
import os
import secrets
import stat
import sys

MAX_BYTES = 1 << 20
MAX_ENTRIES = 10000
RESOLVE_NO_XDEV = 0x01
RESOLVE_NO_MAGICLINKS = 0x02
RESOLVE_NO_SYMLINKS = 0x04
RESOLVE_BENEATH = 0x08
SYS_OPENAT2 = 437
SYS_RENAMEAT2 = {"x86_64": 316, "aarch64": 276}.get(os.uname().machine)
libc = ctypes.CDLL(None, use_errno=True)

class OpenHow(ctypes.Structure):
    _fields_ = [("flags", ctypes.c_uint64), ("mode", ctypes.c_uint64), ("resolve", ctypes.c_uint64)]

def syscall(number, *args):
    if number is None:
        raise OSError(errno.ENOSYS, "unsupported system architecture")
    result = libc.syscall(number, *args)
    if result < 0:
        code = ctypes.get_errno()
        raise OSError(code, os.strerror(code))
    return result

def valid_path(path, root_ok=False):
    if not isinstance(path, str) or len(path.encode("utf-8")) > 4096 or "\x00" in path or "\\" in path or path.startswith("/"):
        raise ValueError("invalid path")
    if root_ok and path in ("", "."):
        return "."
    if any(part in ("", ".", "..") or len(part.encode("utf-8")) > 255 for part in path.split("/")):
        raise ValueError("invalid path")
    return path

def open_beneath(root_fd, path, flags, mode=0):
    how = OpenHow(flags, mode, RESOLVE_NO_XDEV | RESOLVE_NO_MAGICLINKS | RESOLVE_NO_SYMLINKS | RESOLVE_BENEATH)
    return syscall(SYS_OPENAT2, root_fd, ctypes.c_char_p(os.fsencode(path)), ctypes.byref(how), ctypes.sizeof(how))

def open_parent(root_fd, path, create=False):
    parent, _, name = path.rpartition("/")
    if not create or not parent:
        return open_beneath(root_fd, parent or ".", os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC), name
    current = os.dup(root_fd)
    try:
        for component in parent.split("/"):
            try:
                os.mkdir(component, 0o755, dir_fd=current)
            except FileExistsError:
                pass
            next_fd = open_beneath(current, component, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
            os.close(current)
            current = next_fd
        return current, name
    except BaseException:
        os.close(current)
        raise

def require_regular(fd):
    info = os.fstat(fd)
    if not stat.S_ISREG(info.st_mode) or info.st_nlink != 1:
        raise ValueError("unsafe file")
    return info

def reject_existing_unsafe(parent_fd, name):
    try:
        fd = open_beneath(parent_fd, name, os.O_RDONLY | os.O_NONBLOCK | os.O_CLOEXEC)
    except FileNotFoundError:
        return False
    try:
        require_regular(fd)
        return True
    finally:
        os.close(fd)

def write_file(root_fd, path, content, overwrite):
    path = valid_path(path)
    if len(content) > MAX_BYTES:
        raise OverflowError("file too large")
    parent_fd, name = open_parent(root_fd, path, create=True)
    temp = ".ani-upload-" + secrets.token_hex(16)
    made = False
    try:
        exists = reject_existing_unsafe(parent_fd, name)
        if exists and not overwrite:
            raise FileExistsError(errno.EEXIST, "file exists")
        fd = open_beneath(parent_fd, temp, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_CLOEXEC, 0o600)
        made = True
        try:
            view = memoryview(content)
            while view:
                count = os.write(fd, view)
                if not count:
                    raise OSError(errno.EIO, "short write")
                view = view[count:]
            os.fsync(fd)
        finally:
            os.close(fd)
        # Never follow the destination. Non-overwrite is atomic even under races.
        syscall(SYS_RENAMEAT2, parent_fd, ctypes.c_char_p(os.fsencode(temp)), parent_fd,
                ctypes.c_char_p(os.fsencode(name)), 0 if overwrite else 1)
        made = False
        os.fsync(parent_fd)
        return {"path": path, "kind": "file", "size_bytes": len(content)}
    finally:
        if made:
            try:
                os.unlink(temp, dir_fd=parent_fd)
            except FileNotFoundError:
                pass
        os.close(parent_fd)

def read_file(root_fd, path):
    path = valid_path(path)
    fd = open_beneath(root_fd, path, os.O_RDONLY | os.O_NONBLOCK | os.O_CLOEXEC)
    try:
        require_regular(fd)
        chunks = []
        remaining = MAX_BYTES + 1
        while remaining:
            part = os.read(fd, min(65536, remaining))
            if not part:
                break
            chunks.append(part)
            remaining -= len(part)
        data = b"".join(chunks)
        if len(data) > MAX_BYTES:
            raise OverflowError("file too large")
        return {"path": path, "content_base64": base64.b64encode(data).decode("ascii"), "size_bytes": len(data)}
    finally:
        os.close(fd)

def delete_file(root_fd, path):
    path = valid_path(path)
    parent_fd, name = open_parent(root_fd, path)
    try:
        # unlinkat never follows symlinks; validation rejects symlinks and hard links.
        if not reject_existing_unsafe(parent_fd, name):
            raise FileNotFoundError(errno.ENOENT, "file not found")
        os.unlink(name, dir_fd=parent_fd)
        os.fsync(parent_fd)
        return {"path": path}
    finally:
        os.close(parent_fd)

def list_files(root_fd, path):
    path = valid_path(path, root_ok=True)
    try:
        fd = open_beneath(root_fd, path, os.O_RDONLY | os.O_NONBLOCK | os.O_CLOEXEC)
    except FileNotFoundError:
        return {"items": []}
    result = []
    def append(path, kind, info):
        if len(result) >= MAX_ENTRIES:
            raise OverflowError("directory has too many entries")
        result.append({"path": path, "kind": kind,
                       "size_bytes": 0 if kind == "directory" else info.st_size,
                       "updated_at_ns": info.st_mtime_ns})
    def walk(directory_fd, prefix, depth):
        if depth > 128:
            raise OverflowError("directory tree too deep")
        with os.scandir(directory_fd) as entries:
            for entry in entries:
                name = entry.name
                try:
                    name.encode("utf-8")
                    info = entry.stat(follow_symlinks=False)
                except (UnicodeEncodeError, FileNotFoundError):
                    continue
                child = name if not prefix else prefix + "/" + name
                if stat.S_ISREG(info.st_mode) and info.st_nlink == 1:
                    append(child, "file", info)
                elif stat.S_ISDIR(info.st_mode):
                    try:
                        child_fd = open_beneath(directory_fd, name, os.O_RDONLY | os.O_DIRECTORY | os.O_CLOEXEC)
                    except OSError as e:
                        if e.errno in (errno.ENOENT, errno.ENOTDIR, errno.ELOOP, errno.EXDEV):
                            continue
                        raise
                    try:
                        append(child, "directory", os.fstat(child_fd))
                        walk(child_fd, child, depth + 1)
                    finally:
                        os.close(child_fd)
    try:
        info = os.fstat(fd)
        if stat.S_ISDIR(info.st_mode):
            walk(fd, "" if path == "." else path, 0)
        elif stat.S_ISREG(info.st_mode) and info.st_nlink == 1:
            append(path, "file", info)
        else:
            raise ValueError("unsafe file")
        result.sort(key=lambda item: item["path"])
        return {"items": result}
    finally:
        os.close(fd)

def workspace_is_mount(path):
    # mountinfo escapes whitespace, backslash and newline in its mountpoint field.
    expected = path.replace("\\", "\\134").replace(" ", "\\040").replace("\t", "\\011").replace("\n", "\\012")
    with open("/proc/self/mountinfo", "rt", encoding="utf-8") as f:
        return any(len(fields) > 4 and fields[4] == expected for fields in (line.split() for line in f))

def main():
    if not workspace_is_mount("/workspace"):
        raise ValueError("workspace is not a dedicated mount")
    request = json.load(sys.stdin)
    root_fd = os.open("/workspace", os.O_RDONLY | os.O_DIRECTORY | os.O_NOFOLLOW | os.O_CLOEXEC)
    try:
        action = request.get("action")
        if action == "write":
            raw = base64.b64decode(request["content_base64"], validate=True)
            result = write_file(root_fd, request["path"], raw, request.get("overwrite") is True)
        elif action == "read":
            result = read_file(root_fd, request["path"])
        elif action == "list":
            result = list_files(root_fd, request.get("path", ""))
        elif action == "delete":
            result = delete_file(root_fd, request["path"])
        else:
            raise ValueError("invalid action")
        print(json.dumps(result, ensure_ascii=True, separators=(",", ":")))
    finally:
        os.close(root_fd)

if __name__ == "__main__":
    try:
        main()
    except (OSError, ValueError, OverflowError, KeyError) as e:
        # Error text is a stable classification and never includes request data.
        kind = "too_large" if isinstance(e, OverflowError) else "conflict" if isinstance(e, FileExistsError) else "not_found" if isinstance(e, FileNotFoundError) else "invalid" if isinstance(e, ValueError) or getattr(e, "errno", None) in (errno.ELOOP, errno.EXDEV, errno.ENOTDIR) else "unavailable"
        print(json.dumps({"error": kind}), file=sys.stderr)
        sys.exit(1)
