package sandbox

import "embed"

// Files contains the immutable Sandbox schema history.
//
//go:embed *.sql
var Files embed.FS
