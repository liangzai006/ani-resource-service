package sandbox

import (
	"context"
	"net/url"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

type KubernetesCodeExecutor struct {
	clusterID string
	config    *rest.Config
	client    typedcorev1.CoreV1Interface
	newStream func(*rest.Config, string, *url.URL) (remotecommand.Executor, error)
}

func NewKubernetesCodeExecutor(config *rest.Config, clusterID string) (*KubernetesCodeExecutor, error) {
	if config == nil || !biz.UUID(clusterID) {
		return nil, biz.ErrInvalid
	}
	copyConfig := rest.CopyConfig(config)
	// Streaming lifetime is controlled by the request context and code-run
	// timeout, not the short timeout used for ordinary Provider observations.
	copyConfig.Timeout = 0
	client, err := typedcorev1.NewForConfig(copyConfig)
	if err != nil {
		return nil, err
	}
	return &KubernetesCodeExecutor{clusterID: clusterID, config: copyConfig, client: client, newStream: remotecommand.NewSPDYExecutor}, nil
}

// ExecuteCodeRun is usable only after the use case has verified the live Pod
// and committed the one-time dispatch decision. A transport error leaves the
// execution outcome unknown, even if the client context was cancelled.
func (e *KubernetesCodeExecutor) ExecuteCodeRun(ctx context.Context, target biz.DataPlaneTarget, spec biz.CodeRunSpec) (biz.CodeRunResult, error) {
	if err := biz.RequireTenant(ctx, target.TenantID); err != nil {
		return biz.CodeRunResult{}, err
	}
	if e == nil || target.ClusterID != e.clusterID || !biz.UUID(target.SandboxID) || target.Action != biz.DataPlaneCodeRun || target.PodName != "sandbox-"+target.SandboxID || target.Container != "sandbox" || target.Namespace == "" || target.NamespaceUID == "" || target.Version < 1 {
		return biz.CodeRunResult{}, biz.ErrInvalid
	}
	command, err := guardedCodeRunCommand(target.PodUID, spec)
	if err != nil {
		return biz.CodeRunResult{}, err
	}
	requestURL := e.client.RESTClient().Post().Resource("pods").Name(target.PodName).Namespace(target.Namespace).SubResource("exec").VersionedParams(&corev1.PodExecOptions{
		Container: target.Container, Command: command, Stdin: true, Stdout: true, Stderr: true,
	}, scheme.ParameterCodec).URL()
	stream, err := e.newStream(e.config, "POST", requestURL)
	if err != nil {
		return biz.CodeRunResult{}, err
	}
	result, err := streamCodeRun(ctx, stream, spec)
	return biz.CodeRunResult{ExitCode: result.ExitCode, Stdout: result.Stdout, Stderr: result.Stderr, Truncated: result.Truncated}, err
}
