package sandbox

import (
	"context"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// VerifyDataPlaneTarget observes the current managed namespace and Pod. The
// result is only a point-in-time check: a later exec still needs an execution
// fence against lifecycle changes and replacement under the same Pod name.
func (p *KubernetesProvider) VerifyDataPlaneTarget(ctx context.Context, s biz.Sandbox, target biz.DataPlaneTarget) error {
	if err := biz.RequireTenant(ctx, target.TenantID); err != nil {
		return err
	}
	if target.Action != biz.DataPlaneFiles && target.Action != biz.DataPlaneExec && target.Action != biz.DataPlaneCodeRun {
		return biz.ErrInvalid
	}
	if p.tenantNamespacePrefix == "" || s.TenantID != target.TenantID || s.ID != target.SandboxID || s.Version != target.Version || s.Version < 1 ||
		s.Desired != biz.DesiredRunning || s.Observed != biz.ObservedRunning || s.Pending != nil ||
		s.Snapshot.ClusterID != target.ClusterID || s.Snapshot.Namespace != target.Namespace || s.Snapshot.NamespaceUID == "" || s.Snapshot.NamespaceUID != target.NamespaceUID ||
		target.PodName != "sandbox-"+s.ID || target.PodName != s.ProviderRefs.PodName || target.PodUID == "" || target.PodUID != s.ProviderRefs.PodUID || target.Container != "sandbox" {
		return biz.ErrDataPlaneUnavailable
	}
	if err := p.requireCluster(s); err != nil {
		return err
	}
	uid, err := p.verifyTenantNamespace(ctx, s, "")
	if err != nil {
		return err
	}
	pod, err := p.client.Resource(podGVR).Namespace(target.Namespace).Get(ctx, target.PodName, metav1.GetOptions{})
	if err != nil {
		return err
	}
	if string(pod.GetUID()) != target.PodUID || !owned(pod, s) || !p.matchesPod(pod, s) || !podReady(pod) {
		return biz.ErrDataPlaneUnavailable
	}
	containers, found, err := unstructured.NestedSlice(pod.Object, "spec", "containers")
	if err != nil || !found || len(containers) != 1 {
		return biz.ErrDataPlaneUnavailable
	}
	container, ok := containers[0].(map[string]any)
	if !ok || !podUIDEnvironment(container) {
		return biz.ErrDataPlaneUnavailable
	}
	_, err = p.verifyTenantNamespace(ctx, s, uid)
	return err
}
