package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	ErrTokenInvalid     = errors.New("SANDBOX_TOKEN_INVALID")
	ErrTokenExpired     = errors.New("SANDBOX_TOKEN_EXPIRED")
	ErrTokenUnavailable = errors.New("SANDBOX_TOKEN_UNAVAILABLE")
)

const SandboxTokenPrefix = "ani.sbx."

type TokenClaims struct {
	KeyID       string   `json:"kid"`
	TenantID    string   `json:"tid"`
	SandboxID   string   `json:"sid"`
	Version     int64    `json:"ver"`
	TokenID     string   `json:"jti"`
	GovTenantID uint32   `json:"gtid"`
	IssuerID    uint32   `json:"uid"`
	Scopes      []string `json:"scp"`
	IssuedAt    int64    `json:"iat"`
	ExpiresAt   int64    `json:"exp"`
}
type TokenCommand struct {
	TenantID, SandboxID, TokenID, IdempotencyKey string
	GovTenantID, IssuerID                        uint32
	Scopes                                       []string
	ExpiresIn                                    time.Duration
}
type TokenRecord struct {
	Claims   TokenClaims
	Revoked  bool
	Replayed bool
}
type TokenSigner interface {
	CurrentKeyID() string
	Sign(TokenClaims) (string, error)
	Verify(string, time.Time) (TokenClaims, error)
}
type TokenRepository interface {
	AcceptToken(context.Context, TokenCommand, string, time.Time) (TokenRecord, error)
	GetToken(context.Context, string, string, string) (TokenRecord, error)
	RevokeToken(context.Context, string, string, string) error
}
type TokenService struct {
	repo   TokenRepository
	signer TokenSigner
	now    func() time.Time
}

func NewTokenService(repo TokenRepository, signer TokenSigner) (*TokenService, error) {
	if repo == nil || signer == nil || signer.CurrentKeyID() == "" {
		return nil, ErrTokenUnavailable
	}
	return &TokenService{repo: repo, signer: signer, now: time.Now}, nil
}
func NormalizeTokenScopes(scopes []string) ([]string, error) {
	if len(scopes) == 0 {
		scopes = []string{"connect"}
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		switch scope {
		case "connect", "exec", "files", "ports":
		default:
			return nil, ErrInvalid
		}
		if !seen[scope] {
			seen[scope] = true
			out = append(out, scope)
		}
	}
	sort.Strings(out)
	return out, nil
}
func TokenDigest(c TokenCommand) (string, error) {
	if !UUID(c.TenantID) || !UUID(c.SandboxID) || !UUID(c.TokenID) || c.GovTenantID == 0 || c.IssuerID == 0 || c.IdempotencyKey == "" || !utf8.ValidString(c.IdempotencyKey) || strings.ContainsRune(c.IdempotencyKey, 0) || len([]rune(c.IdempotencyKey)) > 128 || c.ExpiresIn <= 0 || c.ExpiresIn > time.Hour || c.ExpiresIn%time.Second != 0 {
		return "", ErrInvalid
	}
	scopes, err := NormalizeTokenScopes(c.Scopes)
	if err != nil {
		return "", err
	}
	if len(scopes) != len(c.Scopes) {
		return "", ErrInvalid
	}
	for i := range scopes {
		if scopes[i] != c.Scopes[i] {
			return "", ErrInvalid
		}
	}
	raw, err := json.Marshal(struct {
		Version               int
		GovTenantID, IssuerID uint32
		Scopes                []string
		ExpiresIn             int64
	}{1, c.GovTenantID, c.IssuerID, c.Scopes, int64(c.ExpiresIn / time.Second)})
	if err != nil {
		return "", ErrInvalid
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
func (s *TokenService) Issue(ctx context.Context, c TokenCommand) (TokenRecord, string, error) {
	if err := RequireTenant(ctx, c.TenantID); err != nil {
		return TokenRecord{}, "", err
	}
	caller, ok := ctx.Value(callerKey{}).(Caller)
	if !ok || caller.Actor != "governance:user:"+strconv.FormatUint(uint64(c.IssuerID), 10) {
		return TokenRecord{}, "", ErrDenied
	}
	if _, err := TokenDigest(c); err != nil {
		return TokenRecord{}, "", err
	}
	now := s.now().UTC()
	record, err := s.repo.AcceptToken(ctx, c, s.signer.CurrentKeyID(), now)
	if err != nil {
		return TokenRecord{}, "", err
	}
	if record.Revoked || record.Claims.ExpiresAt <= s.now().UTC().Unix() {
		return TokenRecord{}, "", ErrConflict
	}
	token, err := s.signer.Sign(record.Claims)
	if err != nil {
		return TokenRecord{}, "", err
	}
	return record, token, nil
}
func (s *TokenService) Verify(ctx context.Context, tenant, id, token, scope string) (TokenClaims, error) {
	if err := RequireTenant(ctx, tenant); err != nil {
		return TokenClaims{}, err
	}
	if !UUID(id) || !UUID(tenant) {
		return TokenClaims{}, ErrInvalid
	}
	claims, err := s.signer.Verify(token, s.now().UTC())
	if err != nil {
		return TokenClaims{}, err
	}
	caller, ok := ctx.Value(callerKey{}).(Caller)
	if !ok || caller.Actor != "governance:sandbox-token:"+claims.TokenID {
		return TokenClaims{}, ErrDenied
	}
	if claims.TenantID != tenant || claims.SandboxID != id {
		return TokenClaims{}, ErrTokenInvalid
	}
	if scope == "" {
		return TokenClaims{}, ErrInvalid
	}
	{
		found := false
		for _, v := range claims.Scopes {
			if v == scope {
				found = true
				break
			}
		}
		if !found {
			return TokenClaims{}, ErrDenied
		}
	}
	record, err := s.repo.GetToken(ctx, tenant, id, claims.TokenID)
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrDataPlaneUnavailable) || errors.Is(err, ErrTokenInvalid) {
		return TokenClaims{}, ErrTokenInvalid
	}
	if err != nil {
		return TokenClaims{}, err
	}
	if record.Revoked || !EqualTokenClaims(record.Claims, claims) {
		return TokenClaims{}, ErrTokenInvalid
	}
	// A database lock or slow dependency can consume the remaining TTL.
	if claims.ExpiresAt <= s.now().UTC().Unix() {
		return TokenClaims{}, ErrTokenExpired
	}
	return claims, nil
}
func (s *TokenService) Revoke(ctx context.Context, tenant, id, tokenID string) error {
	caller, ok := ctx.Value(callerKey{}).(Caller)
	if !ok || !strings.HasPrefix(caller.Actor, "governance:user:") {
		return ErrDenied
	}
	return s.repo.RevokeToken(ctx, tenant, id, tokenID)
}
func EqualTokenClaims(a, b TokenClaims) bool {
	aa, e := json.Marshal(a)
	if e != nil {
		return false
	}
	bb, e := json.Marshal(b)
	return e == nil && string(aa) == string(bb)
}
