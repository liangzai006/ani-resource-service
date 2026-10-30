import base64
import ctypes
import errno
import importlib.util
import os
import pathlib
import tempfile
import threading
import unittest

spec = importlib.util.spec_from_file_location("workspace", pathlib.Path(__file__).with_name("workspace.py"))
w = importlib.util.module_from_spec(spec)
spec.loader.exec_module(w)

class WorkspaceTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = pathlib.Path(self.temp.name) / "work"
        self.root.mkdir()
        self.out = pathlib.Path(self.temp.name) / "outside"
        self.out.mkdir()
        self.fd = os.open(self.root, os.O_RDONLY | os.O_DIRECTORY)
    def tearDown(self):
        os.close(self.fd)
        self.temp.cleanup()
    def test_write_read_list_delete(self):
        self.assertEqual(w.write_file(self.fd, "sub/test.txt", b"hello", False)["size_bytes"], 5)
        self.assertEqual(base64.b64decode(w.read_file(self.fd, "sub/test.txt")["content_base64"]), b"hello")
        self.assertEqual(w.list_files(self.fd, "sub")["items"][0]["path"], "sub/test.txt")
        with self.assertRaises(FileExistsError): w.write_file(self.fd, "sub/test.txt", b"new", False)
        w.write_file(self.fd, "sub/test.txt", b"new", True)
        self.assertEqual((self.root/"sub/test.txt").read_bytes(), b"new")
        w.delete_file(self.fd, "sub/test.txt")
        self.assertFalse((self.root/"sub/test.txt").exists())
    def test_escape_symlink_and_hardlink(self):
        victim = self.out / "victim"
        victim.write_bytes(b"untouched")
        (self.root / "linkdir").symlink_to(self.out, target_is_directory=True)
        (self.root / "linkfile").symlink_to(victim)
        os.link(victim, self.root / "hard")
        for name in ("../outside/victim", "/etc/passwd", "x/../victim", "a//b"):
            with self.assertRaises(ValueError): w.write_file(self.fd, name, b"bad", True)
        for name in ("linkdir/new", "linkfile", "hard"):
            with self.assertRaises((OSError, ValueError)): w.write_file(self.fd, name, b"bad", True)
        for name in ("linkdir/victim", "linkfile", "hard"):
            with self.assertRaises((OSError, ValueError)): w.read_file(self.fd, name)
            with self.assertRaises((OSError, ValueError)): w.delete_file(self.fd, name)
        self.assertEqual(victim.read_bytes(), b"untouched")
        self.assertFalse((self.out / "new").exists())
    def test_size_and_unsupported(self):
        with self.assertRaises(OverflowError): w.write_file(self.fd, "huge", b"x" * (w.MAX_BYTES+1), False)
        (self.root / "huge").write_bytes(b"x" * (w.MAX_BYTES+1))
        with self.assertRaises(OverflowError): w.read_file(self.fd, "huge")
        os.mkfifo(self.root / "fifo")
        with self.assertRaises(ValueError): w.read_file(self.fd, "fifo")
    def test_concurrent_parent_replacement_never_writes_outside(self):
        (self.root / "parent").mkdir()
        (self.root / "swap").symlink_to(self.out, target_is_directory=True)
        done = threading.Event()
        failures = []
        exchanges = []
        def replace():
            try:
                while not done.is_set():
                    w.syscall(w.SYS_RENAMEAT2, self.fd, ctypes.c_char_p(b"parent"),
                              self.fd, ctypes.c_char_p(b"swap"), 2)
                    exchanges.append(1)
            except BaseException as e:
                failures.append(e)
        thread = threading.Thread(target=replace)
        thread.start()
        try:
            for _ in range(500):
                try:
                    w.write_file(self.fd, "parent/escape", b"payload", True)
                except (OSError, ValueError):
                    pass
        finally:
            done.set()
            thread.join()
        self.assertFalse(failures)
        self.assertGreater(len(exchanges), 10)
        self.assertFalse((self.out / "escape").exists())

    def test_symlink_replacement_race(self):
        # Replacement is atomic: overwriting a linked inode never changes it.
        victim = self.out / "victim"
        victim.write_bytes(b"untouched")
        os.link(victim, self.root / "hard")
        with self.assertRaises(ValueError): w.write_file(self.fd, "hard", b"bad", True)
        self.assertEqual(victim.read_bytes(), b"untouched")

if __name__ == "__main__": unittest.main()
