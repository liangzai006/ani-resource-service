package instancev1

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/google/uuid"
)

// CommandIntent contains only client intent, never resolved PVC identity.
type CommandIntent struct {
	Action, InstanceID, PVCName, MountPath string
	ExpectedVersion                        int64
	ReadOnly, RestartIfRunning             bool
}

func CanonicalCommandIntent(action, instanceID string, expectedVersion int64, pvcName, mountPath string, readOnly, restartIfRunning bool) ([]byte, error) {
	id, err := uuid.Parse(instanceID)
	if err != nil || id == uuid.Nil || id.String() != instanceID || expectedVersion < 1 {
		return nil, ErrInvalidBusiness
	}
	values := []any{instanceID, expectedVersion, action}
	switch action {
	case "start", "stop", "restart":
		if pvcName != "" || mountPath != "" || readOnly || restartIfRunning {
			return nil, ErrInvalidBusiness
		}
	case "attach-pvc":
		if pvcName == "" || mountPath == "" {
			return nil, ErrInvalidBusiness
		}
		values = append(values, pvcName, mountPath, readOnly, restartIfRunning)
	case "detach-pvc":
		if pvcName != "" || readOnly || mountPath == "" {
			return nil, ErrInvalidBusiness
		}
		values = append(values, mountPath, restartIfRunning)
	default:
		return nil, ErrInvalidBusiness
	}
	return json.Marshal(values)
}

// ParseCommandIntent decodes integer slots directly, preserving values above 2^53.
func ParseCommandIntent(action, instanceID string, canonical []byte) (CommandIntent, error) {
	var out CommandIntent
	var slots []json.RawMessage
	if json.Unmarshal(canonical, &slots) != nil || len(slots) < 3 {
		return out, ErrInvalidBusiness
	}
	if json.Unmarshal(slots[0], &out.InstanceID) != nil || json.Unmarshal(slots[1], &out.ExpectedVersion) != nil || json.Unmarshal(slots[2], &out.Action) != nil || out.InstanceID != instanceID || out.Action != action {
		return out, ErrInvalidBusiness
	}
	decode := func(raw json.RawMessage, dst any) bool {
		return !bytes.Equal(raw, []byte("null")) && json.Unmarshal(raw, dst) == nil
	}
	switch action {
	case "start", "stop", "restart":
		if len(slots) != 3 {
			return out, ErrInvalidBusiness
		}
	case "attach-pvc":
		if len(slots) != 7 || !decode(slots[3], &out.PVCName) || !decode(slots[4], &out.MountPath) || !decode(slots[5], &out.ReadOnly) || !decode(slots[6], &out.RestartIfRunning) {
			return out, ErrInvalidBusiness
		}
	case "detach-pvc":
		if len(slots) != 5 || !decode(slots[3], &out.MountPath) || !decode(slots[4], &out.RestartIfRunning) {
			return out, ErrInvalidBusiness
		}
	default:
		return out, ErrInvalidBusiness
	}
	expected, err := CanonicalCommandIntent(out.Action, out.InstanceID, out.ExpectedVersion, out.PVCName, out.MountPath, out.ReadOnly, out.RestartIfRunning)
	if err != nil || !bytes.Equal(canonical, expected) {
		return out, ErrInvalidBusiness
	}
	return out, nil
}

func CommandIntentDigest(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}
