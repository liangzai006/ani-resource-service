package sandbox

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"regexp"
	"strings"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/sandbox"
)

var tokenKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// TokenSigner holds provisioned, stable signing keys. It never generates a
// process-local fallback. Retain older keys while issued tokens remain valid.
type TokenSigner struct {
	current string
	keys    map[string][]byte
}

func NewTokenSigner(data []byte) (*TokenSigner, error) {
	var config struct {
		Current string            `json:"current"`
		Keys    map[string]string `json:"keys"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return nil, biz.ErrTokenUnavailable
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, biz.ErrTokenUnavailable
	}
	if !tokenKeyIDPattern.MatchString(config.Current) || len(config.Keys) == 0 || len(config.Keys) > 16 {
		return nil, biz.ErrTokenUnavailable
	}
	keys := make(map[string][]byte, len(config.Keys))
	for id, encoded := range config.Keys {
		if !tokenKeyIDPattern.MatchString(id) {
			return nil, biz.ErrTokenUnavailable
		}
		key, err := base64.StdEncoding.Strict().DecodeString(encoded)
		if err != nil || len(key) < 32 || len(key) > 128 {
			return nil, biz.ErrTokenUnavailable
		}
		keys[id] = key
	}
	if keys[config.Current] == nil {
		return nil, biz.ErrTokenUnavailable
	}
	return &TokenSigner{current: config.Current, keys: keys}, nil
}
func (s *TokenSigner) CurrentKeyID() string {
	if s == nil {
		return ""
	}
	return s.current
}
func validTokenClaims(c biz.TokenClaims) bool {
	if !tokenKeyIDPattern.MatchString(c.KeyID) || !biz.UUID(c.TenantID) || !biz.UUID(c.SandboxID) || !biz.UUID(c.TokenID) || c.Version < 1 || c.GovTenantID == 0 || c.IssuerID == 0 || c.IssuedAt <= 0 || c.ExpiresAt <= c.IssuedAt || c.ExpiresAt-c.IssuedAt > 3600 {
		return false
	}
	scopes, err := biz.NormalizeTokenScopes(c.Scopes)
	if err != nil || len(scopes) != len(c.Scopes) {
		return false
	}
	for i := range scopes {
		if scopes[i] != c.Scopes[i] {
			return false
		}
	}
	return true
}
func (s *TokenSigner) Sign(c biz.TokenClaims) (string, error) {
	if s == nil || !validTokenClaims(c) || len(s.keys[c.KeyID]) < 32 {
		return "", biz.ErrTokenUnavailable
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "", biz.ErrTokenUnavailable
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, s.keys[c.KeyID])
	_, _ = mac.Write([]byte(biz.SandboxTokenPrefix + body))
	return biz.SandboxTokenPrefix + body + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
func (s *TokenSigner) Verify(token string, now time.Time) (biz.TokenClaims, error) {
	if s == nil || len(token) > 4096 || !strings.HasPrefix(token, biz.SandboxTokenPrefix) {
		return biz.TokenClaims{}, biz.ErrTokenInvalid
	}
	body, signature, ok := strings.Cut(strings.TrimPrefix(token, biz.SandboxTokenPrefix), ".")
	if !ok || body == "" || signature == "" || strings.Contains(signature, ".") {
		return biz.TokenClaims{}, biz.ErrTokenInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil || len(raw) > 2048 {
		return biz.TokenClaims{}, biz.ErrTokenInvalid
	}
	var c biz.TokenClaims
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&c); err != nil {
		return biz.TokenClaims{}, biz.ErrTokenInvalid
	}
	if err = decoder.Decode(new(any)); err != io.EOF || !validTokenClaims(c) {
		return biz.TokenClaims{}, biz.ErrTokenInvalid
	}
	key := s.keys[c.KeyID]
	if len(key) < 32 {
		return biz.TokenClaims{}, biz.ErrTokenInvalid
	}
	actual, err := base64.RawURLEncoding.Strict().DecodeString(signature)
	if err != nil {
		return biz.TokenClaims{}, biz.ErrTokenInvalid
	}
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(biz.SandboxTokenPrefix + body))
	if !hmac.Equal(actual, mac.Sum(nil)) {
		return biz.TokenClaims{}, biz.ErrTokenInvalid
	}
	if now.IsZero() {
		now = time.Now()
	}
	if c.ExpiresAt <= now.Unix() {
		return biz.TokenClaims{}, biz.ErrTokenExpired
	}
	if c.IssuedAt > now.Unix()+30 {
		return biz.TokenClaims{}, biz.ErrTokenInvalid
	}
	return c, nil
}
