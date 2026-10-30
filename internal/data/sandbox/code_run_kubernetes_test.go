package sandbox

import (
	"context"
	"io"
	"net/url"
	"strings"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func TestKubernetesCodeExecutorKeepsUserCodeOutOfURL(t *testing.T) {
	cluster := "44444444-4444-4444-8444-444444444444"
	tenant := "11111111-1111-4111-8111-111111111111"
	id := "22222222-2222-4222-8222-222222222222"
	executor, err := NewKubernetesCodeExecutor(&rest.Config{Host: "https://kubernetes.invalid", Timeout: time.Second}, cluster)
	if err != nil {
		t.Fatal(err)
	}
	code := "print('private source')"
	executor.newStream = func(config *rest.Config, method string, endpoint *url.URL) (remotecommand.Executor, error) {
		if config.Timeout != 0 || method != "POST" || strings.Contains(endpoint.String(), code) || !strings.Contains(endpoint.Path, "/namespaces/tenant-test/pods/sandbox-"+id+"/exec") {
			t.Fatalf("bad exec request: %s %s", method, endpoint.Redacted())
		}
		return fakeCodeRunStream(func(_ context.Context, o remotecommand.StreamOptions) error {
			input, err := io.ReadAll(o.Stdin)
			if err != nil || string(input) != "23\n"+code+"hello" {
				t.Fatalf("stream input=%q err=%v", input, err)
			}
			_, _ = io.WriteString(o.Stdout, "ok")
			return nil
		}), nil
	}
	ctx := biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "user"})
	target := biz.DataPlaneTarget{TenantID: tenant, SandboxID: id, Action: biz.DataPlaneCodeRun, Version: 1, ClusterID: cluster, Namespace: "tenant-test", NamespaceUID: "ns-uid", PodName: "sandbox-" + id, PodUID: "pod-uid", Container: "sandbox"}
	result, err := executor.ExecuteCodeRun(ctx, target, biz.CodeRunSpec{Language: "python", Code: code, Stdin: "hello", Timeout: time.Second})
	if err != nil || result.ExitCode == nil || *result.ExitCode != 0 || string(result.Stdout) != "ok" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
