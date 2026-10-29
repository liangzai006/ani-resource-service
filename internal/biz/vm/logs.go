package vm

import (
	"context"
	"errors"
)

// DefaultLogLimits mirrors the container-instance log channel. Limits are
// operator-owned transport protection and counters stay process-local.
var DefaultLogLimits = LogLimits{FrameBytes: 16 * 1024, BufferBytes: 256 * 1024, OpenTimeoutSeconds: 15, WriteTimeoutSeconds: 10, LifetimeSeconds: 600, TargetPollSeconds: 2, ProcessConnections: 100, TenantConnections: 10, InstanceConnections: 3}

type LogLimits struct {
	FrameBytes, BufferBytes                                                     int
	OpenTimeoutSeconds, WriteTimeoutSeconds, LifetimeSeconds, TargetPollSeconds int
	ProcessConnections, TenantConnections, InstanceConnections                  int
}

func (v LogLimits) Defaults() LogLimits {
	d := DefaultLogLimits
	if v.FrameBytes > 0 {
		d.FrameBytes = v.FrameBytes
	}
	if v.BufferBytes > 0 {
		d.BufferBytes = v.BufferBytes
	}
	if v.OpenTimeoutSeconds > 0 {
		d.OpenTimeoutSeconds = v.OpenTimeoutSeconds
	}
	if v.WriteTimeoutSeconds > 0 {
		d.WriteTimeoutSeconds = v.WriteTimeoutSeconds
	}
	if v.LifetimeSeconds > 0 {
		d.LifetimeSeconds = v.LifetimeSeconds
	}
	if v.TargetPollSeconds > 0 {
		d.TargetPollSeconds = v.TargetPollSeconds
	}
	if v.ProcessConnections > 0 {
		d.ProcessConnections = v.ProcessConnections
	}
	if v.TenantConnections > 0 {
		d.TenantConnections = v.TenantConnections
	}
	if v.InstanceConnections > 0 {
		d.InstanceConnections = v.InstanceConnections
	}
	return d
}

func ValidateLogLimits(v LogLimits) error {
	d := v.Defaults()
	if d.FrameBytes < 1 || d.FrameBytes > d.BufferBytes/3 {
		return ErrInvalid
	}
	return nil
}

type LogFrame struct {
	Kind   string
	Data   []byte
	Reason string
}

var (
	// ErrLogTarget is public: a VM without a live launcher cannot serve logs.
	ErrLogTarget = errors.New("VM_LOG_TARGET_UNAVAILABLE")
	ErrLogLimit  = errors.New("VM_LOG_CONNECTION_LIMIT")
)

// LogPort reads the guest serial console while binding every frame to the
// launcher UID observed before the stream was opened.
type LogPort interface {
	StreamLogs(context.Context, VM, Observation, uint32, bool, func(LogFrame) error) error
}
