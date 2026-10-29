package conf

import (
	"testing"
	"time"

	"google.golang.org/protobuf/types/known/durationpb"
)

func TestValidateServerDoesNotRequireNetworkConfiguration(t *testing.T) {
	cfg := &Bootstrap{Server: validConfig().Server}
	if err := cfg.ValidateServer(); err != nil {
		t.Fatal("server-only composition required an unrelated domain", err)
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("full composition stopped requiring Network")
	}
}

func TestValidateServerPreservesListenerBoundaries(t *testing.T) {
	for name, change := range map[string]func(*Bootstrap){
		"missing server":    func(c *Bootstrap) { c.Server = nil },
		"missing grpc":      func(c *Bootstrap) { c.Server.Grpc = nil },
		"missing admin":     func(c *Bootstrap) { c.Server.Admin = nil },
		"external listener": func(c *Bootstrap) { c.Server.Grpc.Addr = "192.0.2.1:19090" },
		"non-tcp listener":  func(c *Bootstrap) { c.Server.Admin.Network = "unix" },
		"zero port":         func(c *Bootstrap) { c.Server.Grpc.Addr = "0.0.0.0:0" },
		"duplicate port":    func(c *Bootstrap) { c.Server.Admin.Addr = "0.0.0.0:19090" },
		"unbounded timeout": func(c *Bootstrap) { c.Server.Grpc.Timeout = durationpb.New(time.Minute) },
		"missing shutdown":  func(c *Bootstrap) { c.Server.ShutdownTimeout = nil },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := &Bootstrap{Server: validConfig().Server}
			change(cfg)
			if err := cfg.ValidateServer(); err == nil {
				t.Fatal("server-only composition accepted invalid listener configuration")
			}
		})
	}
	var absent *Bootstrap
	if err := absent.ValidateServer(); err == nil {
		t.Fatal("nil bootstrap accepted")
	}
}
