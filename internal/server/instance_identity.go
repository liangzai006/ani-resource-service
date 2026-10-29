package server

// Domain registration extends the existing trusted Governance listener.
var instanceGovernanceMethods = map[string]struct{}{
	"/instance.v1.InstanceService/Restart":                    {},
	"/instance.v1.InstanceService/AttachPVC":                  {},
	"/instance.v1.InstanceService/DetachPVC":                  {},
	"/instance.v1.InstanceService/ListInstanceMountablePVCs":  {},
	"/instance.v1.InstanceService/LookupCommandReplay":        {},
	"/instance.v1.InstanceService/ListMountablePVCs":          {},
	"/instance.v1.InstanceService/GetInstanceConfiguration":   {},
	"/instance.v1.InstanceService/GetInstanceRuntime":         {},
	"/instance.v1.InstanceService/ListInstanceEvents":         {},
	"/instance.v1.InstanceService/ResolveCreateSpec":          {},
	"/instance.v1.InstanceService/AcceptCreate":               {},
	"/instance.v1.InstanceService/AcceptDelete":               {},
	"/instance.v1.InstanceService/Start":                      {},
	"/instance.v1.InstanceService/Stop":                       {},
	"/instance.v1.InstanceService/GetInstance":                {},
	"/instance.v1.InstanceService/ResolveInstanceMetricScope": {},
	"/instance.v1.InstanceService/ListInstances":              {},
	"/instance.v1.InstanceService/GetOperation":               {},
	"/instance.v1.InstanceService/ResolveTerminalTarget":      {},
}
