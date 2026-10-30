package sandbox

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func TestFileRequestLimitsAndPaths(t *testing.T) {
	for _, path := range []string{"../bad", "/absolute", "a/../b", "a//b", "a\\b", ""} {
		if err := ValidateFileRequest(FileRequest{Action: "write", Path: path}); !errors.Is(err, ErrInvalid) {
			t.Fatalf("path %q: %v", path, err)
		}
	}
	if err := ValidateFileRequest(FileRequest{Action: "write", Path: "ok", ContentBase64: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", SandboxFileMaxBytes)))}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateFileRequest(FileRequest{Action: "write", Path: "ok", ContentBase64: base64.StdEncoding.EncodeToString([]byte(strings.Repeat("x", SandboxFileMaxBytes+1)))}); !errors.Is(err, ErrFileTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
	if err := ValidateFileRequest(FileRequest{Action: "list", Limit: 501}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("limit: %v", err)
	}
}
