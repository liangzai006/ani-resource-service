package vm

import (
	"context"
	"testing"

	"github.com/google/uuid"
	pb "github.com/zhangzhe-ctrl/ani-resource-service/api/vm/v1"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type logStreamSink struct {
	grpc.ServerStream
	ctx  context.Context
	sent []*pb.VMLogFrame
}

func (s *logStreamSink) Context() context.Context    { return s.ctx }
func (s *logStreamSink) Send(f *pb.VMLogFrame) error { s.sent = append(s.sent, f); return nil }

type logPortFake struct {
	frames []biz.LogFrame
	calls  int
	seen   biz.Observation
}

func (l *logPortFake) StreamLogs(_ context.Context, _ biz.VM, x biz.Observation, _ uint32, _ bool, emit func(biz.LogFrame) error) error {
	l.calls++
	l.seen = x
	for _, f := range l.frames {
		if e := emit(f); e != nil {
			return e
		}
	}
	return nil
}

type stoppedNative struct{ biz.NativePort }

func (stoppedNative) Observe(context.Context, biz.VM) (biz.Observation, error) {
	return biz.Observation{Stopped: true}, nil
}

func logServiceFixture(t *testing.T) (*Service, biz.VM, *logPortFake) {
	t.Helper()
	const cluster = "11111111-1111-4111-8111-111111111111"
	v := biz.VM{TenantID: uuid.NewString(), ID: uuid.NewString(), Version: 3, Spec: biz.Spec{Namespace: "tenant-logs", ClusterID: cluster}, Native: biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-logs", Name: "vm-logs", UID: "vm-uid"}}
	port := &logPortFake{frames: []biz.LogFrame{{Kind: "READY"}, {Kind: "DATA", Data: []byte("boot")}, {Kind: "END", Reason: "EOF"}}}
	s := &Service{repo: &snapshotServiceRepo{current: v}, native: &consoleOnlyNative{}, logs: port, config: Config{ClusterID: cluster}, logTenants: map[string]int{}, logVMs: map[string]int{}}
	return s, v, port
}

func logContext(tenant string) context.Context {
	return biz.WithCaller(context.Background(), biz.Caller{TenantID: tenant, Actor: "governance:user:42"})
}

func TestStreamVMLogsFramesTheObservedLauncher(t *testing.T) {
	s, v, port := logServiceFixture(t)
	sink := &logStreamSink{ctx: logContext(v.TenantID)}
	if e := s.StreamVMLogs(&pb.StreamVMLogsRequest{TenantId: v.TenantID, VmId: v.ID, TailLines: 100}, sink); e != nil {
		t.Fatal(e)
	}
	if port.calls != 1 || port.seen.VMI == nil || port.seen.Launcher != nil {
		t.Fatalf("observation or port wiring: %+v", port)
	}
	if len(sink.sent) != 3 || sink.sent[0].Kind != pb.VMLogFrame_READY || string(sink.sent[1].Data) != "boot" || sink.sent[2].Reason != "EOF" {
		t.Fatalf("frames: %+v", sink.sent)
	}
}

func TestStreamVMLogsRejectsInvalidAndStoppedTargets(t *testing.T) {
	s, v, port := logServiceFixture(t)
	sink := &logStreamSink{ctx: logContext(v.TenantID)}
	for _, tail := range []uint32{0, 1001} {
		if e := s.StreamVMLogs(&pb.StreamVMLogsRequest{TenantId: v.TenantID, VmId: v.ID, TailLines: tail}, sink); status.Code(e) != codes.InvalidArgument {
			t.Fatalf("tail %d accepted: %v", tail, e)
		}
	}
	if e := s.StreamVMLogs(&pb.StreamVMLogsRequest{TenantId: uuid.NewString(), VmId: v.ID, TailLines: 10}, sink); status.Code(e) != codes.PermissionDenied {
		t.Fatalf("foreign tenant accepted: %v", e)
	}
	s.native = stoppedNative{}
	e := s.StreamVMLogs(&pb.StreamVMLogsRequest{TenantId: v.TenantID, VmId: v.ID, TailLines: 10}, sink)
	if status.Code(e) != codes.FailedPrecondition || status.Convert(e).Message() != "VM_LOG_TARGET_UNAVAILABLE" || port.calls != 0 {
		t.Fatalf("stopped VM served logs: %v %+v", e, port)
	}
}

func TestStreamVMLogsConnectionLimit(t *testing.T) {
	s, v, _ := logServiceFixture(t)
	s.config.LogLimits = biz.LogLimits{InstanceConnections: 1}
	s.logVMs[v.TenantID+":"+v.ID] = 1
	e := s.StreamVMLogs(&pb.StreamVMLogsRequest{TenantId: v.TenantID, VmId: v.ID, TailLines: 10}, &logStreamSink{ctx: logContext(v.TenantID)})
	if status.Code(e) != codes.ResourceExhausted || status.Convert(e).Message() != "VM_LOG_CONNECTION_LIMIT" {
		t.Fatalf("limit missing: %v", e)
	}
}
