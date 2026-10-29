#!/usr/bin/env python3
"""Offline bridge policy/protocol tests; no SSH or cluster command is run."""

import base64
import copy
import http.client
import http.server
import importlib.util
import json
from pathlib import Path
import threading
import unittest


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, Path(__file__).with_name(filename))
    module = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(module)
    return module


bridge = load("container_restart_ssh_bridge", "container-restart-ssh-bridge.py")
fixture = load("container_restart_observer_fixture", "test_container_restart_live_observe.py")
observer = bridge.observer


def exec_command():
    request = fixture.request()
    payload = {
        "schema": "container-restart-marker-exec-v1", "event": request["event"],
        "instance_id": fixture.INSTANCE, "run_id": fixture.RUN,
        "expected_pod_uid": fixture.OLD_POD_UID,
        "marker_content": request["marker_content"],
        "mounts": [{"mount_path": "/workspace", "marker_name": request["marker_name"],
                    "read_only": False, "expected_sha256": request["marker_sha256"]}],
    }
    encoded = base64.b64encode(json.dumps(payload).encode()).decode("ascii")
    return observer.KUBECTL_PREFIX + ["exec", fixture.POD_NAME, "-n", fixture.NAMESPACE, "-c", "workspace", "--", "python3", "-c", observer.POD_PROGRAM, encoded]


class BoundaryBridge(bridge.Bridge):
    def __init__(self):
        super().__init__(fixture.NAMESPACE, "a" * 64)
        self.boundary = fixture.ClusterBoundary()

    def ssh(self, argv):
        return self.boundary.run(argv)


class BridgePolicyTest(unittest.TestCase):
    def test_only_exact_read_and_marker_exec_shapes_are_permitted(self):
        reads = [observer.CONFIG_QUERY,
                 ["get", "namespace", fixture.NAMESPACE, "-o", "json"],
                 ["get", "pods", "-n", fixture.NAMESPACE, "-o", "json"],
                 ["get", "notebooks.kubeflow.org", fixture.NAME, "-n", fixture.NAMESPACE, "-o", "json"],
                 ["get", "persistentvolumeclaims", "research-data", "-n", fixture.NAMESPACE, "-o", "json"],
                 ["get", "pods", fixture.POD_NAME, "-n", fixture.NAMESPACE, "-o", "json", "--ignore-not-found=true"]]
        for args in reads:
            with self.subTest(args=args):
                self.assertIsNone(bridge.permitted_command(observer.KUBECTL_PREFIX + args, fixture.NAMESPACE))
        self.assertEqual(bridge.permitted_command(exec_command(), fixture.NAMESPACE)["instance_id"], fixture.INSTANCE)

    def test_shell_writes_foreign_namespace_and_changed_program_are_rejected(self):
        changed = exec_command()
        changed[-2] += "\nprint('changed')"
        commands = [
            ["sh", "-c", "kubectl get pods"],
            observer.KUBECTL_PREFIX + ["delete", "pods", fixture.POD_NAME, "-n", fixture.NAMESPACE],
            observer.KUBECTL_PREFIX + ["get", "secrets", "secret", "-n", fixture.NAMESPACE, "-o", "json"],
            observer.KUBECTL_PREFIX + ["get", "pods", fixture.POD_NAME, "-n", "other", "-o", "json"],
            observer.KUBECTL_PREFIX + ["get", "pods", fixture.POD_NAME + ";touch /tmp/x", "-n", fixture.NAMESPACE, "-o", "json"],
            observer.KUBECTL_PREFIX + ["exec", fixture.POD_NAME, "-n", fixture.NAMESPACE, "--", "sh", "-c", "id"],
            changed, exec_command() + ["extra"],
        ]
        for command in commands:
            with self.subTest(command=command[:8]), self.assertRaises(observer.ObserveError):
                bridge.permitted_command(command, fixture.NAMESPACE)

    def test_exec_rechecks_real_owner_chain_and_current_pvc_paths(self):
        transport = BoundaryBridge()
        result = transport.run({"argv": exec_command(), "stdin_b64": ""})
        self.assertEqual(result["returncode"], 0)
        self.assertEqual(len(transport.boundary.exec_payloads), 1)
        self.assertEqual([call[2:4] for call in transport.boundary.calls[:4]], [["get", "namespace"], ["get", "notebooks.kubeflow.org"], ["get", "statefulsets.apps"], ["get", "pods"]])

    def test_wrong_pod_uid_or_owner_never_executes(self):
        for field in ("uid", "owner"):
            transport = BoundaryBridge()
            if field == "uid":
                transport.boundary.pod["metadata"]["uid"] = fixture.OTHER_UID
            else:
                transport.boundary.pod["metadata"]["ownerReferences"][0]["uid"] = fixture.OTHER_UID
            with self.subTest(field=field), self.assertRaisesRegex(observer.ObserveError, "^EXEC_POD_OWNER_MISMATCH$"):
                transport.run({"argv": exec_command(), "stdin_b64": ""})
            self.assertEqual(transport.boundary.exec_payloads, [])

    def test_non_pvc_path_and_nonempty_stdin_are_rejected(self):
        command = exec_command()
        payload = observer.validate_exec_payload(command[-1])
        payload["mounts"][0]["mount_path"] = "/dev"
        command[-1] = base64.b64encode(json.dumps(payload).encode()).decode("ascii")
        transport = BoundaryBridge()
        with self.assertRaisesRegex(observer.ObserveError, "^EXEC_CURRENT_MOUNT_MISMATCH$"):
            transport.run({"argv": command, "stdin_b64": ""})
        self.assertEqual(transport.boundary.exec_payloads, [])
        with self.assertRaisesRegex(observer.ObserveError, "^REQUEST_FIELDS_FORBIDDEN$"):
            transport.run({"argv": exec_command(), "stdin_b64": "eA=="})


class ProtocolTest(unittest.TestCase):
    def test_loopback_authentication_and_bounded_request(self):
        class FakeTransport:
            token = "t" * 64
            calls = []

            def run(self, value):
                self.calls.append(value)
                return {"returncode": 0, "stdout": "verified", "stderr": ""}

        server = http.server.HTTPServer(("127.0.0.1", 0), bridge.Handler)
        server.bridge = FakeTransport()
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            def post(token, body):
                connection = http.client.HTTPConnection("127.0.0.1", server.server_port, timeout=3)
                try:
                    connection.request("POST", "/run", body, {"Authorization": "Bearer " + token})
                    response = connection.getresponse()
                    return response.status, json.loads(response.read())
                finally:
                    connection.close()
            self.assertEqual(post("wrong", b"{}")[0], 401)
            self.assertEqual(server.bridge.calls, [])
            request = {"argv": observer.KUBECTL_PREFIX + observer.CONFIG_QUERY, "stdin_b64": ""}
            status, result = post(server.bridge.token, json.dumps(request).encode())
            self.assertEqual((status, result["stdout"]), (200, "verified"))
            self.assertEqual(server.bridge.calls, [request])
            self.assertEqual(post(server.bridge.token, b" " * (observer.MAX_REQUEST + 1))[0], 403)
            self.assertEqual(server.bridge.calls, [request])
        finally:
            server.shutdown()
            thread.join(timeout=3)
            server.server_close()


if __name__ == "__main__":
    unittest.main()
