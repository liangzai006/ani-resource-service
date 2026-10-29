package sandboxv1

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
)

// CreateIntentDigest freezes the public request before resolving catalog or
// deployment defaults. Empty workspace size remains distinct from an explicit size.
// New requests use an empty instanceID; non-empty values retain the historical
// digest format solely for replay of already accepted legacy commands.
func CreateIntentDigest(instanceID string, data *CreateSandboxData) (string, error) {
	if data == nil {
		return "", errors.New("INVALID_SANDBOX_REQUEST")
	}
	value := struct {
		InstanceID     string `json:"instance_id"`
		Name           string `json:"name"`
		ImageReference string `json:"image_reference"`
		PresetID       string `json:"preset_id"`
		WorkspaceSize  string `json:"workspace_size"`
	}{instanceID, strings.TrimSpace(data.Name), strings.TrimSpace(data.ImageReference), data.PresetId, data.WorkspaceSize}
	return digest("sandbox-intent-v1\n", value)
}

// CommandDigest binds the accepted frozen specification and, for new clients,
// the original public intent. Omitting intent preserves legacy command digests.
// New requests use an empty instanceID; legacy non-empty values are retained
// solely to verify a replay of an already accepted historical command.
func CommandDigest(c *CommandContext, instanceID string, spec *ResolveCreateSpecResponse, intentDigest string) (string, error) {
	if c == nil || spec == nil {
		return "", errors.New("INVALID_SANDBOX_REQUEST")
	}
	value := struct {
		TenantID     string                     `json:"tenant_id"`
		ResourceID   string                     `json:"resource_id"`
		OperationID  string                     `json:"operation_id"`
		InstanceID   string                     `json:"instance_id"`
		Spec         *ResolveCreateSpecResponse `json:"spec"`
		IntentDigest string                     `json:"create_intent_digest,omitempty"`
	}{c.TenantId, c.ResourceId, c.OperationId, instanceID, spec, intentDigest}
	return digest("sandbox-create-v1\n", value)
}

// MatchLegacyCreateIntent uses only the accepted snapshot. Historical rows did
// not record whether workspace size was defaulted, so an omitted size matches
// their original frozen size without consulting today's configuration.
func MatchLegacyCreateIntent(data *CreateSandboxData, snapshot *SandboxSnapshot) bool {
	return data != nil && snapshot != nil && snapshot.Workspace != nil &&
		strings.TrimSpace(data.Name) == snapshot.Name && strings.TrimSpace(data.ImageReference) == snapshot.ImageReference &&
		data.PresetId == snapshot.PresetId && (data.WorkspaceSize == "" || data.WorkspaceSize == snapshot.Workspace.Size)
}
func digest(prefix string, value any) (string, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte(prefix), raw...))
	return hex.EncodeToString(sum[:]), nil
}
