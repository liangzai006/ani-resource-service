//go:build instanceintegration

package vm

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/vm"
	"github.com/zhangzhe-ctrl/ani-resource-service/tests/testenv"
)

type powerVMNative struct {
	biz.NativePort
	ref              biz.ObjectRef
	vmi              string
	starts, restarts int
}

func (n *powerVMNative) ValidateSource(context.Context, biz.Source) error        { return nil }
func (n *powerVMNative) EnsureVM(context.Context, biz.VM) (biz.ObjectRef, error) { return n.ref, nil }
func (n *powerVMNative) Observe(context.Context, biz.VM) (biz.Observation, error) {
	x := biz.Observation{VM: n.ref, Stopped: n.vmi == "", Running: n.vmi != "", State: "Stopped"}
	if n.vmi != "" {
		x.State = "Running"
		x.VMI = &biz.ObjectRef{UID: n.vmi}
	}
	return x, nil
}
func (n *powerVMNative) Start(context.Context, biz.VM) error {
	n.starts++
	if n.starts == 1 {
		return biz.ErrUnknown
	}
	n.vmi = uuid.NewString()
	return nil
}
func (n *powerVMNative) Restart(context.Context, biz.VM) error { n.restarts++; return biz.ErrUnknown }

type powerVMNetwork struct{ preparingVMNetwork }

func (n powerVMNetwork) Observe(context.Context, biz.VM, biz.Observation) ([]biz.Address, error) {
	return nil, nil
}

func TestVMPowerUnknownRecoveryDoesNotRepeatRestart(t *testing.T) {
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	if e := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); e != nil {
		t.Fatal(e)
	}
	p, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	tenant, id, op := uuid.NewString(), uuid.NewString(), uuid.NewString()
	_, e = p.AcceptCreate(ctx, biz.Acceptance{TenantID: tenant, VMID: id, OperationID: op, RequestDigest: stringDigest('a'), Spec: biz.Spec{Canonical: []byte(`{"power":"native-boundary"}`), Digest: stringDigest('b'), Name: "power", StartAfterCreate: true}})
	if e != nil {
		t.Fatal(e)
	}
	native := &powerVMNative{ref: biz.ObjectRef{Kind: "VirtualMachine", UID: uuid.NewString(), Name: "vm-power"}}
	w, e := biz.NewWorker(p, native, powerVMNetwork{})
	if e != nil {
		t.Fatal(e)
	}
	step := func() {
		t.Helper()
		until := time.Now().Add(3 * time.Second)
		for time.Now().Before(until) {
			worked, err := w.Step(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if worked {
				return
			}
			time.Sleep(30 * time.Millisecond)
		}
		t.Fatal("durable operation never became due")
	}
	step()
	step()
	step() // network, VM, first start loses its result
	if native.starts != 1 {
		t.Fatalf("first start count %d", native.starts)
	}
	// Reopen PostgreSQL as a new worker, retaining the original operation/key.
	reopened, e := OpenPostgres(ctx, db.RuntimeDSN)
	if e != nil {
		t.Fatal(e)
	}
	defer reopened.Close()
	w, e = biz.NewWorker(reopened, native, powerVMNetwork{})
	if e != nil {
		t.Fatal(e)
	}
	step()
	step()
	saved, e := reopened.GetOperation(ctx, tenant, id, op)
	if e != nil || saved.State != "succeeded" || native.starts != 2 {
		t.Fatalf("observed-stopped Start did not recover: %+v starts=%d err=%v", saved, native.starts, e)
	}
	restart, e := reopened.AcceptCommand(ctx, biz.Command{TenantID: tenant, VMID: id, Action: "restart", Key: "restart-one", RequestDigest: stringDigest('c'), Intent: []byte(`{"expected_version":1}`), ExpectedVersion: 1})
	if e != nil {
		t.Fatal(e)
	}
	step()
	step()
	pending, e := reopened.GetOperation(ctx, tenant, id, restart.ID)
	if e != nil || pending.State != "unknown" || native.restarts != 1 {
		t.Fatalf("unknown Restart was resent: %+v count=%d err=%v", pending, native.restarts, e)
	}
	native.vmi = uuid.NewString()
	step()
	saved, e = reopened.GetOperation(ctx, tenant, id, restart.ID)
	if e != nil || saved.State != "succeeded" || native.restarts != 1 {
		t.Fatalf("new VMI not observed for original Restart: %+v count=%d err=%v", saved, native.restarts, e)
	}
}

type failedStartNative struct {
	biz.NativePort
	ref         biz.ObjectRef
	observation biz.Observation
	observeErr  error
	starts      int
}

func (n *failedStartNative) ValidateSource(context.Context, biz.Source) error { return nil }
func (n *failedStartNative) EnsureVM(context.Context, biz.VM) (biz.ObjectRef, error) {
	return n.ref, nil
}
func (n *failedStartNative) Observe(context.Context, biz.VM) (biz.Observation, error) {
	return n.observation, n.observeErr
}
func (n *failedStartNative) Start(context.Context, biz.VM) error {
	n.starts++
	return biz.ErrUnknown
}

func TestVMStartTerminalFailureReleasesOriginalOperationWithoutResending(t *testing.T) {
	ctx := context.Background()
	db := testenv.NewDatabase(t)
	if err := Migrate(ctx, db.Owner.Config().ConnString(), db.RuntimeRole); err != nil {
		t.Fatal(err)
	}
	p, err := OpenPostgres(ctx, db.RuntimeDSN)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	for _, mode := range []string{"failed-current", "old-failed-new-failed", "old-failed-vmi-unchanged", "legacy-missing-before-vmi", "foreign-pending-target", "pending-native-request", "still-preparing", "observation-unknown"} {
		if !t.Run(mode, func(t *testing.T) {
			terminalFailure := mode == "failed-current" || mode == "old-failed-new-failed"
			tenant, id, createID := uuid.NewString(), uuid.NewString(), uuid.NewString()
			_, err := p.AcceptCreate(ctx, biz.Acceptance{TenantID: tenant, VMID: id, OperationID: createID, RequestDigest: stringDigest('a'), Spec: biz.Spec{Canonical: []byte(`{"power":"failed-start-boundary"}`), Digest: stringDigest('b'), Name: "failed-start"}})
			if err != nil {
				t.Fatal(err)
			}
			native := &failedStartNative{ref: biz.ObjectRef{Kind: "VirtualMachine", Namespace: "tenant-start", Name: "vm-" + id, UID: uuid.NewString()}}
			native.observation = biz.Observation{VM: native.ref, Stopped: true, State: "Stopped"}
			worker, err := biz.NewWorker(p, native, powerVMNetwork{})
			if err != nil {
				t.Fatal(err)
			}
			step := func() {
				t.Helper()
				deadline := time.Now().Add(3 * time.Second)
				for time.Now().Before(deadline) {
					worked, err := worker.Step(ctx)
					if err != nil {
						t.Fatal(err)
					}
					if worked {
						return
					}
					time.Sleep(25 * time.Millisecond)
				}
				t.Fatal("original operation did not become due")
			}
			step()
			step()
			step() // Actual create admission, network, VM identity, stopped completion.
			original, err := p.AcceptCommand(ctx, biz.Command{TenantID: tenant, VMID: id, Action: "start", Key: "one-original-start", RequestDigest: stringDigest('c'), Intent: []byte(`{"expected_version":1}`), ExpectedVersion: 1})
			if err != nil {
				t.Fatal(err)
			}
			if mode == "old-failed-vmi-unchanged" || mode == "old-failed-new-failed" {
				native.observation = biz.Observation{VM: native.ref, VMI: &biz.ObjectRef{UID: "prior-failed-vmi"}, State: "Failed"}
			}
			step() // One native submission; its response is genuinely unknown.
			pending, err := p.GetOperation(ctx, tenant, id, original.ID)
			if err != nil || pending.State != "unknown" || native.starts != 1 {
				t.Fatalf("original Start was not retained after its unknown response: %+v starts=%d err=%v", pending, native.starts, err)
			}
			beforeVMIUID := ""
			if mode == "old-failed-vmi-unchanged" || mode == "old-failed-new-failed" {
				beforeVMIUID = "prior-failed-vmi"
			}
			if pending.Pending == nil || pending.Pending.BeforeVMIUID == nil || *pending.Pending.BeforeVMIUID != beforeVMIUID {
				t.Fatalf("Start submission lost its observed VMI precondition: %+v", pending.Pending)
			}
			if mode == "legacy-missing-before-vmi" || mode == "foreign-pending-target" {
				// Resume a legacy or inconsistent persisted checkpoint through the real repository.
				deadline := time.Now().Add(3 * time.Second)
				for {
					claimed, vm, claimErr := p.Claim(ctx, uuid.NewString(), 30*time.Second)
					if errors.Is(claimErr, biz.ErrNotFound) && time.Now().Before(deadline) {
						time.Sleep(25 * time.Millisecond)
						continue
					}
					if claimErr != nil || claimed.ID != original.ID || claimed.Pending == nil {
						t.Fatalf("original checkpoint could not be resumed: %+v err=%v", claimed, claimErr)
					}
					if mode == "legacy-missing-before-vmi" {
						claimed.Pending.BeforeVMIUID = nil
					} else {
						claimed.Pending.Target.UID = "another-native-vm"
					}
					claimed.State = "unknown"
					if err := p.Save(ctx, claimed, vm); err != nil {
						t.Fatal(err)
					}
					break
				}
			}
			reopened, err := OpenPostgres(ctx, db.RuntimeDSN)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			worker, err = biz.NewWorker(reopened, native, powerVMNetwork{})
			if err != nil {
				t.Fatal(err)
			}
			x := biz.Observation{VM: native.ref, VMI: &biz.ObjectRef{UID: "this-start-vmi"}, State: "Failed"}
			switch mode {
			case "old-failed-vmi-unchanged":
				x = native.observation
			case "pending-native-request":
				x.StateChangeRequests = []string{`{"action":"Start"}`}
			case "still-preparing":
				x.State = "Scheduled"
			case "observation-unknown":
				native.observeErr = biz.ErrUnknown
			}
			native.observation = x
			step()
			saved, err := reopened.GetOperation(ctx, tenant, id, original.ID)
			if err != nil || saved.ID != original.ID || saved.IntentVersion != 2 || native.starts != 1 {
				t.Fatalf("recovery changed the original Start or repeated it: %+v starts=%d err=%v", saved, native.starts, err)
			}
			if terminalFailure {
				if saved.State != "failed" || saved.Reason != "VM_NATIVE_START_FAILED" || saved.Pending != nil {
					t.Fatalf("known terminal Start failure did not converge: %+v", saved)
				}
				vm, err := reopened.Get(ctx, tenant, id)
				if err != nil || vm.Version != 2 || vm.Observed != "Failed" || vm.Reason != saved.Reason {
					t.Fatalf("terminal Start failure was not observable on the VM: %+v err=%v", vm, err)
				}
			} else if saved.State != "unknown" || saved.Reason != biz.ErrUnknown.Error() || saved.Pending == nil {
				t.Fatalf("insufficient or stale native evidence was made terminal: %+v", saved)
			}
			_, err = reopened.AcceptCommand(ctx, biz.Command{TenantID: tenant, VMID: id, Action: "stop", Key: "stop-after-start", RequestDigest: stringDigest('d'), Intent: []byte(`{"expected_version":2}`), ExpectedVersion: 2})
			if terminalFailure {
				if err != nil {
					t.Fatalf("known terminal failure kept the VM busy: %v", err)
				}
				native.observation = biz.Observation{VM: native.ref, State: "Stopped", Stopped: true}
				step() // Finish the accepted Stop through normal reconciliation.
			} else if !errors.Is(err, biz.ErrBusy) {
				t.Fatalf("unresolved original Start lost its busy fence: %v", err)
			}
			if !terminalFailure {
				native.observeErr = nil
				native.observation = biz.Observation{VM: native.ref, VMI: &biz.ObjectRef{UID: "replacement-vmi"}, State: "Running", Running: true}
				step()
				saved, err = reopened.GetOperation(ctx, tenant, id, original.ID)
				if err != nil || saved.State != "succeeded" || native.starts != 1 {
					t.Fatalf("old failed VMI prevented the original Start from observing its replacement: %+v starts=%d err=%v", saved, native.starts, err)
				}
			}
		}) {
			return
		}
	}
}
