#!/usr/bin/env python3
"""Fedora LIVE observation hook for Gov's container restart acceptance driver.

Required: ANI_ACCEPTANCE_TENANT_NAMESPACE. No login credentials are accepted.
Transport defaults to SSH kubercloud@100.64.128.12:22 with strict host checking.
ANI_ACCEPTANCE_SSH_HOST may also be 127.0.0.1 for the task's TCP relay;
ANI_ACCEPTANCE_SSH_PORT and ANI_ACCEPTANCE_SSH_KNOWN_HOSTS are optional.
Alternatively set ANI_ACCEPTANCE_SSH_BRIDGE_URL=http://127.0.0.1:<port> and
ANI_ACCEPTANCE_SSH_BRIDGE_TOKEN_FILE to the task-owned 0600 token file. The
bridge transports only the fixed kubectl read/marker commands; decisions stay
here on Fedora. No SSH private key is copied.

Optional ANI_ACCEPTANCE_OBSERVER_CONFIG names a task-owned 0600 JSON file:
 {"schema":"container-restart-live-observer-config-v1",
  "state_dir":"/absolute/private/observer-state",
  "cluster_context":"kubernetes-admin@ani-kcn-full",
  "cluster_endpoint":"https://lb.kubesphere.local:6443",
  "namespace_uid":"<optional frozen UUID>","tenant_id":"<optional UUID>",
  "read_only_markers":{"/datasets":{"marker_name":"existing.txt",
                                       "sha256":"<64 lowercase hex>"}}}
No configuration is needed for the first CPU workspace-only path. Readonly
mounts require their existing file/hash before this hook can verify them.

Only synthetic marker files are written, inside the proven current Pod's
workspace and writable borrowed mounts, and only before restart. After restart
markers are read only. Readonly mounts are checked using an exclusive synthetic
write probe which must fail; an unexpectedly created probe is removed by its
own descriptor/name and the observation fails. No volume, controller or Pod is
patched/deleted. The image must contain python3; tools are never installed.
"""

import base64
import datetime as dt
import fcntl
import hashlib
import http.client
import json
import os
from pathlib import Path, PurePosixPath
import re
import shlex
import stat
import subprocess
import sys
import urllib.parse
import uuid


MAX_REQUEST = 65536
MAX_RESPONSE = 3 * 1024 * 1024
SSH_ENTRY = "100.64.128.12"
SSH_USER = "kubercloud"
OWNER = "ani-resource-service"
TENANT_LABEL = "accelerator.ani.io/resource-tenant-id"
RESOURCE_LABEL = "accelerator.ani.io/resource-id"
OWNER_LABEL = "accelerator.ani.io/owner-service"
CREATE_LABEL = "accelerator.ani.io/create-operation-id"
KUBECTL_PREFIX = ["kubectl", "--request-timeout=15s"]
CONFIG_QUERY = ["config", "view", "--minify", "-o", 'jsonpath={.current-context}{"\\n"}{.clusters[0].cluster.server}']
SHA256 = re.compile(r"[0-9a-f]{64}\Z")
DNS_LABEL = re.compile(r"[a-z0-9](?:[-a-z0-9]*[a-z0-9])?\Z")
DNS_NAME = re.compile(r"[a-z0-9](?:[-a-z0-9.]*[a-z0-9])?\Z")


class ObserveError(Exception):
    pass


def require(condition, code):
    if not condition:
        raise ObserveError(code)


def canonical_uuid(value):
    require(isinstance(value, str), "INVALID_UUID")
    try:
        result = uuid.UUID(value)
    except ValueError:
        raise ObserveError("INVALID_UUID") from None
    require(result.int != 0 and str(result) == value, "INVALID_UUID")
    return value


def object_json(raw):
    def unique(pairs):
        result = {}
        for key, value in pairs:
            require(key not in result, "DUPLICATE_JSON_KEY")
            result[key] = value
        return result
    try:
        result = json.loads(raw, object_pairs_hook=unique, parse_constant=lambda _: (_ for _ in ()).throw(ValueError()))
    except (ValueError, UnicodeError):
        raise ObserveError("INVALID_JSON") from None
    require(isinstance(result, dict), "JSON_OBJECT_REQUIRED")
    return result


def canonical_path(path):
    require(isinstance(path, str) and 1 < len(path) <= 4096 and path.startswith("/") and str(PurePosixPath(path)) == path, "INVALID_MOUNT_PATH")
    require("\\" not in path and all(ord(char) >= 32 and ord(char) != 127 for char in path) and all(part not in (".", "..", "") for part in path[1:].split("/")), "INVALID_MOUNT_PATH")
    return path


def marker_name(value):
    require(isinstance(value, str) and 0 < len(value) <= 253 and value not in (".", "..") and "/" not in value and "\\" not in value and all(ord(char) >= 32 and ord(char) != 127 for char in value), "INVALID_MARKER_NAME")
    return value


def private_file(path):
    path = Path(path)
    require(path.is_absolute(), "PRIVATE_FILE_ABSOLUTE_PATH_REQUIRED")
    try:
        fd = os.open(path, os.O_RDONLY | os.O_NOFOLLOW)
        with os.fdopen(fd, "rb") as stream:
            info = os.fstat(stream.fileno())
            require(stat.S_ISREG(info.st_mode) and info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) == 0o600, "PRIVATE_FILE_OWNER_MODE_REQUIRED")
            raw = stream.read(MAX_REQUEST + 1)
    except OSError:
        raise ObserveError("PRIVATE_FILE_UNAVAILABLE") from None
    require(len(raw) <= MAX_REQUEST, "PRIVATE_FILE_TOO_LARGE")
    return raw


def validate_request(request):
    require(set(request) == {"schema", "event", "run_id", "instance_id", "operation_id", "mounts", "marker_name", "marker_content", "marker_sha256", "old_pod"}, "INVALID_OBSERVATION_FIELDS")
    require(request["schema"] == "container-restart-live-observation-v1" and request["event"] in ("before-restart", "after-restart"), "INVALID_OBSERVATION_SCHEMA")
    run_id, instance_id = canonical_uuid(request["run_id"]), canonical_uuid(request["instance_id"])
    content = "ani-container-restart:" + run_id + "\n"
    require(request["marker_name"] == ".ani-restart-" + run_id + ".txt" and request["marker_content"] == content and request["marker_sha256"] == hashlib.sha256(content.encode()).hexdigest(), "SYNTHETIC_MARKER_REQUIRED")
    if request["operation_id"] is not None:
        canonical_uuid(request["operation_id"])
    mounts = request["mounts"]
    require(isinstance(mounts, list) and 1 <= len(mounts) <= 9, "INVALID_CURRENT_MOUNTS")
    paths, workspace = [], 0
    for mount in mounts:
        require(isinstance(mount, dict) and isinstance(mount.get("read_only"), bool), "INVALID_CURRENT_MOUNT")
        path = canonical_path(mount.get("mount_path"))
        require(path not in paths and not any(path.startswith(other + "/") or other.startswith(path + "/") for other in paths), "OVERLAPPING_MOUNTS")
        paths.append(path)
        if mount.get("ownership") == "workspace":
            require(set(mount) == {"mount_path", "read_only", "ownership"} and mount["read_only"] is False, "INVALID_WORKSPACE_MOUNT")
            workspace += 1
        else:
            require(set(mount) == {"mount_path", "read_only", "ownership", "pvc_name"} and mount.get("ownership") == "borrowed" and isinstance(mount.get("pvc_name"), str) and len(mount["pvc_name"]) <= 253 and DNS_NAME.fullmatch(mount["pvc_name"]), "INVALID_BORROWED_MOUNT")
    require(workspace == 1 and mounts[0]["ownership"] == "workspace", "ONE_WORKSPACE_REQUIRED")
    if request["event"] == "before-restart":
        require(request["old_pod"] is None, "UNEXPECTED_OLD_POD")
    else:
        old = request["old_pod"]
        require(isinstance(old, dict) and set(old) == {"name", "uid"}, "OLD_POD_REQUIRED")
        canonical_uuid(old["uid"])
        require(isinstance(old["name"], str) and re.fullmatch(re.escape("instance-" + instance_id) + r"-[0-9]+", old["name"]), "OLD_POD_SCOPE_MISMATCH")
    return request


class Config:
    def __init__(self, environment):
        self.namespace = environment.get("ANI_ACCEPTANCE_TENANT_NAMESPACE", "")
        require(len(self.namespace) <= 63 and DNS_LABEL.fullmatch(self.namespace), "TENANT_NAMESPACE_REQUIRED")
        extra = {}
        if environment.get("ANI_ACCEPTANCE_OBSERVER_CONFIG"):
            extra = object_json(private_file(environment["ANI_ACCEPTANCE_OBSERVER_CONFIG"]))
            require(extra.get("schema") == "container-restart-live-observer-config-v1" and set(extra) <= {"schema", "state_dir", "cluster_context", "cluster_endpoint", "namespace_uid", "tenant_id", "read_only_markers"}, "INVALID_PRIVATE_CONFIGURATION")
        self.context = extra.get("cluster_context", "kubernetes-admin@ani-kcn-full")
        self.endpoint = extra.get("cluster_endpoint", "https://lb.kubesphere.local:6443")
        require(isinstance(self.context, str) and self.context and isinstance(self.endpoint, str) and self.endpoint.startswith("https://"), "CLUSTER_IDENTITY_REQUIRED")
        self.namespace_uid, self.tenant_id = extra.get("namespace_uid"), extra.get("tenant_id")
        for value in (self.namespace_uid, self.tenant_id):
            if value is not None:
                canonical_uuid(value)
        self.read_only_markers = extra.get("read_only_markers", {})
        require(isinstance(self.read_only_markers, dict), "INVALID_READ_ONLY_CONFIGURATION")
        self.state_dir = Path(extra.get("state_dir", str(Path(environment.get("HOME", "")) / ".local/state/ani/container-restart-live-observe")))
        require(self.state_dir.is_absolute(), "PRIVATE_STATE_DIRECTORY_REQUIRED")
        self.ssh_host = environment.get("ANI_ACCEPTANCE_SSH_HOST", SSH_ENTRY)
        require(self.ssh_host in (SSH_ENTRY, "127.0.0.1"), "SSH_TARGET_FORBIDDEN")
        port = environment.get("ANI_ACCEPTANCE_SSH_PORT", "22")
        require(isinstance(port, str) and port.isdecimal() and 1 <= int(port) <= 65535, "INVALID_SSH_PORT")
        self.ssh_port = int(port)
        self.known_hosts = environment.get("ANI_ACCEPTANCE_SSH_KNOWN_HOSTS")
        if self.known_hosts:
            require(Path(self.known_hosts).is_absolute(), "ABSOLUTE_KNOWN_HOSTS_REQUIRED")
        self.bridge = environment.get("ANI_ACCEPTANCE_SSH_BRIDGE_URL")
        self.bridge_token = None
        if self.bridge:
            parsed = urllib.parse.urlsplit(self.bridge)
            try:
                bridge_port = parsed.port
            except ValueError:
                raise ObserveError("INVALID_BRIDGE_URL") from None
            require(parsed.scheme == "http" and parsed.hostname == "127.0.0.1" and bridge_port and not parsed.username and not parsed.password and not parsed.query and not parsed.fragment and parsed.path in ("", "/run"), "INVALID_BRIDGE_URL")
            self.bridge_port = bridge_port
            token_path = environment.get("ANI_ACCEPTANCE_SSH_BRIDGE_TOKEN_FILE")
            require(token_path, "PRIVATE_BRIDGE_TOKEN_REQUIRED")
            token = private_file(token_path).strip()
            require(re.fullmatch(rb"[A-Za-z0-9_-]{32,256}", token), "INVALID_BRIDGE_TOKEN")
            self.bridge_token = token.decode("ascii")


def ssh_argv(host, port, known_hosts, remote_argv):
    command = ["ssh", "-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10", "-o", "StrictHostKeyChecking=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes", "-o", "HostKeyAlias=" + SSH_ENTRY, "-p", str(port)]
    if known_hosts:
        command += ["-o", "UserKnownHostsFile=" + str(known_hosts)]
    # SSH invokes the remote login shell. Quote every argv element explicitly;
    # JSON/payload data is never interpolated as remote shell source.
    return command + [SSH_USER + "@" + host, shlex.join(remote_argv)]


class Transport:
    def __init__(self, config):
        self.config = config

    def run(self, argv):
        if self.config.bridge:
            body = json.dumps({"argv": argv, "stdin_b64": ""}, separators=(",", ":")).encode()
            require(len(body) <= MAX_REQUEST, "BRIDGE_REQUEST_TOO_LARGE")
            connection = http.client.HTTPConnection("127.0.0.1", self.config.bridge_port, timeout=45)
            try:
                connection.request("POST", "/run", body, {"Authorization": "Bearer " + self.config.bridge_token, "Content-Type": "application/json"})
                response = connection.getresponse()
                raw = response.read(MAX_RESPONSE + 1)
                require(response.status == 200 and len(raw) <= MAX_RESPONSE, "SSH_BRIDGE_UNAVAILABLE")
                result = object_json(raw)
                require(set(result) == {"returncode", "stdout", "stderr"} and isinstance(result["returncode"], int) and isinstance(result["stdout"], str) and isinstance(result["stderr"], str), "INVALID_BRIDGE_RESPONSE")
                return result
            except (OSError, http.client.HTTPException):
                raise ObserveError("SSH_BRIDGE_UNAVAILABLE") from None
            finally:
                connection.close()
        try:
            process = subprocess.run(ssh_argv(self.config.ssh_host, self.config.ssh_port, self.config.known_hosts, argv), stdin=subprocess.DEVNULL, stdout=subprocess.PIPE, stderr=subprocess.PIPE, timeout=45, check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise ObserveError("SSH_UNAVAILABLE") from None
        require(len(process.stdout) + len(process.stderr) <= MAX_RESPONSE, "SSH_RESPONSE_TOO_LARGE")
        return {"returncode": process.returncode, "stdout": process.stdout.decode("utf-8", "replace"), "stderr": process.stderr.decode("utf-8", "replace")}


# This exact source is also checked by the optional local SSH transport bridge.
# No user-provided Python/shell source is accepted. All values are JSON data.
POD_PROGRAM = r'''
import base64, errno, hashlib, json, os, stat, sys
class Failure(Exception): pass
def need(ok, code):
    if not ok: raise Failure(code)
def digest(fd):
    os.lseek(fd, 0, os.SEEK_SET)
    result = hashlib.sha256()
    while True:
        data = os.read(fd, 65536)
        if not data: break
        result.update(data)
    return result.hexdigest()
def check_file(fd):
    info = os.fstat(fd)
    need(stat.S_ISREG(info.st_mode) and info.st_size <= 64*1024*1024, "MARKER_FILE_INVALID")
try:
    data = json.loads(base64.b64decode(sys.argv[1], validate=True))
    need(os.environ.get("ANI_INSTANCE_POD_UID") == data["expected_pod_uid"], "POD_UID_CHANGED")
    points = set()
    with open("/proc/self/mountinfo", encoding="utf-8") as stream:
        for line in stream:
            point = line.split()[4]
            for encoded, plain in ((r"\040", " "), (r"\011", "\t"), (r"\012", "\n"), (r"\134", "\\")):
                point = point.replace(encoded, plain)
            points.add(point)
    output = []
    for mount in data["mounts"]:
        path, name = mount["mount_path"], mount["marker_name"]
        need(path in points and os.path.realpath(path) == path, "ACTUAL_MOUNT_MISSING")
        directory = os.open(path, os.O_RDONLY|os.O_DIRECTORY|os.O_NOFOLLOW)
        try:
            readonly = bool(os.fstatvfs(directory).f_flag & os.ST_RDONLY)
            need(readonly == mount["read_only"], "ACTUAL_READ_ONLY_MISMATCH")
            written = False
            if data["event"] == "before-restart" and not mount["read_only"]:
                created = True
                try:
                    fd = os.open(name, os.O_CREAT|os.O_EXCL|os.O_RDWR|os.O_NOFOLLOW, 0o600, dir_fd=directory)
                except FileExistsError:
                    fd = os.open(name, os.O_RDWR|os.O_NOFOLLOW|os.O_NONBLOCK, dir_fd=directory)
                    created = False
                try:
                    if not created:
                        check_file(fd)
                        need(os.fstat(fd).st_nlink == 1 and digest(fd) == mount["expected_sha256"], "EXISTING_MARKER_CONFLICT")
                    content = data["marker_content"].encode()
                    os.lseek(fd, 0, os.SEEK_SET)
                    remaining = memoryview(content)
                    while remaining:
                        count = os.write(fd, remaining)
                        need(count > 0, "MARKER_WRITE_INCOMPLETE")
                        remaining = remaining[count:]
                    os.ftruncate(fd, len(content))
                    os.fsync(fd)
                    need(digest(fd) == mount["expected_sha256"], "MARKER_WRITE_HASH_CHANGED")
                    written = True
                finally:
                    os.close(fd)
                os.fsync(directory)
            fd = os.open(name, os.O_RDONLY|os.O_NOFOLLOW|os.O_NONBLOCK, dir_fd=directory)
            try:
                check_file(fd)
                actual = digest(fd)
            finally:
                os.close(fd)
            need(actual == mount["expected_sha256"], "MARKER_READ_HASH_CHANGED")
            marker = {"mount_path":path,"marker_name":name,"sha256":actual,"read_verified":True}
            if written: marker["write_verified"] = True
            if mount["read_only"]:
                probe = ".ani-restart-ro-"+data["run_id"]+"-"+data["expected_pod_uid"]+".probe"
                try:
                    fd = os.open(probe, os.O_CREAT|os.O_EXCL|os.O_WRONLY|os.O_NOFOLLOW, 0o600, dir_fd=directory)
                except OSError as failure:
                    need(failure.errno in (errno.EROFS,errno.EACCES,errno.EPERM), "READ_ONLY_PROBE_INCONCLUSIVE")
                else:
                    os.close(fd)
                    os.unlink(probe, dir_fd=directory)
                    raise Failure("READ_ONLY_WRITE_SUCCEEDED")
                marker["read_only_write_rejected"] = True
            output.append(marker)
        finally:
            os.close(directory)
    need(os.environ.get("ANI_INSTANCE_POD_UID") == data["expected_pod_uid"], "POD_UID_CHANGED")
    print(json.dumps({"markers":output}, separators=(",",":")))
except Failure as failure:
    print(json.dumps({"error":str(failure)}),file=sys.stderr)
    sys.exit(2)
except Exception:
    print('{"error":"MARKER_IO_UNAVAILABLE"}',file=sys.stderr)
    sys.exit(2)
'''.strip()


def validate_exec_payload(encoded):
    require(isinstance(encoded, str) and len(encoded) < MAX_REQUEST, "INVALID_EXEC_PAYLOAD")
    try:
        data = object_json(base64.b64decode(encoded, validate=True))
    except (ValueError, TypeError):
        raise ObserveError("INVALID_EXEC_PAYLOAD") from None
    require(set(data) == {"schema", "event", "instance_id", "run_id", "expected_pod_uid", "marker_content", "mounts"} and data["schema"] == "container-restart-marker-exec-v1" and data["event"] in ("before-restart", "after-restart"), "INVALID_EXEC_PAYLOAD")
    for key in ("instance_id", "run_id", "expected_pod_uid"):
        canonical_uuid(data[key])
    content = "ani-container-restart:" + data["run_id"] + "\n"
    require(data["marker_content"] == content and isinstance(data["mounts"], list) and 1 <= len(data["mounts"]) <= 9, "INVALID_EXEC_MARKERS")
    paths = set()
    for mount in data["mounts"]:
        require(isinstance(mount, dict) and set(mount) == {"mount_path", "marker_name", "read_only", "expected_sha256"} and isinstance(mount["read_only"], bool), "INVALID_EXEC_MOUNT")
        path = canonical_path(mount["mount_path"])
        require(path not in paths, "DUPLICATE_EXEC_MOUNT")
        paths.add(path)
        marker_name(mount["marker_name"])
        require(isinstance(mount["expected_sha256"], str) and SHA256.fullmatch(mount["expected_sha256"]), "INVALID_EXEC_HASH")
        if not mount["read_only"]:
            require(mount["marker_name"] == ".ani-restart-" + data["run_id"] + ".txt" and mount["expected_sha256"] == hashlib.sha256(content.encode()).hexdigest(), "SYNTHETIC_EXEC_MARKER_REQUIRED")
    return data


class Kubectl:
    def __init__(self, config, transport):
        self.config, self.transport = config, transport

    def command(self, args):
        result = self.transport.run(KUBECTL_PREFIX + args)
        if result["returncode"] != 0:
            stderr = result["stderr"]
            if "python3" in stderr and ("not found" in stderr or "executable file" in stderr):
                raise ObserveError("EXEC_RUNTIME_UNAVAILABLE")
            for line in reversed(stderr.splitlines()):
                try:
                    error = json.loads(line).get("error")
                    if isinstance(error, str) and re.fullmatch(r"[A-Z][A-Z0-9_]{0,80}", error):
                        raise ObserveError(error)
                except (ValueError, AttributeError):
                    pass
            raise ObserveError("CLUSTER_COMMAND_UNAVAILABLE")
        return result["stdout"]

    def get(self, kind, name, missing=False):
        args = ["get", kind, name]
        if kind != "namespace":
            args += ["-n", self.config.namespace]
        args += ["-o", "json"]
        if missing:
            args += ["--ignore-not-found=true"]
        raw = self.command(args)
        if missing and not raw.strip():
            return None
        return object_json(raw)


def metadata(obj, kind, namespace, name=None):
    require(isinstance(obj, dict) and obj.get("kind") == kind and isinstance(obj.get("metadata"), dict), "OBJECT_KIND_MISMATCH")
    meta = obj["metadata"]
    require(meta.get("namespace", "") == namespace and (name is None or meta.get("name") == name) and not meta.get("deletionTimestamp"), "OBJECT_IDENTITY_MISMATCH")
    canonical_uuid(meta.get("uid"))
    return meta


def same_control_snapshot(before, after):
    # A status subresource write advances resourceVersion. Accept that change
    # only after comparing every other field, including full spec, generation,
    # UID, owners, labels, all annotations and deletion metadata. managedFields
    # is deliberately retained; this is not an allowlist of business fields.
    def control_fields(obj):
        result = {key: value for key, value in obj.items() if key != "status"}
        result["metadata"] = {key: value for key, value in obj.get("metadata", {}).items() if key != "resourceVersion"}
        return result
    return control_fields(before) == control_fields(after)


def controlled(obj, group, kind, name, uid):
    controllers = [ref for ref in obj.get("metadata", {}).get("ownerReferences", []) if ref.get("controller") is True]
    return len(controllers) == 1 and controllers[0].get("apiVersion", "").split("/")[0] == group and controllers[0].get("kind") == kind and controllers[0].get("name") == name and controllers[0].get("uid") == uid


def managed_labels(meta, tenant, instance, create=None):
    labels = meta.get("labels", {})
    require(labels.get(TENANT_LABEL) == tenant and labels.get(RESOURCE_LABEL) == instance and labels.get(OWNER_LABEL) == OWNER, "WORKLOAD_OWNER_MISMATCH")
    canonical_uuid(labels.get(CREATE_LABEL))
    if create is not None:
        require(labels[CREATE_LABEL] == create, "CREATE_OWNER_MISMATCH")
    return labels[CREATE_LABEL]


def verify_template(spec, expected):
    containers = spec.get("containers", [])
    require(len(containers) == 1 and containers[0].get("name") == "workspace", "MANAGED_CONTAINER_MISMATCH")
    actual_volumes, actual_mounts = {}, {}
    for volume in spec.get("volumes", []):
        if "persistentVolumeClaim" in volume:
            name = volume.get("name")
            require(name not in actual_volumes, "DUPLICATE_PVC_VOLUME")
            actual_volumes[name] = volume["persistentVolumeClaim"]
    for mount in containers[0].get("volumeMounts", []):
        name = mount.get("name")
        if name in expected:
            require(name not in actual_mounts and "subPath" not in mount and "subPathExpr" not in mount, "MANAGED_MOUNT_MISMATCH")
            actual_mounts[name] = mount
    require(set(actual_volumes) == set(expected) == set(actual_mounts), "CURRENT_MOUNT_TEMPLATE_MISMATCH")
    for name, mount in expected.items():
        volume, actual = actual_volumes[name], actual_mounts[name]
        require(volume.get("claimName") == mount["pvc_name"] and volume.get("readOnly", False) is mount["read_only"] and actual.get("mountPath") == mount["mount_path"] and actual.get("readOnly", False) is mount["read_only"], "CURRENT_MOUNT_TEMPLATE_MISMATCH")
    return containers[0]


def ready(pod):
    status = pod.get("status", {})
    return status.get("phase") == "Running" and any(item.get("type") == "Ready" and item.get("status") == "True" for item in status.get("conditions", [])) and any(item.get("name") == "workspace" and item.get("ready") is True for item in status.get("containerStatuses", []))


def save_state(path, value):
    temporary = path.with_name(path.name + "." + str(uuid.uuid4()) + ".tmp")
    fd = os.open(temporary, os.O_WRONLY | os.O_CREAT | os.O_EXCL | os.O_NOFOLLOW, 0o600)
    try:
        with os.fdopen(fd, "wb") as stream:
            stream.write(json.dumps(value, sort_keys=True, separators=(",", ":")).encode() + b"\n")
            stream.flush()
            os.fsync(stream.fileno())
        os.replace(temporary, path)
        directory = os.open(path.parent, os.O_RDONLY | os.O_DIRECTORY)
        try:
            os.fsync(directory)
        finally:
            os.close(directory)
    finally:
        if temporary.exists():
            temporary.unlink()


def observe(request, config, transport=None):
    request = validate_request(request)
    transport = transport or Transport(config)
    kube = Kubectl(config, transport)
    config.state_dir.mkdir(parents=True, mode=0o700, exist_ok=True)
    info = config.state_dir.lstat()
    require(stat.S_ISDIR(info.st_mode) and info.st_uid == os.geteuid() and stat.S_IMODE(info.st_mode) == 0o700, "PRIVATE_STATE_OWNER_MODE_REQUIRED")
    key = request["run_id"] + "-" + request["instance_id"]
    state_path = config.state_dir / (key + ".json")
    lock_fd = os.open(config.state_dir / (key + ".lock"), os.O_CREAT | os.O_RDWR | os.O_NOFOLLOW, 0o600)
    try:
        try:
            fcntl.flock(lock_fd, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise ObserveError("OBSERVATION_ALREADY_RUNNING") from None
        baseline = object_json(private_file(state_path)) if state_path.exists() else None
        if baseline:
            require(baseline.get("schema") == "container-restart-live-observer-state-v1" and baseline.get("namespace") == config.namespace and baseline.get("instance_id") == request["instance_id"] and baseline.get("run_id") == request["run_id"], "BASELINE_SCOPE_MISMATCH")
        if request["event"] == "after-restart":
            require(baseline and baseline.get("before"), "BEFORE_OBSERVATION_REQUIRED")
        identity = kube.command(CONFIG_QUERY).strip().splitlines()
        require(identity == [config.context, config.endpoint], "CLUSTER_IDENTITY_CHANGED")
        namespace = kube.get("namespace", config.namespace)
        ns_meta = metadata(namespace, "Namespace", "", config.namespace)
        ns_labels = ns_meta.get("labels", {})
        tenant = canonical_uuid(ns_labels.get("network.ani.io/tenant-id"))
        require(ns_labels.get("network.ani.io/managed-by") == "ani-network-service" and (config.tenant_id is None or config.tenant_id == tenant) and (config.namespace_uid is None or config.namespace_uid == ns_meta["uid"]), "NAMESPACE_OWNER_MISMATCH")
        instance_id, name = request["instance_id"], "instance-" + request["instance_id"]
        notebook = kube.get("notebooks.kubeflow.org", name)
        nb_meta = metadata(notebook, "Notebook", config.namespace, name)
        create = managed_labels(nb_meta, tenant, instance_id)
        require("kubeflow-resource-stopped" not in nb_meta.get("annotations", {}), "NOTEBOOK_IS_STOPPED")
        sts = kube.get("statefulsets.apps", name)
        sts_meta = metadata(sts, "StatefulSet", config.namespace, name)
        require(controlled(sts, "kubeflow.org", "Notebook", name, nb_meta["uid"]) and sts.get("spec", {}).get("replicas", 1) == 1 and sts.get("status", {}).get("observedGeneration", 0) >= sts_meta.get("generation", 1), "STATEFULSET_OWNER_OR_GENERATION_MISMATCH")
        pod_list = object_json(kube.command(["get", "pods", "-n", config.namespace, "-o", "json"]))
        pods = [pod for pod in pod_list.get("items", []) if controlled(pod, "apps", "StatefulSet", name, sts_meta["uid"])]
        require(len(pods) == 1, "CURRENT_POD_AMBIGUOUS")
        pod = pods[0]
        pod_meta = metadata(pod, "Pod", config.namespace)
        require(re.fullmatch(re.escape(name) + r"-[0-9]+", pod_meta.get("name", "")) and ready(pod), "CURRENT_POD_NOT_READY")
        managed_labels(pod_meta, tenant, instance_id, create)
        expected, exec_mounts, mount_ids = {}, [], []
        for index, mount in enumerate(request["mounts"]):
            claim = "workspace-" + instance_id if mount["ownership"] == "workspace" else mount["pvc_name"]
            volume_name = "workspace" if index == 0 else "borrowed-" + str(index - 1)
            expected[volume_name] = dict(mount, pvc_name=claim)
            pvc = kube.get("persistentvolumeclaims", claim)
            pvc_meta = metadata(pvc, "PersistentVolumeClaim", config.namespace, claim)
            pvc_labels = pvc_meta.get("labels", {})
            if index == 0:
                managed_labels(pvc_meta, tenant, instance_id, create)
            else:
                require(pvc_labels.get(TENANT_LABEL, tenant) == tenant and pvc_labels.get("storage.ani.io/tenant-id", tenant) == tenant and not any(ref.get("kind") == "Notebook" for ref in pvc_meta.get("ownerReferences", [])), "BORROWED_PVC_OWNER_MISMATCH")
            require(pvc.get("spec", {}).get("volumeMode", "Filesystem") == "Filesystem", "FILESYSTEM_PVC_REQUIRED")
            mount_ids.append({"mount_path": mount["mount_path"], "pvc_uid": pvc_meta["uid"]})
            entry = {"mount_path": mount["mount_path"], "read_only": mount["read_only"], "marker_name": request["marker_name"], "expected_sha256": request["marker_sha256"]}
            if mount["read_only"]:
                frozen = config.read_only_markers.get(mount["mount_path"])
                require(isinstance(frozen, dict) and set(frozen) == {"marker_name", "sha256"} and SHA256.fullmatch(frozen.get("sha256", "")), "READ_ONLY_EXISTING_MARKER_REQUIRED")
                entry["marker_name"], entry["expected_sha256"] = marker_name(frozen["marker_name"]), frozen["sha256"]
            exec_mounts.append(entry)
        verify_template(notebook.get("spec", {}).get("template", {}).get("spec", {}), expected)
        verify_template(sts.get("spec", {}).get("template", {}).get("spec", {}), expected)
        container = verify_template(pod.get("spec", {}), expected)
        uid_env = [item for item in container.get("env", []) if item.get("name") == "ANI_INSTANCE_POD_UID"]
        require(len(uid_env) == 1 and uid_env[0].get("valueFrom", {}).get("fieldRef", {}).get("fieldPath") == "metadata.uid" and "value" not in uid_env[0], "POD_EXEC_UID_GUARD_MISSING")
        current = {"namespace_uid": ns_meta["uid"], "tenant_id": tenant, "notebook_uid": nb_meta["uid"], "workspace_pvc_uid": mount_ids[0]["pvc_uid"], "pod_uid": pod_meta["uid"], "pod_name": pod_meta["name"], "mount_identities": mount_ids, "mounts": request["mounts"]}
        if baseline:
            before = baseline["before"]
            for field in ("namespace_uid", "tenant_id", "notebook_uid", "workspace_pvc_uid"):
                require(before.get(field) == current[field], "PERSISTENT_OBJECT_IDENTITY_CHANGED")
        if baseline and request["event"] == "after-restart":
            for field in ("mount_identities", "mounts"):
                require(before.get(field) == current[field], "PERSISTENT_OBJECT_IDENTITY_CHANGED")
            require(request["old_pod"] == {"name": before["pod_name"], "uid": before["pod_uid"]} and pod_meta["uid"] != before["pod_uid"], "RESTART_POD_IDENTITY_NOT_CHANGED")
        payload = {"schema": "container-restart-marker-exec-v1", "event": request["event"], "instance_id": instance_id, "run_id": request["run_id"], "expected_pod_uid": pod_meta["uid"], "marker_content": request["marker_content"], "mounts": exec_mounts}
        encoded = base64.b64encode(json.dumps(payload, separators=(",", ":")).encode()).decode("ascii")
        validate_exec_payload(encoded)
        marker_result = object_json(kube.command(["exec", pod_meta["name"], "-n", config.namespace, "-c", "workspace", "--", "python3", "-c", POD_PROGRAM, encoded]))
        markers = marker_result.get("markers")
        require(isinstance(markers, list) and len(markers) == len(exec_mounts), "MARKER_EXEC_PROOF_INCOMPLETE")
        for marker, frozen in zip(markers, exec_mounts):
            require(isinstance(marker, dict) and marker.get("mount_path") == frozen["mount_path"] and marker.get("marker_name") == frozen["marker_name"] and marker.get("sha256") == frozen["expected_sha256"] and marker.get("read_verified") is True, "MARKER_EXEC_PROOF_MISMATCH")
            if request["event"] == "before-restart" and not frozen["read_only"]:
                require(marker.get("write_verified") is True, "MARKER_WRITE_NOT_PROVEN")
            if frozen["read_only"]:
                require(marker.get("read_only_write_rejected") is True, "READ_ONLY_WRITE_FAILURE_NOT_PROVEN")
        result = {"source": "live", "instance_id": instance_id, "run_id": request["run_id"], "observed_at": dt.datetime.now(dt.timezone.utc).isoformat().replace("+00:00", "Z"), **{key: current[key] for key in ("notebook_uid", "workspace_pvc_uid", "pod_uid", "pod_name", "mount_identities")}, "markers": markers}
        if request["event"] == "after-restart":
            old = request["old_pod"]
            previous = kube.get("pods", old["name"], missing=True)
            observed_uid = None if previous is None else canonical_uuid(previous.get("metadata", {}).get("uid"))
            require(observed_uid != old["uid"], "OLD_POD_STILL_EXISTS")
            result["old_pod_get"] = {"name": old["name"], "expected_uid": old["uid"], "method": "direct_get", "not_found": previous is None, "observed_uid": observed_uid}
        # Re-read the exact identities after exec. These are point-in-time
        # observations, not a claim of protection against out-of-platform deletes.
        final_pod = kube.get("pods", pod_meta["name"])
        final_meta = metadata(final_pod, "Pod", config.namespace, pod_meta["name"])
        final_nb = kube.get("notebooks.kubeflow.org", name)
        final_nb_meta = metadata(final_nb, "Notebook", config.namespace, name)
        require(final_meta["uid"] == pod_meta["uid"] and ready(final_pod) and controlled(final_pod, "apps", "StatefulSet", name, sts_meta["uid"]) and final_nb_meta["uid"] == nb_meta["uid"] and same_control_snapshot(pod, final_pod) and same_control_snapshot(notebook, final_nb), "OBJECT_CHANGED_DURING_OBSERVATION")
        final_sts = kube.get("statefulsets.apps", name)
        final_sts_meta = metadata(final_sts, "StatefulSet", config.namespace, name)
        require(final_sts_meta["uid"] == sts_meta["uid"] and final_sts_meta.get("generation") == sts_meta.get("generation") and controlled(final_sts, "kubeflow.org", "Notebook", name, nb_meta["uid"]) and final_sts.get("spec", {}).get("replicas", 1) == 1 and final_sts.get("status", {}).get("observedGeneration", 0) >= final_sts_meta.get("generation", 1) and same_control_snapshot(sts, final_sts), "OBJECT_CHANGED_DURING_OBSERVATION")
        verify_template(final_pod.get("spec", {}), expected)
        for mount, frozen in zip(request["mounts"], mount_ids):
            claim = "workspace-" + instance_id if mount["ownership"] == "workspace" else mount["pvc_name"]
            final_pvc_meta = metadata(kube.get("persistentvolumeclaims", claim), "PersistentVolumeClaim", config.namespace, claim)
            require(final_pvc_meta["uid"] == frozen["pvc_uid"], "OBJECT_CHANGED_DURING_OBSERVATION")
        final_ns = metadata(kube.get("namespace", config.namespace), "Namespace", "", config.namespace)
        require(final_ns["uid"] == ns_meta["uid"] and final_ns.get("labels", {}).get("network.ani.io/tenant-id") == tenant and final_ns.get("labels", {}).get("network.ani.io/managed-by") == "ani-network-service", "OBJECT_CHANGED_DURING_OBSERVATION")
        if request["event"] == "before-restart":
            state = {"schema": "container-restart-live-observer-state-v1", "namespace": config.namespace, "instance_id": instance_id, "run_id": request["run_id"], "before": dict(current, markers=markers)}
        else:
            state = dict(baseline, after=result)
        save_state(state_path, state)
        return result
    finally:
        os.close(lock_fd)


def main():
    try:
        release = Path("/etc/os-release").read_text()
        require(re.search(r'^ID=["\']?fedora["\']?$', release, re.MULTILINE), "FEDORA_EXECUTION_REQUIRED")
        raw = sys.stdin.buffer.read(MAX_REQUEST + 1)
        require(len(raw) <= MAX_REQUEST, "OBSERVATION_REQUEST_TOO_LARGE")
        result = observe(object_json(raw), Config(os.environ))
        sys.stdout.write(json.dumps(result, sort_keys=True, separators=(",", ":")) + "\n")
        return 0
    except ObserveError as failure:
        sys.stderr.write(json.dumps({"error": str(failure)}, separators=(",", ":")) + "\n")
        return 2
    except Exception:
        sys.stderr.write('{"error":"OBSERVATION_UNAVAILABLE"}\n')
        return 2


if __name__ == "__main__":
    raise SystemExit(main())
