package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"
)

type tokenExpirySigner struct{ claims TokenClaims }

func (s tokenExpirySigner) CurrentKeyID() string                          { return "v1" }
func (s tokenExpirySigner) Sign(TokenClaims) (string, error)              { return "", nil }
func (s tokenExpirySigner) Verify(string, time.Time) (TokenClaims, error) { return s.claims, nil }

type tokenExpiryRepo struct {
	record TokenRecord
	after  func()
}

func (r tokenExpiryRepo) AcceptToken(context.Context, TokenCommand, string, time.Time) (TokenRecord, error) {
	r.after()
	return r.record, nil
}
func (r tokenExpiryRepo) GetToken(context.Context, string, string, string) (TokenRecord, error) {
	r.after()
	return r.record, nil
}
func (r tokenExpiryRepo) RevokeToken(context.Context, string, string, string) error { return nil }
func TestSandboxTokenExpiryRecheckedAfterRepositoryWait(t *testing.T) {
	now := time.Unix(1700000000, 0)
	claims := TokenClaims{TenantID: "11111111-1111-4111-8111-111111111111", SandboxID: "22222222-2222-4222-8222-222222222222", TokenID: "33333333-3333-4333-8333-333333333333", Scopes: []string{"files"}, ExpiresAt: now.Add(time.Second).Unix()}
	repo := tokenExpiryRepo{record: TokenRecord{Claims: claims}, after: func() { now = now.Add(time.Second) }}
	service := &TokenService{repo: repo, signer: tokenExpirySigner{claims: claims}, now: func() time.Time { return now }}
	ctx := WithCaller(context.Background(), Caller{TenantID: claims.TenantID, Actor: "governance:sandbox-token:" + claims.TokenID})
	if _, err := service.Verify(ctx, claims.TenantID, claims.SandboxID, "value", "files"); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("expired during lookup: %v", err)
	}
}

func TestSandboxTokenIssueRejectsExpiredAfterRepositoryWait(t *testing.T) {
	now := time.Unix(1700000000, 0)
	claims := TokenClaims{TenantID: "11111111-1111-4111-8111-111111111111", SandboxID: "22222222-2222-4222-8222-222222222222", TokenID: "33333333-3333-4333-8333-333333333333", Scopes: []string{"files"}, ExpiresAt: now.Add(time.Second).Unix()}
	command := TokenCommand{TenantID: claims.TenantID, SandboxID: claims.SandboxID, TokenID: claims.TokenID, IdempotencyKey: "key", GovTenantID: 7, IssuerID: 42, Scopes: []string{"files"}, ExpiresIn: time.Second}
	repo := tokenExpiryRepo{record: TokenRecord{Claims: claims}, after: func() { now = now.Add(time.Second) }}
	service := &TokenService{repo: repo, signer: tokenExpirySigner{claims: claims}, now: func() time.Time { return now }}
	ctx := WithCaller(context.Background(), Caller{TenantID: claims.TenantID, Actor: "governance:user:42"})
	if _, _, err := service.Issue(ctx, command); !errors.Is(err, ErrConflict) {
		t.Fatalf("issued expired token: %v", err)
	}
}
