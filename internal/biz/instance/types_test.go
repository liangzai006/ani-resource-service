package instance

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCPUAdmissionRejectsGPUState(t *testing.T) {
	const id = "11111111-1111-4111-8111-111111111111"
	c := Create{TenantID: id, ID: id, OperationID: id, RequestHash: strings.Repeat("a", 64), BusinessDigest: strings.Repeat("b", 64), Spec: Spec{ContractVersion: 2, WorkspaceMountPath: "/workspace", InstanceKind: KindContainer, Scheduling: Scheduling{PodPlacement: "NONE"}, ComputeType: TypeCPU, ImageReference: "registry.invalid/work@sha256:" + strings.Repeat("c", 64), ImageDigest: "sha256:" + strings.Repeat("c", 64), CPU: "1", Memory: "2Gi", UID: 1000, GID: 1000, Shell: "/bin/bash", ServiceAccount: "workload", StorageClass: "ani-block", WorkspaceSize: "1Gi", ClusterID: id, Namespace: "tenant-" + id, NotebookName: "instance-" + id, PVCName: "workspace-" + id}}
	if err := ValidateCreate(c); err != nil {
		t.Fatal("fixed CPU specification rejected", err)
	}
	// PostgreSQL snapshots use the real JSON representation, in which a nil
	// RawMessage becomes null and decodes to non-empty bytes spelling "null".
	snapshot, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var restored Create
	if err := json.Unmarshal(snapshot, &restored); err != nil {
		t.Fatal(err)
	}
	if err := ValidateCreate(restored); err != nil || !restored.Spec.GPU.Empty() {
		t.Fatal("persisted CPU snapshot gained a GPU plan", err)
	}
	for name, mutate := range map[string]func(*Create){
		"limit":              func(c *Create) { c.Spec.GPU.Limits = map[string]string{"volcano.sh/vgpu-number": "1"} },
		"selector":           func(c *Create) { c.Spec.GPU.NodeLabels = map[string]string{"gpu": "true"} },
		"annotation":         func(c *Create) { c.Spec.GPU.Annotations = map[string]string{"volcano.sh/vgpu-mode": "hami-core"} },
		"scheduler":          func(c *Create) { c.Spec.GPU.Scheduler = "volcano" },
		"queue":              func(c *Create) { c.Spec.GPU.Queue = "default" },
		"unknown type":       func(c *Create) { c.Spec.ComputeType = "automatic" },
		"empty compute type": func(c *Create) { c.Spec.ComputeType = "" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := c
			mutate(&changed)
			if err := ValidateCreate(changed); !errors.Is(err, ErrInvalid) {
				t.Fatal("invalid CPU resource contract accepted", err)
			}
		})
	}
	d := Delete{InstanceKind: KindContainer, ComputeType: TypeCPU, TenantID: id, ID: id, OperationID: id, CreateOperationID: id, RequestHash: c.RequestHash}
	if err := ValidateDelete(d); err != nil {
		t.Fatal("CPU delete required a GPU plan", err)
	}
	if err := ValidateCreate(Create{}); !errors.Is(err, ErrInvalid) {
		t.Fatal("missing instance classification accepted")
	}
}

func TestOwnerCleanupReadyRequiresEntireClosedInstance(t *testing.T) {
	ready := Instance{
		Spec: Spec{ComputeType: TypeGPU}, Desired: Deleted,
		ComputeCleaned: true, WorkspaceCleaned: true, VolumeReferencesReleased: true,
	}
	if !OwnerCleanupReady(ready) {
		t.Fatal("owner cleanup incorrectly required ACC evidence or a Gov ACK")
	}
	for name, mutate := range map[string]func(*Instance){
		"running":                    func(i *Instance) { i.Desired = Running },
		"stopped":                    func(i *Instance) { i.Desired = Stopped },
		"pending write":              func(i *Instance) { i.Pending = &Mutation{Kind: "create_notebook"} },
		"compute remains":            func(i *Instance) { i.ComputeCleaned = false },
		"workspace remains":          func(i *Instance) { i.WorkspaceCleaned = false },
		"borrowed references remain": func(i *Instance) { i.VolumeReferencesReleased = false },
		"network remains":            func(i *Instance) { i.Network = &NetworkRuntime{} },
		"unknown compute type":       func(i *Instance) { i.Spec.ComputeType = "automatic" },
		"missing compute type":       func(i *Instance) { i.Spec.ComputeType = "" },
	} {
		t.Run(name, func(t *testing.T) {
			i := ready
			mutate(&i)
			if OwnerCleanupReady(i) {
				t.Fatal("incomplete owner cleanup accepted")
			}
		})
	}
	ready.Spec.ComputeType = TypeCPU
	ready.Network = &NetworkRuntime{Cleaned: true}
	if !OwnerCleanupReady(ready) {
		t.Fatal("completed CPU instance or released network rejected")
	}
}
