package storage

import (
	"encoding/base64"
	"encoding/json"
)

// cursorPayload 是不透明游标的内部结构；token 是 S3 continuation token。
type cursorPayload struct {
	BucketID string `json:"b"`
	Prefix   string `json:"p"`
	Token    string `json:"t"`
}

const maxCursorLength = 4096

// EncodeCursor 把 S3 continuation token 封装为绑定 bucket 与 prefix 的游标。
func EncodeCursor(bucketID, prefix, token string) string {
	body, _ := json.Marshal(cursorPayload{BucketID: bucketID, Prefix: prefix, Token: token})
	return base64.RawURLEncoding.EncodeToString(body)
}

// DecodeCursor 校验游标可解析且与本次 bucket/prefix 绑定一致；不匹配返回 ErrInvalid，
// 由调用方转 400，不降级为首页。
func DecodeCursor(cursor, bucketID, prefix string) (string, error) {
	if cursor == "" {
		return "", nil
	}
	if len(cursor) > maxCursorLength {
		return "", ErrInvalid
	}
	body, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(body) > maxCursorLength {
		return "", ErrInvalid
	}
	var payload cursorPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", ErrInvalid
	}
	if payload.BucketID != bucketID || payload.Prefix != prefix || payload.Token == "" {
		return "", ErrInvalid
	}
	return payload.Token, nil
}
