package instance

import (
	"encoding/json"
	"fmt"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	"testing"
)

func TestCurrentConfigurationPreservesCleanupCompletion(t *testing.T) {
	_, current, _, _ := diagnosticFixture(t, false)
	for _, completedAt := range []string{"0001-01-01T00:00:00Z", "2026-10-27T07:00:00Z"} {
		t.Run(completedAt, func(t *testing.T) {
			raw, err := json.Marshal(current)
			if err != nil {
				t.Fatal(err)
			}
			var snapshot map[string]json.RawMessage
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			snapshot["CleanupCompletedAt"], _ = json.Marshal(completedAt)
			raw, err = json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			var restored biz.Instance
			if err := decodeSnapshot(raw, &restored); err != nil {
				t.Fatalf("decode current shared-worker snapshot: %v", err)
			}
			raw, err = json.Marshal(restored)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(raw, &snapshot); err != nil {
				t.Fatal(err)
			}
			var got string
			if err := json.Unmarshal(snapshot["CleanupCompletedAt"], &got); err != nil || got != completedAt {
				t.Fatalf("cleanup completion lost: got %q, want %q; error %v", got, completedAt, err)
			}
			snapshot["UnrecognizedRuntimeField"] = json.RawMessage(`true`)
			raw, err = json.Marshal(snapshot)
			if err != nil {
				t.Fatal(err)
			}
			if err := decodeSnapshot(raw, &restored); err == nil {
				t.Fatal("unrecognized runtime field accepted")
			}
		})
	}
}

func TestCurrentConfigurationRejectsRetiredSnapshotsAndNotebook(t *testing.T) {
	_, current, _, _ := diagnosticFixture(t, false)
	for _, version := range []uint32{0, 1} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			instance := current
			instance.Spec.ContractVersion = version
			instance.Spec.WorkspaceMountPath = ""
			if _, err := RenderNotebook(instance); err == nil {
				t.Error("retired configuration rendered a Notebook")
			}
			for _, hash := range []string{instance.CreateRequestHash, ""} {
				instance.CreateRequestHash = hash
				raw, err := json.Marshal(instance)
				if err != nil {
					t.Fatal(err)
				}
				var restored biz.Instance
				if err := decodeSnapshot(raw, &restored); err == nil {
					t.Error("retired configuration decoded from persisted snapshot")
				}
			}
		})
	}
}
