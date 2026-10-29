#!/usr/bin/env python3
"""Task-private loopback SSH transport for the Fedora LIVE observer.

Run on the workstation which already has SSH access to the approved cluster:
  ./container-restart-ssh-bridge.py --namespace <tenant namespace> \
      --token-file /absolute/task-private-token --port 39399
Reverse-forward that loopback port to Fedora; the acceptance driver, API calls,
mount selection and marker decisions continue to run on Fedora. This process
does not accept credentials, arbitrary local commands, shell programs, kubectl
writes, or arbitrary exec programs. No authentication/header/body is logged.
The token must be a task-owned regular 0600 file containing 32-256 URL-safe
random characters. Stop this process and remove the token after acceptance.
"""

import argparse
import hmac
import http.server
import importlib.util
import json
from pathlib import Path
import re
import subprocess
import tempfile
import time


spec = importlib.util.spec_from_file_location("container_restart_observer", Path(__file__).with_name("container-restart-live-observe.py"))
observer = importlib.util.module_from_spec(spec)
spec.loader.exec_module(observer)


def validate_command(argv, namespace):
    """Return an exec payload or None; every permitted argv has one shape."""
    observer.require(isinstance(argv, list) and all(isinstance(item, str) and "\x00" not in item for item in argv) and argv[:2] == observer.KUBECTL_PREFIX, "COMMAND_FORBIDDEN")
    args = argv[2:]
    if args == observer.CONFIG_QUERY or args == ["get", "namespace", namespace, "-o", "json"] or args == ["get", "pods", "-n", namespace, "-o", "json"]:
        return None
    if len(args) in (7, 8) and args[0] == "get" and args[3:7] == ["-n", namespace, "-o", "json"]:
        kind, name = args[1:3]
        observer.require(kind in ("notebooks.kubeflow.org", "statefulsets.apps", "pods", "persistentvolumeclaims"), "GET_RESOURCE_FORBIDDEN")
        if kind == "persistentvolumeclaims":
            observer.require(len(name) <= 253 and observer.DNS_NAME.fullmatch(name), "PVC_NAME_FORBIDDEN")
        else:
            pattern = r"instance-([0-9a-f-]{36})" + (r"-[0-9]+" if kind == "pods" else "")
            match = re.fullmatch(pattern, name)
            observer.require(match is not None, "WORKLOAD_NAME_FORBIDDEN")
            observer.canonical_uuid(match[1])
        observer.require(len(args) == 7 or (kind == "pods" and args[7] == "--ignore-not-found=true"), "GET_FLAGS_FORBIDDEN")
        return None
    raise observer.ObserveError("EXEC_PROGRAM_FORBIDDEN")


def permitted_command(argv, namespace):
    # Keep exec separate from read shapes so neither accepts optional flags.
    if isinstance(argv, list) and argv[:2] == observer.KUBECTL_PREFIX and len(argv) == 13 and argv[2] == "exec":
        args = argv[2:]
        observer.require(all(isinstance(item, str) and "\x00" not in item for item in argv) and args[2:10] == ["-n", namespace, "-c", "workspace", "--", "python3", "-c", observer.POD_PROGRAM], "EXEC_PROGRAM_FORBIDDEN")
        payload = observer.validate_exec_payload(args[10])
        observer.require(re.fullmatch(re.escape("instance-" + payload["instance_id"]) + r"-[0-9]+", args[1]), "EXEC_POD_SCOPE_MISMATCH")
        return payload
    return validate_command(argv, namespace)


class Bridge:
    def __init__(self, namespace, token, known_hosts=None):
        self.namespace, self.token, self.known_hosts = namespace, token, known_hosts
        self.deadline = None

    def ssh(self, argv):
        # Temporary files avoid accumulating unbounded remote output in memory.
        with tempfile.TemporaryFile() as stdout, tempfile.TemporaryFile() as stderr:
            try:
                timeout = 45 if self.deadline is None else min(45, self.deadline - time.monotonic())
                observer.require(timeout > 0, "SSH_TIMEOUT")
                result = subprocess.run(observer.ssh_argv(observer.SSH_ENTRY, 22, self.known_hosts, argv), stdin=subprocess.DEVNULL, stdout=stdout, stderr=stderr, timeout=timeout, check=False)
                code = result.returncode
            except subprocess.TimeoutExpired:
                return {"returncode": 124, "stdout": "", "stderr": "SSH_TIMEOUT"}
            except OSError:
                return {"returncode": 125, "stdout": "", "stderr": "SSH_UNAVAILABLE"}
            stdout.seek(0)
            stderr.seek(0)
            output, error = stdout.read(observer.MAX_RESPONSE + 1), stderr.read(observer.MAX_RESPONSE + 1)
            if len(output) + len(error) > observer.MAX_RESPONSE - 1024:
                return {"returncode": 125, "stdout": "", "stderr": "SSH_RESPONSE_TOO_LARGE"}
            result = {"returncode": code, "stdout": output.decode("utf-8", "replace"), "stderr": error.decode("utf-8", "replace")}
            if len(json.dumps(result).encode()) > observer.MAX_RESPONSE:
                return {"returncode": 125, "stdout": "", "stderr": "SSH_RESPONSE_TOO_LARGE"}
            return result

    def get(self, kind, name):
        args = ["get", kind, name]
        if kind != "namespace":
            args += ["-n", self.namespace]
        result = self.ssh(observer.KUBECTL_PREFIX + args + ["-o", "json"])
        observer.require(result["returncode"] == 0, "EXEC_IDENTITY_UNAVAILABLE")
        return observer.object_json(result["stdout"])

    def guard_exec(self, argv, payload):
        """Enforce transport scope again before allowing the fixed marker code."""
        name = "instance-" + payload["instance_id"]
        namespace = self.get("namespace", self.namespace)
        ns = observer.metadata(namespace, "Namespace", "", self.namespace)
        labels = ns.get("labels", {})
        tenant = observer.canonical_uuid(labels.get("network.ani.io/tenant-id"))
        observer.require(labels.get("network.ani.io/managed-by") == "ani-network-service", "EXEC_NAMESPACE_OWNER_MISMATCH")
        notebook = self.get("notebooks.kubeflow.org", name)
        nb = observer.metadata(notebook, "Notebook", self.namespace, name)
        create = observer.managed_labels(nb, tenant, payload["instance_id"])
        sts = self.get("statefulsets.apps", name)
        sts_meta = observer.metadata(sts, "StatefulSet", self.namespace, name)
        observer.require(observer.controlled(sts, "kubeflow.org", "Notebook", name, nb["uid"]), "EXEC_STATEFULSET_OWNER_MISMATCH")
        pod = self.get("pods", argv[3])
        meta = observer.metadata(pod, "Pod", self.namespace, argv[3])
        observer.require(meta["uid"] == payload["expected_pod_uid"] and observer.controlled(pod, "apps", "StatefulSet", name, sts_meta["uid"]) and observer.ready(pod), "EXEC_POD_OWNER_MISMATCH")
        observer.managed_labels(meta, tenant, payload["instance_id"], create)
        containers = [item for item in pod.get("spec", {}).get("containers", []) if item.get("name") == "workspace"]
        observer.require(len(containers) == 1, "EXEC_CONTAINER_MISMATCH")
        volumes = {item.get("name"): item.get("persistentVolumeClaim") for item in pod.get("spec", {}).get("volumes", []) if "persistentVolumeClaim" in item}
        allowed = {}
        for item in containers[0].get("volumeMounts", []):
            volume = volumes.get(item.get("name"))
            if volume is None:
                continue
            managed = item.get("name") == "workspace" and volume.get("claimName") == "workspace-" + payload["instance_id"]
            managed = managed or re.fullmatch(r"borrowed-[0-9]+", item.get("name", "")) is not None
            observer.require(managed and "subPath" not in item and "subPathExpr" not in item and item.get("mountPath") not in allowed and item.get("readOnly", False) is volume.get("readOnly", False), "EXEC_CURRENT_MOUNT_MISMATCH")
            allowed[item.get("mountPath")] = item.get("readOnly", False)
        observer.require(set(allowed) == {item["mount_path"] for item in payload["mounts"]} and all(allowed[item["mount_path"]] is item["read_only"] for item in payload["mounts"]), "EXEC_CURRENT_MOUNT_MISMATCH")

    def run(self, request):
        observer.require(set(request) == {"argv", "stdin_b64"} and request["stdin_b64"] == "", "REQUEST_FIELDS_FORBIDDEN")
        payload = permitted_command(request["argv"], self.namespace)
        self.deadline = time.monotonic() + 45
        try:
            if payload is not None:
                self.guard_exec(request["argv"], payload)
            return self.ssh(request["argv"])
        finally:
            self.deadline = None


class Handler(http.server.BaseHTTPRequestHandler):
    server_version = "TaskSSHBridge"

    def setup(self):
        super().setup()
        self.connection.settimeout(5)

    def log_message(self, *_):
        pass

    def respond(self, status, value):
        data = json.dumps(value, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.send_header("Cache-Control", "no-store")
        self.end_headers()
        self.wfile.write(data)

    def do_POST(self):
        try:
            observer.require(self.path == "/run" and not self.headers.get("Transfer-Encoding"), "REQUEST_FORBIDDEN")
            authorization = self.headers.get("Authorization", "")
            if len(self.headers.get_all("Authorization", [])) != 1 or not hmac.compare_digest(authorization.encode("utf-8"), ("Bearer " + self.server.bridge.token).encode("ascii")):
                self.respond(401, {"error": "UNAUTHORIZED"})
                return
            size = self.headers.get("Content-Length", "")
            observer.require(len(self.headers.get_all("Content-Length", [])) == 1 and size.isascii() and size.isdecimal() and 0 < int(size) <= observer.MAX_REQUEST, "REQUEST_TOO_LARGE")
            raw = self.rfile.read(int(size))
            observer.require(len(raw) == int(size), "REQUEST_INCOMPLETE")
            self.respond(200, self.server.bridge.run(observer.object_json(raw)))
        except observer.ObserveError as error:
            self.respond(403, {"error": str(error)})
        except (OSError, ValueError):
            self.respond(503, {"error": "TRANSPORT_UNAVAILABLE"})


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--namespace", required=True)
    parser.add_argument("--token-file", required=True)
    parser.add_argument("--port", type=int, default=39399)
    parser.add_argument("--known-hosts")
    args = parser.parse_args()
    try:
        observer.require(len(args.namespace) <= 63 and observer.DNS_LABEL.fullmatch(args.namespace) and 1 <= args.port <= 65535, "INVALID_BRIDGE_SCOPE")
        if args.known_hosts:
            observer.require(Path(args.known_hosts).is_absolute(), "ABSOLUTE_KNOWN_HOSTS_REQUIRED")
        token = observer.private_file(args.token_file).strip()
        observer.require(re.fullmatch(rb"[A-Za-z0-9_-]{32,256}", token), "INVALID_BRIDGE_TOKEN")
        server = http.server.HTTPServer(("127.0.0.1", args.port), Handler)
        server.bridge = Bridge(args.namespace, token.decode("ascii"), args.known_hosts)
        try:
            server.serve_forever()
        finally:
            server.server_close()
        return 0
    except observer.ObserveError as error:
        print(json.dumps({"error": str(error)}), file=__import__("sys").stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
