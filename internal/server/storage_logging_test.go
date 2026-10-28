package server

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/go-kratos/kratos/v3/transport"
	storage "github.com/zhangzhe-ctrl/ani-resource-service/api/storage/v1"
)

func TestStorageObjectKeyNotLogged(t *testing.T) {
	var out bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&out, nil))
	req := &storage.HeadObjectRequest{BucketId: "files", Key: "tenant/private-object-sentinel.txt"}
	ctx := transport.NewServerContext(context.Background(), imageLogTransport{storage.StorageService_HeadObject_FullMethodName})
	called := false
	_, e := requestLogging(logger)(func(_ context.Context, r any) (any, error) {
		called = true
		if r != req {
			t.Fatal("real request not delivered")
		}
		return &storage.HeadObjectResponse{}, nil
	})(ctx, req)
	if e != nil || !called || strings.Contains(out.String(), "private-object-sentinel") || !strings.Contains(out.String(), "[REDACTED]") {
		t.Fatal("storage object key leaked into request log")
	}
}
