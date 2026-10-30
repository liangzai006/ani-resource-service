package sandbox

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"strings"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"
)

//go:embed filehelper/workspace.py
var sandboxWorkspaceHelper string

// ExecuteFiles must run only after the target has been admitted and fenced.
// Mutating operations whose stream fails have an unknown outcome.
func (e *KubernetesCodeExecutor) ExecuteFiles(ctx context.Context, target biz.DataPlaneTarget, request biz.FileRequest) (biz.FileResult, error) {
	if err := biz.RequireTenant(ctx, target.TenantID); err != nil {
		return biz.FileResult{}, err
	}
	if e == nil || target.Action != biz.DataPlaneFiles || target.ClusterID != e.clusterID || !biz.UUID(target.SandboxID) || target.PodName != "sandbox-"+target.SandboxID || target.Container != "sandbox" || target.Namespace == "" || target.NamespaceUID == "" || target.PodUID == "" || target.Version < 1 {
		return biz.FileResult{}, biz.ErrInvalid
	}
	if err := biz.ValidateFileRequest(request); err != nil {
		return biz.FileResult{}, err
	}
	input, err := json.Marshal(struct {
		Action        string `json:"action"`
		Path          string `json:"path"`
		ContentBase64 string `json:"content_base64,omitempty"`
		Overwrite     bool   `json:"overwrite,omitempty"`
	}{request.Action, request.Path, request.ContentBase64, request.Overwrite})
	if err != nil {
		return biz.FileResult{}, err
	}
	command := []string{"/bin/sh", "-c", podUIDGuard, "sandbox-uid-guard", target.PodUID, "python3", "-c", sandboxWorkspaceHelper}
	url := e.client.RESTClient().Post().Resource("pods").Name(target.PodName).Namespace(target.Namespace).SubResource("exec").VersionedParams(&corev1.PodExecOptions{Container: target.Container, Command: command, Stdin: true, Stdout: true, Stderr: true}, scheme.ParameterCodec).URL()
	stream, err := e.newStream(e.config, "POST", url)
	if err != nil {
		return biz.FileResult{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	stdout := &boundedOutput{limit: 4 << 20}
	stderr := &boundedOutput{limit: 8192}
	err = stream.StreamWithContext(runCtx, remotecommand.StreamOptions{Stdin: bytes.NewReader(input), Stdout: stdout, Stderr: stderr})
	out, truncated := stdout.result()
	errout, _ := stderr.result()
	if truncated {
		return biz.FileResult{}, biz.ErrDependencyUnavailable
	}
	if err != nil {
		var exit interface {
			Exited() bool
			ExitStatus() int
		}
		if errors.As(err, &exit) && exit.Exited() {
			var response struct {
				Error string `json:"error"`
			}
			_ = json.Unmarshal(errout, &response)
			switch response.Error {
			case "too_large":
				return biz.FileResult{}, biz.ErrFileTooLarge
			case "invalid":
				return biz.FileResult{}, biz.ErrInvalid
			case "conflict":
				return biz.FileResult{}, biz.ErrConflict
			case "not_found":
				return biz.FileResult{}, biz.ErrNotFound
			}
			return biz.FileResult{}, biz.ErrDependencyUnavailable
		}
		return biz.FileResult{}, err
	}
	var result biz.FileResult
	decoder := json.NewDecoder(strings.NewReader(string(out)))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&result); err != nil {
		return biz.FileResult{}, biz.ErrDependencyUnavailable
	}
	return result, nil
}
