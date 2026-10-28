package server

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
)

func TestWorkerDiagnosticsLogProviderCauseAndResourceContext(t *testing.T) {
	o, err := NewObservability("test", "test", NewReadiness(func() bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Shutdown(context.Background())
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	work := biz.Work{Resource: biz.ResourceWork{Kind: "vlan", ID: "vlan-id"}, BindingID: "binding-id", Operation: biz.Operation{ID: "operation-id"}}
	o.ObserveWork(context.Background(), logger, work, biz.Progress{Reason: biz.ProviderUnavailable, ProviderError: &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: errors.New("client rate limiter wait: context deadline exceeded")}}, nil)
	for _, want := range []string{"client rate limiter wait: context deadline exceeded", "vlan-id", "binding-id", "operation-id"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing diagnostic %q: %s", want, output.String())
		}
	}
}

func TestVirtualSwitchDiagnosticsLogNodeCauseWithoutClaimingObservation(t *testing.T) {
	o, err := NewObservability("test-virtual-switch", "test", NewReadiness(func() bool { return true }))
	if err != nil {
		t.Fatal(err)
	}
	defer o.Shutdown(context.Background())
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	work := biz.VirtualSwitchWork{Switch: biz.VirtualSwitch{EgressMetadata: biz.EgressMetadata{ID: "switch-id", LastOperationID: "operation-id"}, BindingID: "binding-id", BindingState: "configured"}}
	progress := biz.VirtualSwitchProgress{BindingState: "configured", Reason: biz.ProviderUnavailable, Action: "inspect", NodeName: "node-a", NodeUID: "node-uid", ProviderError: &biz.ProviderError{Kind: biz.ProviderTemporary, Cause: errors.New("node facts collection expired")}}
	o.ObserveVirtualSwitchWork(context.Background(), logger, work, progress, nil)
	for _, want := range []string{"node facts collection expired", "switch-id", "binding-id", "operation-id", "node-a", "node-uid", `"action":"inspect"`, `"observed":false`, `"result":"provider_error"`} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("missing diagnostic %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), `"result":"observed"`) {
		t.Fatalf("temporary failure reported successful observation: %s", output.String())
	}
}
