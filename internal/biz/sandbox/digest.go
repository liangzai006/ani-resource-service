package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

// ActionDigest uses an empty historicalInstance for standalone Sandbox actions.
// A non-empty value is used only to compare already-persisted pre-upgrade actions.
func ActionDigest(historicalInstance string, version int64, action string) string {
	raw, _ := json.Marshal(struct {
		InstanceID string `json:"instance_id"`
		Version    int64  `json:"expected_version"`
		Action     string `json:"action"`
	}{historicalInstance, version, action})
	sum := sha256.Sum256(append([]byte("sandbox-action-v1\n"), raw...))
	return hex.EncodeToString(sum[:])
}
