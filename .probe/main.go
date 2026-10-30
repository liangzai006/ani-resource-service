package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/google/uuid"
	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

func main() {
	addr := flag.String("addr", "ani-resource-service.ani-system.svc.cluster.local:19094", "image runtime address")
	caFile := flag.String("ca", "/certs/ca.crt", "CA file")
	certFile := flag.String("cert", "/certs/tls.crt", "client cert")
	keyFile := flag.String("key", "/certs/tls.key", "client key")
	serverName := flag.String("server-name", "ani-network-service", "server name")
	tenant := flag.String("tenant", "", "tenant id")
	cluster := flag.String("cluster", "", "cluster id")
	namespace := flag.String("namespace", "", "namespace")
	namespaceUID := flag.String("namespace-uid", "", "namespace uid")
	registry := flag.String("registry", "", "registry authority")
	space := flag.String("space", "", "space id")
	imageID := flag.String("image-id", "", "catalogue image id")
	imageScope := flag.String("image-scope", "", "catalogue image scope")
	timeout := flag.Duration("timeout", 30*time.Second, "timeout")
	flag.Parse()

	caPEM, err := os.ReadFile(*caFile)
	if err != nil {
		fail("read ca: %v", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		fail("ca parse failed")
	}
	cert, err := tls.LoadX509KeyPair(*certFile, *keyFile)
	if err != nil {
		fail("load keypair: %v", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		fail("parse leaf: %v", err)
	}
	fmt.Printf("client DNS SANs: %v\n", leaf.DNSNames)

	creds := credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		RootCAs:      roots,
		Certificates: []tls.Certificate{cert},
		ServerName:   *serverName,
	})
	conn, err := grpc.NewClient(*addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		fail("dial: %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	ctx = metadata.AppendToOutgoingContext(ctx,
		"x-ani-tenant-id", *tenant,
		"x-ani-request-id", uuid.NewString(),
	)

	client := imagev1.NewImageRuntimeServiceClient(conn)

	if *imageID != "" {
		fmt.Println("=== ResolveImageForWorkload")
		scope := imagev1.ImageScope_IMAGE_SCOPE_PLATFORM
		if *imageScope == "tenant" {
			scope = imagev1.ImageScope_IMAGE_SCOPE_TENANT
		}
		resp, err := client.ResolveImageForWorkload(ctx, &imagev1.ResolveImageForWorkloadRequest{
			TenantId: *tenant,
			Scope:    scope,
			ImageId:  *imageID,
			TargetPlatform: &imagev1.ImagePlatform{
				Os:           "linux",
				Architecture: "amd64",
			},
		})
		if err != nil {
			fmt.Printf("Resolve error: %v\n", err)
		} else {
			out, _ := json.MarshalIndent(resp, "", "  ")
			fmt.Println(string(out))
		}
	}

	fmt.Println("=== EnsureTenantPullSecret")
	resp, err := client.EnsureTenantPullSecret(ctx, &imagev1.EnsureTenantPullSecretRequest{
		TenantId:          *tenant,
		ClusterId:         *cluster,
		Namespace:         *namespace,
		NamespaceUid:      *namespaceUID,
		RegistryAuthority: *registry,
		SpaceId:           *space,
	})
	if err != nil {
		fmt.Printf("Ensure error: %v\n", err)
	} else {
		out, _ := json.MarshalIndent(resp, "", "  ")
		fmt.Println(string(out))
	}

	fmt.Println("=== GetTenantPullMaterial (must be denied)")
	_, err = client.GetTenantPullMaterial(ctx, &imagev1.GetTenantPullMaterialRequest{TenantId: *tenant})
	fmt.Printf("GetTenantPullMaterial error: %v\n", err)
}

func fail(format string, args ...any) {
	fmt.Printf(format+"\n", args...)
	os.Exit(1)
}
