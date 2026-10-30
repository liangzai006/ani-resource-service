package sandboxv1

import (
	"strings"
	"testing"
)

func TestCreateIntentDigestKeepsLegacyReferenceAndBindsImageIdentity(t *testing.T) {
	old := &CreateSandboxData{Name: "demo", ImageReference: "reg.example/a@sha256:" + strings.Repeat("a", 64), PresetId: "33333333-3333-4333-8333-333333333333", WorkspaceSize: "5Gi"}
	got, err := CreateIntentDigest("", old)
	if err != nil || got != "e67cbb7b00fcdab555d2441c867715a1c1972e64a89ea4ed7ae6e15676aeb142" {
		t.Fatalf("legacy digest changed: %q %v", got, err)
	}
	selected := &CreateSandboxData{Name: old.Name, ImageId: "img_a", ImageScope: "platform", PresetId: old.PresetId, WorkspaceSize: old.WorkspaceSize}
	a, err := CreateIntentDigest("", selected)
	if err != nil {
		t.Fatal(err)
	}
	selected.ImageScope = "tenant"
	b, err := CreateIntentDigest("", selected)
	if err != nil || a == b || a == got {
		t.Fatal("image identity and scope must change the intent digest")
	}
}
