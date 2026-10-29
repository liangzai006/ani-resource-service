#!/usr/bin/env python3
"""Execute the exact guest marker program against a private temporary filesystem.

Only external mountinfo, Pod-UID environment and RO-device behavior are doubled.
File writes, fsync, read/hash and symlink checks execute the real guest program.
"""

import base64
import contextlib
import errno
import hashlib
import importlib.util
import io
import json
import os
from pathlib import Path
import sys
import tempfile
from types import SimpleNamespace
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location("container_restart_marker_source", Path(__file__).with_name("container-restart-live-observe.py"))
observer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(observer)
RUN = "22222222-2222-4222-8222-222222222222"
POD = "88888888-8888-4888-8888-888888888888"
CONTENT = "ani-container-restart:" + RUN + "\n"
HASH = hashlib.sha256(CONTENT.encode()).hexdigest()


class MarkerProgramTest(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.path = Path(self.directory.name)
        self.name = ".ani-restart-" + RUN + ".txt"

    def execute(self, event, readonly=False, existing_name=None, existing_hash=None):
        payload = {"event": event, "run_id": RUN, "expected_pod_uid": POD, "marker_content": CONTENT,
                   "mounts": [{"mount_path": str(self.path), "read_only": readonly,
                               "marker_name": existing_name or self.name,
                               "expected_sha256": existing_hash or HASH}]}
        argv = ["python3", base64.b64encode(json.dumps(payload).encode()).decode("ascii")]
        original_open = open
        original_os_open = os.open
        writes, flushes = [], []
        def mountinfo(path, *args, **kwargs):
            if path == "/proc/self/mountinfo":
                return io.StringIO("1 0 0:1 / " + str(self.path) + " rw - tmpfs tmpfs rw\n")
            return original_open(path, *args, **kwargs)
        def device_open(path, flags, *args, **kwargs):
            if flags & (os.O_WRONLY | os.O_RDWR | os.O_CREAT | os.O_TRUNC):
                writes.append(path)
                if readonly:
                    raise OSError(errno.EROFS, "readonly external device")
            return original_os_open(path, flags, *args, **kwargs)
        original_fsync = os.fsync
        def fsync(fd):
            flushes.append(fd)
            return original_fsync(fd)
        stdout, stderr = io.StringIO(), io.StringIO()
        code = 0
        with mock.patch("builtins.open", side_effect=mountinfo), mock.patch.object(sys, "argv", argv), mock.patch.dict(os.environ, {"ANI_INSTANCE_POD_UID": POD}), mock.patch("os.open", side_effect=device_open), mock.patch("os.fstatvfs", return_value=SimpleNamespace(f_flag=os.ST_RDONLY if readonly else 0)), mock.patch("os.fsync", side_effect=fsync), contextlib.redirect_stdout(stdout), contextlib.redirect_stderr(stderr):
            try:
                exec(compile(observer.POD_PROGRAM, "fixed_guest_marker_program", "exec"), {})
            except SystemExit as failure:
                code = failure.code
        return code, json.loads(stdout.getvalue() or stderr.getvalue()), writes, flushes

    def test_real_write_flush_read_hash_then_after_is_read_only(self):
        code, result, writes, flushes = self.execute("before-restart")
        self.assertEqual(code, 0)
        self.assertEqual((self.path / self.name).read_bytes(), CONTENT.encode())
        self.assertTrue(result["markers"][0]["write_verified"])
        self.assertTrue(writes)
        self.assertEqual(len(flushes), 2)
        code, result, writes, flushes = self.execute("after-restart")
        self.assertEqual(code, 0)
        self.assertEqual(result["markers"][0]["sha256"], HASH)
        self.assertNotIn("write_verified", result["markers"][0])
        self.assertEqual((writes, flushes), ([], []))

    def test_existing_unrelated_marker_is_preserved(self):
        marker = self.path / self.name
        marker.write_bytes(b"preexisting unrelated content")
        code, result, _, _ = self.execute("before-restart")
        self.assertEqual((code, result["error"]), (2, "EXISTING_MARKER_CONFLICT"))
        self.assertEqual(marker.read_bytes(), b"preexisting unrelated content")

    def test_readonly_existing_hash_and_rejected_probe(self):
        marker = self.path / "existing-data.txt"
        marker.write_bytes(b"existing borrowed readonly input\n")
        expected = hashlib.sha256(marker.read_bytes()).hexdigest()
        for event in ("before-restart", "after-restart"):
            with self.subTest(event=event):
                code, result, writes, flushes = self.execute(event, True, marker.name, expected)
                self.assertEqual(code, 0)
                self.assertTrue(result["markers"][0]["read_only_write_rejected"])
                self.assertEqual(result["markers"][0]["sha256"], expected)
                self.assertNotIn("write_verified", result["markers"][0])
                self.assertEqual(len(writes), 1)
                self.assertTrue(writes[0].startswith(".ani-restart-ro-"))
                self.assertEqual(flushes, [])
        self.assertEqual(list(self.path.iterdir()), [marker])


if __name__ == "__main__":
    unittest.main()
