#!/usr/bin/env python3
"""Offline boundary tests; run on Fedora, never against a cluster."""

import base64
import copy
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import shlex
import tempfile
import unittest


SOURCE = Path(__file__).with_name("container-restart-live-observe.py")
SPEC = importlib.util.spec_from_file_location("container_restart_live_observe", SOURCE)
observer = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(observer)

INSTANCE = "11111111-1111-4111-8111-111111111111"
RUN = "22222222-2222-4222-8222-222222222222"
TENANT = "33333333-3333-4333-8333-333333333333"
NAMESPACE_UID = "44444444-4444-4444-8444-444444444444"
NOTEBOOK_UID = "55555555-5555-4555-8555-555555555555"
STATEFULSET_UID = "66666666-6666-4666-8666-666666666666"
WORKSPACE_UID = "77777777-7777-4777-8777-777777777777"
OLD_POD_UID = "88888888-8888-4888-8888-888888888888"
NEW_POD_UID = "99999999-9999-4999-8999-999999999999"
CREATE = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
OTHER_UID = "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
NAMESPACE = "tenant-restart-acceptance"
NAME = "instance-" + INSTANCE
POD_NAME = NAME + "-0"
CLAIM = "workspace-" + INSTANCE


def request(event="before-restart"):
    content = "ani-container-restart:" + RUN + "\n"
    return {
        "schema": "container-restart-live-observation-v1",
        "event": event,
        "run_id": RUN,
        "instance_id": INSTANCE,
        "operation_id": None if event == "before-restart" else CREATE,
        "mounts": [{"mount_path": "/workspace", "read_only": False, "ownership": "workspace"}],
        "marker_name": ".ani-restart-" + RUN + ".txt",
        "marker_content": content,
        "marker_sha256": hashlib.sha256(content.encode()).hexdigest(),
        "old_pod": None if event == "before-restart" else {"name": POD_NAME, "uid": OLD_POD_UID},
    }


def labels():
    return {
        observer.TENANT_LABEL: TENANT,
        observer.RESOURCE_LABEL: INSTANCE,
        observer.OWNER_LABEL: observer.OWNER,
        observer.CREATE_LABEL: CREATE,
    }


def meta(name, uid, namespace=NAMESPACE, **extra):
    return {"name": name, "uid": uid, "namespace": namespace, "resourceVersion": "31", **extra}


def controller(group, kind, name, uid):
    return {"apiVersion": group + "/v1", "kind": kind, "name": name, "uid": uid, "controller": True}


def template():
    return {
        "containers": [{
            "name": "workspace",
            "env": [{"name": "ANI_INSTANCE_POD_UID", "valueFrom": {"fieldRef": {"fieldPath": "metadata.uid"}}}],
            "volumeMounts": [{"name": "workspace", "mountPath": "/workspace", "readOnly": False}],
        }],
        "volumes": [{"name": "workspace", "persistentVolumeClaim": {"claimName": CLAIM, "readOnly": False}}],
    }


class ClusterBoundary:
    """Only the external kubectl boundary is doubled; observer logic is real."""

    def __init__(self, pod_uid=OLD_POD_UID, after_exec=None):
        self.namespace = {
            "kind": "Namespace",
            "metadata": meta(NAMESPACE, NAMESPACE_UID, "", labels={
                "network.ani.io/tenant-id": TENANT,
                "network.ani.io/managed-by": "ani-network-service",
            }),
        }
        self.notebook = {
            "kind": "Notebook", "metadata": meta(NAME, NOTEBOOK_UID, labels=labels()),
            "spec": {"template": {"spec": template()}},
        }
        self.statefulset = {
            "kind": "StatefulSet",
            "metadata": meta(NAME, STATEFULSET_UID, generation=2, ownerReferences=[controller("kubeflow.org", "Notebook", NAME, NOTEBOOK_UID)]),
            "spec": {"replicas": 1, "template": {"spec": template()}},
            "status": {"observedGeneration": 2},
        }
        self.pod = {
            "kind": "Pod",
            "metadata": meta(POD_NAME, pod_uid, labels=labels(), ownerReferences=[controller("apps", "StatefulSet", NAME, STATEFULSET_UID)]),
            "spec": template(),
            "status": {"phase": "Running", "conditions": [{"type": "Ready", "status": "True"}], "containerStatuses": [{"name": "workspace", "ready": True}]},
        }
        self.workspace = {
            "kind": "PersistentVolumeClaim", "metadata": meta(CLAIM, WORKSPACE_UID, labels=labels()),
            "spec": {"volumeMode": "Filesystem"},
        }
        self.exec_payloads = []
        self.calls = []
        self.old_pod_lookup = "replacement"
        self.after_exec = after_exec

    def run(self, argv):
        self.calls.append(list(argv))
        if argv[:len(observer.KUBECTL_PREFIX)] != observer.KUBECTL_PREFIX:
            raise AssertionError("unexpected command prefix")
        args = argv[len(observer.KUBECTL_PREFIX):]
        if args == observer.CONFIG_QUERY:
            return self.output("kubernetes-admin@ani-kcn-full\nhttps://lb.kubesphere.local:6443")
        if args == ["get", "pods", "-n", NAMESPACE, "-o", "json"]:
            return self.output({"kind": "PodList", "items": [self.pod]})
        if args[0] == "get":
            kind, name = args[1:3]
            expected = ["get", kind, name]
            if kind != "namespace":
                expected += ["-n", NAMESPACE]
            expected += ["-o", "json"]
            missing = args[-1] == "--ignore-not-found=true"
            if missing:
                expected += ["--ignore-not-found=true"]
            if args != expected:
                raise AssertionError("unexpected kubectl get arguments")
            objects = {
                ("namespace", NAMESPACE): self.namespace,
                ("notebooks.kubeflow.org", NAME): self.notebook,
                ("statefulsets.apps", NAME): self.statefulset,
                ("persistentvolumeclaims", CLAIM): self.workspace,
                ("pods", POD_NAME): self.pod,
            }
            if (kind, name) == ("pods", POD_NAME) and missing:
                if self.old_pod_lookup == "missing":
                    return self.output("")
                if self.old_pod_lookup == "old":
                    old = copy.deepcopy(self.pod)
                    old["metadata"]["uid"] = OLD_POD_UID
                    return self.output(old)
            if (kind, name) not in objects:
                raise AssertionError("unexpected resource/name")
            return self.output(objects[(kind, name)])
        if args[0] == "exec":
            self_expected = ["exec", POD_NAME, "-n", NAMESPACE, "-c", "workspace", "--", "python3", "-c", observer.POD_PROGRAM]
            if args[:-1] != self_expected:
                raise AssertionError("observer must execute only its fixed Python program")
            data = observer.validate_exec_payload(args[-1])
            if data["expected_pod_uid"] != self.pod["metadata"]["uid"]:
                raise AssertionError("expected UID was not frozen before exec")
            self.exec_payloads.append(data)
            markers = []
            for mount in data["mounts"]:
                marker = {"mount_path": mount["mount_path"], "marker_name": mount["marker_name"], "sha256": mount["expected_sha256"], "read_verified": True}
                if data["event"] == "before-restart":
                    marker["write_verified"] = True
                markers.append(marker)
            response = self.output({"markers": markers})
            if self.after_exec is not None:
                callback, self.after_exec = self.after_exec, None
                callback(self)
            return response
        raise AssertionError("unexpected kubectl operation")

    @staticmethod
    def output(value):
        return {"returncode": 0, "stdout": value if isinstance(value, str) else json.dumps(value), "stderr": ""}


class LiveObservationTest(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.config = observer.Config({"HOME": self.temporary.name, "ANI_ACCEPTANCE_TENANT_NAMESPACE": NAMESPACE})

    def before(self):
        boundary = ClusterBoundary()
        result = observer.observe(request(), self.config, boundary)
        return boundary, result

    def test_cpu_workspace_before_after_preserves_identity_and_marker(self):
        before_boundary, before = self.before()
        after_boundary = ClusterBoundary(NEW_POD_UID)
        after = observer.observe(request("after-restart"), self.config, after_boundary)
        self.assertEqual(before["source"], "live")
        self.assertEqual(before["pod_uid"], OLD_POD_UID)
        self.assertEqual(after["pod_uid"], NEW_POD_UID)
        for field in ("notebook_uid", "workspace_pvc_uid", "mount_identities"):
            self.assertEqual(before[field], after[field])
        self.assertEqual(before["markers"][0]["sha256"], after["markers"][0]["sha256"])
        self.assertTrue(before["markers"][0]["write_verified"])
        self.assertNotIn("write_verified", after["markers"][0])
        self.assertEqual(after["old_pod_get"], {"name": POD_NAME, "expected_uid": OLD_POD_UID, "method": "direct_get", "not_found": False, "observed_uid": NEW_POD_UID})
        self.assertEqual([item["event"] for item in before_boundary.exec_payloads + after_boundary.exec_payloads], ["before-restart", "after-restart"])
        state = self.config.state_dir / (RUN + "-" + INSTANCE + ".json")
        self.assertEqual(os.stat(state).st_mode & 0o777, 0o600)
        self.assertEqual(json.loads(state.read_text())["after"]["pod_uid"], NEW_POD_UID)

    def test_old_pod_direct_get_absence_is_preserved(self):
        self.before()
        boundary = ClusterBoundary(NEW_POD_UID)
        boundary.old_pod_lookup = "missing"
        result = observer.observe(request("after-restart"), self.config, boundary)
        self.assertTrue(result["old_pod_get"]["not_found"])
        self.assertIsNone(result["old_pod_get"]["observed_uid"])

    def test_same_name_workspace_replacement_is_rejected_before_exec(self):
        self.before()
        boundary = ClusterBoundary(NEW_POD_UID)
        boundary.workspace["metadata"]["uid"] = OTHER_UID
        with self.assertRaisesRegex(observer.ObserveError, "^PERSISTENT_OBJECT_IDENTITY_CHANGED$"):
            observer.observe(request("after-restart"), self.config, boundary)
        self.assertEqual(boundary.exec_payloads, [])

    def test_foreign_pod_owner_is_rejected_before_exec(self):
        boundary = ClusterBoundary()
        boundary.pod["metadata"]["ownerReferences"][0]["uid"] = OTHER_UID
        with self.assertRaisesRegex(observer.ObserveError, "^CURRENT_POD_AMBIGUOUS$"):
            observer.observe(request(), self.config, boundary)
        self.assertEqual(boundary.exec_payloads, [])

    def test_old_uid_still_present_is_rejected(self):
        self.before()
        boundary = ClusterBoundary(NEW_POD_UID)
        boundary.old_pod_lookup = "old"
        with self.assertRaisesRegex(observer.ObserveError, "^OLD_POD_STILL_EXISTS$"):
            observer.observe(request("after-restart"), self.config, boundary)
        self.assertEqual(len(boundary.exec_payloads), 1)
        self.assertEqual(boundary.exec_payloads[0]["event"], "after-restart")
        state = self.config.state_dir / (RUN + "-" + INSTANCE + ".json")
        self.assertNotIn("after", json.loads(state.read_text()))

    def test_namespace_identity_mismatch_is_rejected_before_exec(self):
        boundary = ClusterBoundary()
        boundary.namespace["metadata"]["name"] = "foreign-tenant"
        with self.assertRaisesRegex(observer.ObserveError, "^OBJECT_IDENTITY_MISMATCH$"):
            observer.observe(request(), self.config, boundary)
        self.assertEqual(boundary.exec_payloads, [])

    def test_after_requires_private_before_state(self):
        boundary = ClusterBoundary(NEW_POD_UID)
        with self.assertRaisesRegex(observer.ObserveError, "^BEFORE_OBSERVATION_REQUIRED$"):
            observer.observe(request("after-restart"), self.config, boundary)
        self.assertEqual(boundary.calls, [])

    def test_status_and_resource_version_churn_during_marker_exec_is_allowed(self):
        for target in ("notebook", "statefulset", "pod"):
            with self.subTest(target=target):
                self.config.state_dir = Path(self.temporary.name) / (target + "-status")

                def status_update(boundary):
                    obj = getattr(boundary, target)
                    obj["metadata"]["resourceVersion"] = "32"
                    obj.setdefault("status", {}).setdefault("conditions", []).append({"type": "ControllerObserved", "status": "True", "lastTransitionTime": "2026-10-28T12:00:00Z"})

                boundary = ClusterBoundary(after_exec=status_update)
                result = observer.observe(request(), self.config, boundary)
                self.assertEqual(result["pod_uid"], OLD_POD_UID)
                self.assertTrue(result["markers"][0]["read_verified"])
                self.assertEqual(len(boundary.exec_payloads), 1)
                self.assertIsNone(boundary.after_exec)

    def test_control_changes_during_marker_exec_are_rejected(self):
        for target in ("notebook", "statefulset", "pod"):
            for change in ("nonmount_spec", "uid", "mutation_annotation"):
                with self.subTest(target=target, change=change):
                    self.config.state_dir = Path(self.temporary.name) / (target + "-" + change)

                    def control_update(boundary):
                        obj = getattr(boundary, target)
                        if change == "uid":
                            obj["metadata"]["uid"] = OTHER_UID
                        elif change == "mutation_annotation":
                            obj["metadata"].setdefault("annotations", {})["instance.ani.io/mutation-id"] = OTHER_UID
                        else:
                            spec = obj["spec"] if target == "pod" else obj["spec"]["template"]["spec"]
                            spec["containers"][0]["image"] = "registry.invalid/changed@sha256:" + "c" * 64

                    boundary = ClusterBoundary(after_exec=control_update)
                    with self.assertRaises(observer.ObserveError):
                        observer.observe(request(), self.config, boundary)
                    self.assertEqual(len(boundary.exec_payloads), 1)
                    self.assertIsNone(boundary.after_exec)
                    state = self.config.state_dir / (RUN + "-" + INSTANCE + ".json")
                    self.assertFalse(state.exists(), "a changed control target must not save a successful before proof")


class InputSafetyTest(unittest.TestCase):
    def test_exec_data_is_fixed_synthetic_marker_and_canonical_mount(self):
        original = request()
        payload = {
            "schema": "container-restart-marker-exec-v1", "event": original["event"],
            "instance_id": INSTANCE, "run_id": RUN, "expected_pod_uid": OLD_POD_UID,
            "marker_content": original["marker_content"],
            "mounts": [{"mount_path": "/workspace", "marker_name": original["marker_name"], "read_only": False, "expected_sha256": original["marker_sha256"]}],
        }
        def encode(value):
            return base64.b64encode(json.dumps(value).encode()).decode("ascii")
        self.assertEqual(observer.validate_exec_payload(encode(payload)), payload)
        for path in ("/workspace/../secrets", "/workspace//nested", "/workspace\nother", "/workspace\\other"):
            changed = copy.deepcopy(payload)
            changed["mounts"][0]["mount_path"] = path
            with self.subTest(path=path), self.assertRaises(observer.ObserveError):
                observer.validate_exec_payload(encode(changed))
        changed = copy.deepcopy(payload)
        changed["mounts"][0]["marker_name"] = "unrelated-business-file"
        with self.assertRaisesRegex(observer.ObserveError, "^SYNTHETIC_EXEC_MARKER_REQUIRED$"):
            observer.validate_exec_payload(encode(changed))

    def test_remote_argv_is_shell_quoted_without_data_interpolation(self):
        argv = ["kubectl", "exec", POD_NAME, "--", "python3", "-c", observer.POD_PROGRAM, "$(touch /tmp/forbidden); 'quoted'\nnext"]
        command = observer.ssh_argv("100.64.128.12", 22, "/tmp/task-known-hosts", argv)
        self.assertEqual(shlex.split(command[-1]), argv)
        self.assertEqual(command[-2], "kubercloud@100.64.128.12")
        self.assertIn("StrictHostKeyChecking=yes", command)
        self.assertIn("HostKeyAlias=100.64.128.12", command)

    def test_transport_target_and_bridge_scope_are_closed(self):
        for variable, value in (("ANI_ACCEPTANCE_SSH_HOST", "other-host"), ("ANI_ACCEPTANCE_SSH_HOST", "100.64.128.12;id"), ("ANI_ACCEPTANCE_SSH_PORT", "22 -o ProxyCommand=id"), ("ANI_ACCEPTANCE_SSH_BRIDGE_URL", "http://example.com:39399")):
            with self.subTest(variable=variable, value=value), self.assertRaises(observer.ObserveError):
                observer.Config({"HOME": "/tmp", "ANI_ACCEPTANCE_TENANT_NAMESPACE": NAMESPACE, variable: value})

    def test_request_rejects_marker_redirection_and_overlapping_mounts(self):
        redirected = request()
        redirected["marker_name"] = "../../business-file"
        with self.assertRaisesRegex(observer.ObserveError, "^SYNTHETIC_MARKER_REQUIRED$"):
            observer.validate_request(redirected)
        overlapped = request()
        overlapped["mounts"].append({"mount_path": "/workspace/datasets", "read_only": False, "ownership": "borrowed", "pvc_name": "research-data"})
        with self.assertRaisesRegex(observer.ObserveError, "^OVERLAPPING_MOUNTS$"):
            observer.validate_request(overlapped)


if __name__ == "__main__":
    unittest.main()
