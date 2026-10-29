package instance

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/instance"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func configuredInstance(t *testing.T) biz.Instance {
	t.Helper()
	_, i, _, _ := diagnosticFixture(t, false)
	i.Spec.ContractVersion = 2
	i.Spec.WorkspaceMountPath = "/home/work"
	i.Spec.VolumeMounts = []biz.VolumeMount{
		{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "dataset", PVCUID: "dataset-original", MountPath: "/datasets", Ownership: "borrowed"},
		{ClusterID: i.Spec.ClusterID, Namespace: i.Spec.Namespace, PVCName: "models", PVCUID: "models-original", MountPath: "/models", ReadOnly: true, Ownership: "borrowed"},
	}
	i.CurrentMounts = append([]biz.VolumeMount{}, i.Spec.VolumeMounts...)
	i.Spec.Labels = map[string]string{"team.example/app": "research"}
	i.Spec.Annotations = map[string]string{"team.example/note": "literal note"}
	i.Spec.Env = []biz.EnvVar{{Name: "BASE", Value: "  literal  "}, {Name: "DEPENDENT", Value: "$(BASE)/child"}, {Name: "EMPTY", Value: ""}, {Name: "DOLLARS", Value: "$$"}}
	return i
}

func projectedPod(t *testing.T, i biz.Instance) *unstructured.Unstructured {
	t.Helper()
	notebook, err := RenderNotebook(i)
	if err != nil {
		t.Fatal(err)
	}
	template, _, _ := unstructured.NestedMap(notebook.Object, "spec", "template")
	pod := &unstructured.Unstructured{Object: template}
	pod.SetLabels(notebook.GetLabels())
	pod.SetAnnotations(notebook.GetAnnotations())
	pod.SetNamespace(i.Spec.Namespace)
	return pod
}

func TestV2NotebookFrozenConfigurationProjection(t *testing.T) {
	i := configuredInstance(t)
	notebook, err := RenderNotebook(i)
	if err != nil {
		t.Fatal(err)
	}
	if notebook.GetLabels()["team.example/app"] != "research" || notebook.GetLabels()[tenantLabel] != i.TenantID || notebook.GetAnnotations()["team.example/note"] != "literal note" {
		t.Fatal("user metadata and managed identity must coexist on Notebook metadata")
	}
	if _, exists, _ := unstructured.NestedMap(notebook.Object, "spec", "template", "metadata"); exists {
		t.Fatal("Notebook v1.10 prunes PodTemplate metadata")
	}
	pod := projectedPod(t, i)
	containers, _, _ := unstructured.NestedSlice(pod.Object, "spec", "containers")
	c := containers[0].(map[string]any)
	if c["workingDir"] != "/home/work" {
		t.Fatal("frozen workspace path not projected")
	}
	env, _, _ := unstructured.NestedSlice(c, "env")
	if len(env) != len(i.Spec.Env)+1 {
		t.Fatal("literal env or managed UID guard omitted")
	}
	// EnvVar.value uses $$ for a literal $ and does not expand escaped references:
	// https://kubernetes.io/docs/reference/kubernetes-api/core/pod-v1/
	expectedPodValues := []string{"  literal  ", "$$(BASE)/child", "", "$$$$"}
	for k, expected := range i.Spec.Env {
		actual := env[k].(map[string]any)
		if actual["name"] != expected.Name || actual["value"] != expectedPodValues[k] || actual["valueFrom"] != nil {
			t.Fatal("literal env order or Kubelet dollar escaping changed")
		}
	}
	mounts, _, _ := unstructured.NestedSlice(c, "volumeMounts")
	if len(mounts) != 3 || mounts[0].(map[string]any)["mountPath"] != "/home/work" || mounts[2].(map[string]any)["readOnly"] != true {
		t.Fatal("workspace, borrowed mounts, or readOnly not projected")
	}
	if err := verifyPod(pod, i); err != nil {
		t.Fatal("valid frozen Pod rejected", err)
	}
	mutations := map[string]func(*unstructured.Unstructured){
		"lost label": func(p *unstructured.Unstructured) { p.SetLabels(map[string]string{}) },
		"changed annotation": func(p *unstructured.Unstructured) {
			p.SetAnnotations(map[string]string{"team.example/note": "changed"})
		},
		"changed env value": func(p *unstructured.Unstructured) {
			cs, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
			env, _, _ := unstructured.NestedSlice(cs[0].(map[string]any), "env")
			env[0].(map[string]any)["value"] = "changed"
			cs[0].(map[string]any)["env"] = env
			_ = unstructured.SetNestedSlice(p.Object, cs, "spec", "containers")
		},
		"changed borrowed claim": func(p *unstructured.Unstructured) {
			vs, _, _ := unstructured.NestedSlice(p.Object, "spec", "volumes")
			vs[1].(map[string]any)["persistentVolumeClaim"] = map[string]any{"claimName": "replacement"}
			_ = unstructured.SetNestedSlice(p.Object, vs, "spec", "volumes")
		},
		"removed readOnly": func(p *unstructured.Unstructured) {
			cs, _, _ := unstructured.NestedSlice(p.Object, "spec", "containers")
			ms, _, _ := unstructured.NestedSlice(cs[0].(map[string]any), "volumeMounts")
			delete(ms[2].(map[string]any), "readOnly")
			cs[0].(map[string]any)["volumeMounts"] = ms
			_ = unstructured.SetNestedSlice(p.Object, cs, "spec", "containers")
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			changed := pod.DeepCopy()
			mutate(changed)
			if err := verifyPod(changed, i); !errors.Is(err, biz.ErrOwnership) {
				t.Fatal("changed effective configuration accepted", err)
			}
		})
	}
}

func TestV2NotebookDoesNotMutateFrozenConfiguration(t *testing.T) {
	i := configuredInstance(t)
	before := i.Spec
	before.Env = append([]biz.EnvVar(nil), i.Spec.Env...)
	labelsBefore := map[string]string{"team.example/app": "research"}
	annotationsBefore := map[string]string{"team.example/note": "literal note"}
	if _, err := RenderNotebook(i); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(i.Spec.Labels, labelsBefore) || !reflect.DeepEqual(i.Spec.Annotations, annotationsBefore) || !reflect.DeepEqual(i.Spec.Env, before.Env) {
		t.Fatal("render changed frozen business configuration")
	}
}

func TestStartRejectsReplacedBorrowedPVC(t *testing.T) {
	n, original, client, _ := diagnosticFixture(t, false)
	i := configuredInstance(t)
	i.NotebookUID = original.NotebookUID
	ns := tenantNamespace(i.TenantID, i.Spec.Namespace)
	_, _ = client.Resource(namespaceGVR).Create(context.Background(), ns, metav1.CreateOptions{})
	for _, mount := range i.Spec.VolumeMounts {
		pvc := testBorrowedPVC(i.Spec.Namespace, mount.PVCName, mount.PVCUID, "Pending", "Filesystem")
		if mount.PVCName == "dataset" {
			pvc.SetUID("replacement")
		}
		_, _ = client.Resource(pvcGVR).Namespace(i.Spec.Namespace).Create(context.Background(), pvc, metav1.CreateOptions{})
	}
	client.ClearActions()
	if err := n.SetStopped(context.Background(), i, biz.Observation{NotebookUID: i.NotebookUID}, false); !errors.Is(err, biz.ErrPVCReferenceChanged) {
		t.Fatal("Start accepted same-name replacement PVC", err)
	}
	for _, action := range client.Actions() {
		if action.GetVerb() == "update" || action.GetVerb() == "delete" || action.GetVerb() == "create" {
			t.Fatal("failed reference verification mutated Kubernetes")
		}
	}
}

func TestV2NotebookRejectsReservedConfigurationWithoutLeakingValues(t *testing.T) {
	for name, mutate := range map[string]func(*biz.Spec){
		"managed label":       func(s *biz.Spec) { s.Labels["ani.io/instance-id"] = "hidden-value" },
		"managed subdomain":   func(s *biz.Spec) { s.Annotations["custom.ani.io/mode"] = "hidden-value" },
		"network selector":    func(s *biz.Spec) { s.Annotations["k8s.v1.cni.cncf.io/networks"] = "hidden-value" },
		"controller selector": func(s *biz.Spec) { s.Labels["notebook-name"] = "hidden-value" },
		"runtime env": func(s *biz.Spec) {
			s.Env = append(s.Env, biz.EnvVar{Name: "ANI_INSTANCE_POD_UID", Value: "hidden-value"})
		},
		"protected path": func(s *biz.Spec) { s.VolumeMounts[0].MountPath = "/var/run" },
	} {
		t.Run(name, func(t *testing.T) {
			i := configuredInstance(t)
			mutate(&i.Spec)
			if _, err := RenderNotebook(i); err == nil {
				t.Fatal("reserved configuration accepted")
			} else if strings.Contains(err.Error(), "hidden-value") {
				t.Fatal("configuration validation exposed user value")
			}
		})
	}
}
