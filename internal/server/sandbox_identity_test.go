package server

import (
	"testing"

	sandboxpb "github.com/zhangzhe-ctrl/ani-resource-service/api/sandbox/v1"
)

func TestSandboxGovernanceAllowlistMatchesServiceDescriptor(t *testing.T) {
	for _, method := range sandboxpb.SandboxService_ServiceDesc.Methods {
		full := "/sandbox.v1.SandboxService/" + method.MethodName
		if _, ok := sandboxGovernanceMethods[full]; !ok {
			t.Fatalf("sandbox RPC %s is missing from the Governance allowlist", full)
		}
	}
}
