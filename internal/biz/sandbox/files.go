package sandbox

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"
)

const SandboxFileMaxBytes = 1 << 20

var ErrFileTooLarge = errors.New("SANDBOX_FILE_TOO_LARGE")

type FileRequest struct {
	Action, Path, ContentBase64 string
	Overwrite                   bool
	Limit                       uint32
	Cursor                      string
}

type FileEntry struct {
	Path        string `json:"path"`
	Kind        string `json:"kind"`
	SizeBytes   int64  `json:"size_bytes"`
	UpdatedAtNS int64  `json:"updated_at_ns,omitempty"`
}

type FileResult struct {
	Path          string      `json:"path,omitempty"`
	Kind          string      `json:"kind,omitempty"`
	SizeBytes     int64       `json:"size_bytes,omitempty"`
	ContentBase64 string      `json:"content_base64,omitempty"`
	Items         []FileEntry `json:"items,omitempty"`
	Total         int64       `json:"total,omitempty"`
	NextCursor    string      `json:"next_cursor,omitempty"`
}

// ValidateFileRequest rejects ambiguous or dangerous names before admission.
func ValidateFileRequest(r FileRequest) error {
	if r.Action != "list" && r.Action != "read" && r.Action != "write" && r.Action != "delete" {
		return ErrInvalid
	}
	if !utf8.ValidString(r.Path) || len(r.Path) > 4096 || strings.ContainsAny(r.Path, "\x00\\") || strings.HasPrefix(r.Path, "/") {
		return ErrInvalid
	}
	if !(r.Action == "list" && (r.Path == "" || r.Path == ".")) {
		for _, part := range strings.Split(r.Path, "/") {
			if part == "" || part == "." || part == ".." || len(part) > 255 {
				return ErrInvalid
			}
		}
	}
	if r.Action == "list" {
		if r.Limit > 500 {
			return ErrInvalid
		}
		if r.Cursor != "" {
			n, err := strconv.Atoi(r.Cursor)
			if err != nil || n < 0 || strconv.Itoa(n) != r.Cursor {
				return ErrInvalid
			}
		}
	} else if r.Limit != 0 || r.Cursor != "" {
		return ErrInvalid
	}
	if r.Action != "write" {
		if r.ContentBase64 != "" || r.Overwrite {
			return ErrInvalid
		}
		return nil
	}
	if len(r.ContentBase64) > base64.StdEncoding.EncodedLen(SandboxFileMaxBytes) {
		return ErrFileTooLarge
	}
	decoded, err := base64.StdEncoding.Strict().DecodeString(r.ContentBase64)
	if err != nil {
		return ErrInvalid
	}
	if len(decoded) > SandboxFileMaxBytes {
		return ErrFileTooLarge
	}
	return nil
}

type FileCommand struct {
	TenantID, SandboxID, OperationID, IdempotencyKey string
	ExpectedVersion                                  int64
	Request                                          FileRequest
}

type FileIntent struct {
	TenantID, SandboxID, OperationID, State, Reason string
	Target                                          DataPlaneTarget
	Result                                          FileResult
	Replayed                                        bool
}
