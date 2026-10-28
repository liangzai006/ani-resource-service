package main

import (
	"fmt"
	"os"
	"time"

	"github.com/go-kratos/kratos/v3/middleware"
	kratosgrpc "github.com/go-kratos/kratos/v3/transport/grpc"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"github.com/zhangzhe-ctrl/ani-resource-service/internal/server"
	imageservice "github.com/zhangzhe-ctrl/ani-resource-service/internal/service/image"
	"google.golang.org/protobuf/types/known/durationpb"
)

func openImageRuntime(images *imageComponents, middlewares ...middleware.Middleware) (*kratosgrpc.Server, error) {
	addr := os.Getenv("ANI_IMAGE_RUNTIME_ADDR")
	if addr == "" {
		for _, key := range []string{"ANI_IMAGE_RUNTIME_CA", "ANI_IMAGE_RUNTIME_CERT", "ANI_IMAGE_RUNTIME_KEY"} {
			if os.Getenv(key) != "" {
				return nil, fmt.Errorf("image runtime TLS configuration requires ANI_IMAGE_RUNTIME_ADDR")
			}
		}
		return nil, nil
	}
	if images == nil || images.runtime == nil {
		return nil, fmt.Errorf("image runtime listener requires enabled Image runtime")
	}
	tlsConfig, err := server.ImageRuntimeTLS(os.Getenv("ANI_IMAGE_RUNTIME_CA"), os.Getenv("ANI_IMAGE_RUNTIME_CERT"), os.Getenv("ANI_IMAGE_RUNTIME_KEY"))
	if err != nil {
		return nil, err
	}
	return server.NewImageRuntimeGRPCServer(&conf.Server_GRPC{Network: "tcp", Addr: addr, Timeout: durationpb.New(45 * time.Second)}, tlsConfig, imageservice.NewRuntimeService(images.runtime), middlewares...)
}
