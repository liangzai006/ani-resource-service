// Package storage owns the independent-volume and read-only object use cases.
// It declares ports only; Kubernetes and S3 details stay in internal/data.
package storage

import "errors"

// 稳定错误码由 service 层映射为 gRPC status；页面文案在 Gov 侧统一处理。
var (
	ErrInvalid             = errors.New("INVALID_STORAGE_REQUEST")
	ErrScopeDenied         = errors.New("STORAGE_SCOPE_DENIED")
	ErrNotFound            = errors.New("STORAGE_RESOURCE_NOT_FOUND")
	ErrAccessDenied        = errors.New("STORAGE_ACCESS_DENIED")
	ErrCreateConflict      = errors.New("CREATE_TOKEN_CONFLICT")
	ErrIdentityChanged     = errors.New("VOLUME_IDENTITY_CHANGED")
	ErrCapacityDecrease    = errors.New("CAPACITY_MUST_NOT_DECREASE")
	ErrVolumeInUse         = errors.New("VOLUME_IN_USE")
	ErrUsageUnknown        = errors.New("VOLUME_USAGE_UNKNOWN")
	ErrExpansionDenied     = errors.New("VOLUME_EXPANSION_UNSUPPORTED")
	ErrConflict            = errors.New("STORAGE_RESOURCE_CONFLICT")
	ErrVolumeTerminating   = errors.New("VOLUME_TERMINATING")
	ErrDownloadUnavailable = errors.New("DOWNLOAD_ENDPOINT_UNAVAILABLE")
	ErrBackendUnavailable  = errors.New("STORAGE_BACKEND_UNAVAILABLE")
	// ErrNotInstalled 仅用于区分"CRD 未安装"与其他查询失败，由 data 层返回。
	ErrNotInstalled = errors.New("STORAGE_CRD_NOT_INSTALLED")
)

// InUseError 携带本租户可观察到的引用，便于页面展示。
type InUseError struct {
	Consumers []Consumer
}

func (e *InUseError) Error() string { return ErrVolumeInUse.Error() }
func (e *InUseError) Unwrap() error { return ErrVolumeInUse }
