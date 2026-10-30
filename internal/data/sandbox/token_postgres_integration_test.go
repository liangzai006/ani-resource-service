//go:build sandboxintegration

package sandbox_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
	data "github.com/zhangzhe-ctrl/ani-resource-service/internal/data/sandbox"
)

func TestSandboxTokenPersistenceReplayScopeAndRevocation(t *testing.T) {
	f, ctx, base := codeRunFixture(t)
	signer, err := data.NewTokenSigner([]byte(`{"current":"v1","keys":{"v1":"QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE="}}`))
	if err != nil {
		t.Fatal(err)
	}
	service, err := biz.NewTokenService(f.repo, signer)
	if err != nil {
		t.Fatal(err)
	}
	c := biz.TokenCommand{TenantID: base.TenantID, SandboxID: base.SandboxID, TokenID: uuid.NewString(), IdempotencyKey: "token-key", GovTenantID: 7, IssuerID: 42, Scopes: []string{"files"}, ExpiresIn: time.Minute}
	const n = 8
	type response struct {
		record biz.TokenRecord
		token  string
		err    error
	}
	out := make(chan response, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			one := c
			one.TokenID = uuid.NewString()
			record, value, err := service.Issue(ctx, one)
			out <- response{record, value, err}
		}()
	}
	wg.Wait()
	close(out)
	value, id := "", ""
	created := 0
	for v := range out {
		if v.err != nil {
			t.Fatal(v.err)
		}
		if value == "" {
			value = v.token
			id = v.record.Claims.TokenID
		}
		if value != v.token || id != v.record.Claims.TokenID {
			t.Fatal("idempotent replay changed token")
		}
		if !v.record.Replayed {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("created %d", created)
	}
	tokenCtx := biz.WithCaller(context.Background(), biz.Caller{TenantID: c.TenantID, Actor: "governance:sandbox-token:" + id})
	if _, err = service.Verify(tokenCtx, c.TenantID, c.SandboxID, value, "files"); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Verify(tokenCtx, c.TenantID, c.SandboxID, value, "exec"); !errors.Is(err, biz.ErrDenied) {
		t.Fatalf("scope accepted: %v", err)
	}
	if _, err = service.Verify(tokenCtx, c.TenantID, uuid.NewString(), value, "files"); !errors.Is(err, biz.ErrTokenInvalid) {
		t.Fatalf("other sandbox accepted: %v", err)
	}
	other := uuid.NewString()
	otherCtx := biz.WithCaller(context.Background(), biz.Caller{TenantID: other, Actor: "governance:sandbox-token:" + id})
	if _, err = service.Verify(otherCtx, other, c.SandboxID, value, "files"); !errors.Is(err, biz.ErrTokenInvalid) {
		t.Fatalf("other tenant accepted: %v", err)
	}
	if err = service.Revoke(ctx, c.TenantID, c.SandboxID, id); err != nil {
		t.Fatal(err)
	}
	if _, err = service.Verify(tokenCtx, c.TenantID, c.SandboxID, value, "files"); !errors.Is(err, biz.ErrTokenInvalid) {
		t.Fatalf("revoked accepted: %v", err)
	}
	c.TokenID = id
	if _, _, err = service.Issue(ctx, c); !errors.Is(err, biz.ErrConflict) {
		t.Fatalf("revoked replay: %v", err)
	}
}
func TestSandboxTokenInvalidatedByPause(t *testing.T) {
	f, ctx, base := codeRunFixture(t)
	signer, err := data.NewTokenSigner([]byte(`{"current":"v1","keys":{"v1":"QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE="}}`))
	if err != nil {
		t.Fatal(err)
	}
	service, _ := biz.NewTokenService(f.repo, signer)
	c := biz.TokenCommand{TenantID: base.TenantID, SandboxID: base.SandboxID, TokenID: uuid.NewString(), IdempotencyKey: "token-key", GovTenantID: 7, IssuerID: 42, Scopes: []string{"files"}, ExpiresIn: time.Minute}
	_, value, err := service.Issue(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	pause := biz.Command{TenantID: c.TenantID, ID: c.SandboxID, OperationID: uuid.NewString(), Action: "pause", IdempotencyKey: "pause", RequestDigest: biz.ActionDigest("", 1, "pause"), ExpectedVersion: 1}
	if _, err = f.repo.Change(ctx, pause); err != nil {
		t.Fatal(err)
	}
	tokenCtx := biz.WithCaller(context.Background(), biz.Caller{TenantID: c.TenantID, Actor: "governance:sandbox-token:" + c.TokenID})
	if _, err = service.Verify(tokenCtx, c.TenantID, c.SandboxID, value, "files"); !errors.Is(err, biz.ErrTokenInvalid) {
		t.Fatalf("paused token accepted: %v", err)
	}
	// Model a completed resume while retaining the newer lifecycle version.
	if _, err = f.runtime.Exec(ctx, `UPDATE sandbox.sandboxes SET desired='running',observed='running',pending_mutation=NULL WHERE tenant_id=$1 AND sandbox_id=$2`, c.TenantID, c.SandboxID); err != nil {
		t.Fatal(err)
	}
	current, err := f.repo.Get(ctx, c.TenantID, c.SandboxID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = biz.DataPlaneTargetForSandbox(current, biz.DataPlaneFiles); err != nil {
		t.Fatalf("resume fixture not ready: %v", err)
	}
	if _, err = service.Verify(tokenCtx, c.TenantID, c.SandboxID, value, "files"); !errors.Is(err, biz.ErrTokenInvalid) {
		t.Fatalf("old generation accepted after resume: %v", err)
	}
}
