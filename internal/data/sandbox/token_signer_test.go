package sandbox

import (
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

func tokenTestConfig(current string, includeOld bool) []byte {
	key := base64.StdEncoding.EncodeToString([]byte(strings.Repeat("A", 32)))
	old := ""
	if includeOld {
		old = fmt.Sprintf(`,"old":"%s"`, base64.StdEncoding.EncodeToString([]byte(strings.Repeat("B", 32))))
	}
	return []byte(fmt.Sprintf(`{"current":"%s","keys":{"new":"%s"%s}}`, current, key, old))
}
func TestSandboxTokenSignerChecksSignatureExpiryAndRotation(t *testing.T) {
	signer, err := NewTokenSigner(tokenTestConfig("old", true))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	claims := biz.TokenClaims{KeyID: "old", TenantID: "11111111-1111-4111-8111-111111111111", SandboxID: "22222222-2222-4222-8222-222222222222", TokenID: "33333333-3333-4333-8333-333333333333", GovTenantID: 1, IssuerID: 2, Version: 3, Scopes: []string{"exec", "files"}, IssuedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix()}
	token, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := signer.Verify(token, now)
	if err != nil || !biz.EqualTokenClaims(decoded, claims) {
		t.Fatalf("roundtrip: %+v %v", decoded, err)
	}
	rotated, err := NewTokenSigner(tokenTestConfig("new", true))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = rotated.Verify(token, now); err != nil {
		t.Fatalf("retained old key: %v", err)
	}
	removed, _ := NewTokenSigner(tokenTestConfig("new", false))
	if _, err = removed.Verify(token, now); !errors.Is(err, biz.ErrTokenInvalid) {
		t.Fatalf("removed key: %v", err)
	}
	if _, err = rotated.Verify(token, now.Add(time.Hour)); !errors.Is(err, biz.ErrTokenExpired) {
		t.Fatalf("expiration: %v", err)
	}
	pieces := strings.Split(token, ".")
	pieces[len(pieces)-1] = strings.Repeat("A", 43)
	if _, err = rotated.Verify(strings.Join(pieces, "."), now); !errors.Is(err, biz.ErrTokenInvalid) {
		t.Fatalf("modified signature: %v", err)
	}
}
func TestSandboxTokenSignerRefusesMissingOrWeakKeys(t *testing.T) {
	for _, raw := range [][]byte{nil, []byte(`{"current":"a","keys":{}}`), []byte(`{"current":"a","keys":{"a":"YQ=="}}`)} {
		if _, err := NewTokenSigner(raw); !errors.Is(err, biz.ErrTokenUnavailable) {
			t.Fatalf("key accepted: %v", err)
		}
	}
}
