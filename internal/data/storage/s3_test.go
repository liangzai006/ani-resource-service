package storage

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/storage"
)

type fakeAPIError struct{ code string }

func (e fakeAPIError) Error() string     { return e.code }
func (e fakeAPIError) ErrorCode() string { return e.code }

func TestEndpointURL(t *testing.T) {
	for _, valid := range []string{"https://s3.example:9000", "http://10.0.0.1:9000/"} {
		if _, err := endpointURL(valid); err != nil {
			t.Fatalf("%s must be accepted: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "s3.example:9000", "ftp://s3.example", "https://user:pass@s3.example", "https://s3.example?x=1", "https://s3.example#f"} {
		if _, err := endpointURL(invalid); err == nil {
			t.Fatalf("%q must be rejected", invalid)
		}
	}
}

func TestReadCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(path, []byte("# comment\naccess_key=AK\nsecret_key=SK\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	access, secret, err := readCredentials(path)
	if err != nil || access != "AK" || secret != "SK" {
		t.Fatalf("credentials = %q/%q, %v", access, secret, err)
	}
	if _, _, err := readCredentials(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("missing credentials file must fail")
	}
}

func TestS3ErrorMapping(t *testing.T) {
	cases := []struct {
		code string
		want error
	}{
		{"NoSuchKey", biz.ErrNotFound},
		{"NoSuchBucket", biz.ErrNotFound},
		{"AccessDenied", biz.ErrAccessDenied},
		{"SignatureDoesNotMatch", biz.ErrAccessDenied},
		{"SlowDown", biz.ErrBackendUnavailable},
	}
	for _, tc := range cases {
		mapped := s3Error(fakeAPIError{code: tc.code})
		if !errors.Is(mapped, tc.want) {
			t.Fatalf("%s mapped to %v, want %v", tc.code, mapped, tc.want)
		}
	}
}
