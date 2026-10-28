package server

import sandboxbiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"

var sandboxGovernanceMethods = map[string]struct{}{
	"/sandbox.v1.SandboxService/ResolveCreateSpec": {},
	"/sandbox.v1.SandboxService/AcceptCreate":      {},
	"/sandbox.v1.SandboxService/GetSandbox":        {},
	"/sandbox.v1.SandboxService/ListSandboxes":     {},
	"/sandbox.v1.SandboxService/GetOperation":      {},
	"/sandbox.v1.SandboxService/Pause":             {},
	"/sandbox.v1.SandboxService/Resume":            {},
	"/sandbox.v1.SandboxService/Delete":            {},
}

func sandboxCaller(tenant, actor string) sandboxbiz.Caller {
	return sandboxbiz.Caller{TenantID: tenant, Actor: actor}
}
