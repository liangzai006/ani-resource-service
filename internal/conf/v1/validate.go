// Package conf contains the generated and validated runtime configuration.
package conf

import (
	"encoding/base64"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"

	biz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/network"
	"google.golang.org/protobuf/types/known/durationpb"
)

const maximumTimeout = 30 * time.Second

// ValidateServer validates the shared listeners without requiring an unrelated
// domain to be configured. Domain compositions still validate their own inputs.
func (c *Bootstrap) ValidateServer() error {
	if c == nil {
		return fmt.Errorf("bootstrap config is required")
	}
	if c.Server == nil || c.Server.Grpc == nil || c.Server.Admin == nil {
		return fmt.Errorf("grpc and admin server config are required")
	}
	if err := validateListener("grpc", c.Server.Grpc.Network, c.Server.Grpc.Addr, c.Server.Grpc.Timeout); err != nil {
		return err
	}
	if err := validateListener("admin", c.Server.Admin.Network, c.Server.Admin.Addr, c.Server.Admin.Timeout); err != nil {
		return err
	}
	_, grpcPortText, _ := net.SplitHostPort(c.Server.Grpc.Addr)
	_, adminPortText, _ := net.SplitHostPort(c.Server.Admin.Addr)
	grpcPort, _ := strconv.Atoi(grpcPortText)
	adminPort, _ := strconv.Atoi(adminPortText)
	if grpcPort == adminPort {
		return fmt.Errorf("grpc and admin listeners must use distinct ports")
	}
	if err := validateDuration("shutdown", c.Server.ShutdownTimeout, maximumTimeout); err != nil {
		return err
	}
	return nil
}

func (c *Bootstrap) Validate() error {
	if err := c.ValidateServer(); err != nil {
		return err
	}
	if c.Network == nil || c.Network.Worker == nil || strings.TrimSpace(c.Network.DatabaseDsn) == "" ||
		strings.TrimSpace(c.Network.ClusterId) == "" || c.Network.NamespacePrefix == "" {
		return fmt.Errorf("Network database, placement and worker config are required")
	}
	if _, err := biz.NewVPCCIDRPresets(c.Network.VpcCidrPresets); err != nil {
		return fmt.Errorf("network.vpc_cidr_presets: %w", err)
	}
	key, err := base64.StdEncoding.DecodeString(c.Network.CursorSigningKey)
	if err != nil || len(key) < 32 || len(key) > 128 {
		return fmt.Errorf("cursor_signing_key must encode 32..128 bytes as base64")
	}
	w := c.Network.Worker
	for _, setting := range []struct {
		name    string
		value   *durationpb.Duration
		maximum time.Duration
	}{
		{"worker lease", w.Lease, 2 * time.Minute}, {"provider request", w.RequestTimeout, 30 * time.Second},
		{"observation interval", w.ObserveEvery, time.Minute}, {"observation freshness", w.StaleAfter, 10 * time.Minute},
		{"retry minimum", w.RetryMin, time.Minute}, {"retry maximum", w.RetryMax, time.Minute},
		{"worker poll", w.PollInterval, time.Second},
	} {
		if err := validateDuration(setting.name, setting.value, setting.maximum); err != nil {
			return err
		}
		if setting.value.AsDuration() < time.Millisecond {
			return fmt.Errorf("%s must be at least 1ms", setting.name)
		}
	}
	if w.Lease.AsDuration() < 3*w.RequestTimeout.AsDuration() || w.StaleAfter.AsDuration() <= w.ObserveEvery.AsDuration() || w.RetryMax.AsDuration() < w.RetryMin.AsDuration() {
		return fmt.Errorf("worker lease, observation and retry timing are inconsistent")
	}
	if o := c.Network.Observation; o != nil {
		for _, v := range []struct {
			name  string
			value *durationpb.Duration
			max   time.Duration
		}{{"audit interval", o.AuditInterval, 40 * time.Second}, {"audit jitter", o.AuditJitter, 5 * time.Second}, {"audit timeout", o.AuditTimeout, 15 * time.Second}, {"notification flush", o.FlushInterval, time.Second}} {
			if err := validateDuration(v.name, v.value, v.max); err != nil {
				return err
			}
		}
		if o.RequestQps < 1 || o.RequestQps > 100 || o.RequestBurst < o.RequestQps || o.RequestBurst > 200 {
			return fmt.Errorf("invalid Kubernetes request budget")
		}
		if o.QueueCapacity < 16 || o.QueueCapacity > 65536 || o.WorkersPerKind < 1 || o.WorkersPerKind > 4 || o.AuditInterval.AsDuration()+o.AuditJitter.AsDuration()+o.AuditTimeout.AsDuration() >= w.StaleAfter.AsDuration() {
			return fmt.Errorf("invalid observation capacity or freshness budget")
		}
	}
	if lb := c.Network.LoadBalancer; lb != nil {
		if !regexp.MustCompile(`^[a-f0-9]{64}$`).MatchString(lb.InstallationFingerprint) {
			return fmt.Errorf("load_balancer requires the pinned installation fingerprint")
		}
		for _, id := range []string{lb.ControllerImageId, lb.EnvoyImageId, lb.ShutdownImageId, lb.KcImageId} {
			if !regexp.MustCompile(`^(?:[^\s@]+@)?sha256:[a-f0-9]{64}$`).MatchString(id) {
				return fmt.Errorf("load_balancer requires fixed running image IDs")
			}
		}
	}
	if err := c.ObjectStorage.Validate(); err != nil {
		return err
	}
	return c.Image.Validate()
}

func validateListener(name, network, address string, timeout *durationpb.Duration) error {
	if network != "tcp" {
		return fmt.Errorf("%s network must be tcp", name)
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%s address: %w", name, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || (!ip.IsLoopback() && !ip.IsUnspecified()) {
		return fmt.Errorf("%s address must use a literal loopback or unspecified IP", name)
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return fmt.Errorf("%s address requires a numeric port in 1..65535", name)
	}
	return validateDuration(name, timeout, maximumTimeout)
}

func validateDuration(name string, value *durationpb.Duration, maximum time.Duration) error {
	if value == nil {
		return fmt.Errorf("%s timeout is required", name)
	}
	if err := value.CheckValid(); err != nil {
		return fmt.Errorf("%s timeout: %w", name, err)
	}
	duration := value.AsDuration()
	if duration <= 0 || duration > maximum {
		return fmt.Errorf("%s timeout must be within 1ns..%s", name, maximum)
	}
	return nil
}
