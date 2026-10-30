package server

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"github.com/go-kratos/kratos/v3/middleware"
	kratosgrpc "github.com/go-kratos/kratos/v3/transport/grpc"
	"github.com/google/uuid"
	imagev1 "github.com/zhangzhe-ctrl/ani-resource-service/api/image/v1"
	imagebiz "github.com/zhangzhe-ctrl/ani-resource-service/internal/biz/image"
	conf "github.com/zhangzhe-ctrl/ani-resource-service/internal/conf/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const (
	InferenceSAN = "ani-inference-service"
	// ModelDevSAN is the training workload owner. Both identities are trusted
	// for Resolve and Ensure only; neither may read pull material in plaintext.
	ModelDevSAN = "ani-modeldev-service"
)

// runtimeSANs is the exact client identity allowlist. Matching remains literal
// DNS equality: no wildcard, URI, IP or extra SAN is accepted.
var runtimeSANs = []string{InferenceSAN, ModelDevSAN}

func trustedRuntimeIdentity(cert *x509.Certificate) bool {
	for _, san := range runtimeSANs {
		if exactClientDNS(cert, san) {
			return true
		}
	}
	return false
}

func matchedRuntimeSAN(cert *x509.Certificate) string {
	for _, san := range runtimeSANs {
		if exactClientDNS(cert, san) {
			return san
		}
	}
	return ""
}

// ImageRuntimeTLS authenticates the independent workload owners. Keeping this
// listener separate leaves Governance's certificate and method policy intact.
func ImageRuntimeTLS(caFile, certFile, keyFile string) (*tls.Config, error) {
	ca, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("read image runtime CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return nil, fmt.Errorf("image runtime CA contains no certificates")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("load image runtime server certificate: %w", err)
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil || !exactDNS(leaf, NetworkSAN) {
		return nil, fmt.Errorf("image runtime server certificate requires exact DNS SAN ani-network-service")
	}
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert,
		VerifyConnection: func(cs tls.ConnectionState) error {
			if cs.Version != tls.VersionTLS13 || len(cs.VerifiedChains) == 0 || len(cs.PeerCertificates) == 0 || !trustedRuntimeIdentity(cs.PeerCertificates[0]) {
				return fmt.Errorf("untrusted image runtime service identity")
			}
			return nil
		}}, nil
}

// ImageRuntimeUnary trusts tenant assertions only from an authenticated
// workload owner, after it has authorized and frozen the business command.
// GetTenantPullMaterial is deliberately not in the allowed method set: the
// unified Secret writer holds the only plaintext path.
func ImageRuntimeUnary() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		denied := status.Error(codes.Unauthenticated, "trusted runtime identity required")
		p, ok := peer.FromContext(ctx)
		if !ok || p == nil {
			return nil, denied
		}
		ti, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok || ti.State.Version != tls.VersionTLS13 || len(ti.State.VerifiedChains) == 0 || len(ti.State.PeerCertificates) == 0 {
			return nil, denied
		}
		subject := matchedRuntimeSAN(ti.State.PeerCertificates[0])
		if subject == "" {
			return nil, denied
		}
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, denied
		}
		canonicalUUID := func(key string) string {
			values := md.Get(key)
			if len(values) != 1 {
				return ""
			}
			id, err := uuid.Parse(values[0])
			if err != nil || id == uuid.Nil || id.String() != values[0] {
				return ""
			}
			return values[0]
		}
		tenant := canonicalUUID("x-ani-tenant-id")
		if tenant == "" || canonicalUUID("x-ani-request-id") == "" {
			return nil, denied
		}
		for _, key := range []string{"x-ani-actor", OperatorHeader, GovernanceScopeHeader} {
			if len(md.Get(key)) != 0 {
				return nil, denied
			}
		}
		var requestedTenant string
		switch info.FullMethod {
		case imagev1.ImageRuntimeService_ResolveImageForWorkload_FullMethodName:
			r, ok := req.(*imagev1.ResolveImageForWorkloadRequest)
			if !ok || r == nil {
				return nil, status.Error(codes.PermissionDenied, "runtime identity does not permit this request")
			}
			requestedTenant = r.GetTenantId()
		case imagev1.ImageRuntimeService_EnsureTenantPullSecret_FullMethodName:
			r, ok := req.(*imagev1.EnsureTenantPullSecretRequest)
			if !ok || r == nil {
				return nil, status.Error(codes.PermissionDenied, "runtime identity does not permit this request")
			}
			requestedTenant = r.GetTenantId()
		default:
			return nil, status.Error(codes.PermissionDenied, "runtime identity does not permit this method")
		}
		if requestedTenant != tenant {
			return nil, status.Error(codes.PermissionDenied, "image runtime tenant differs from authenticated scope")
		}
		ctx = imagebiz.WithCaller(ctx, imagebiz.Caller{Kind: imagebiz.RuntimeCaller, Subject: subject, TenantID: tenant})
		return next(ctx, req)
	}
}

// NewImageRuntimeGRPCServer exposes only the trusted runtime methods. It has no
// HTTP, health, reflection, catalog or streaming surface.
func NewImageRuntimeGRPCServer(c *conf.Server_GRPC, tlsConfig *tls.Config, runtime imagev1.ImageRuntimeServiceServer, middlewares ...middleware.Middleware) (*kratosgrpc.Server, error) {
	if c == nil || c.Addr == "" || runtime == nil || tlsConfig == nil || tlsConfig.MinVersion < tls.VersionTLS13 ||
		tlsConfig.ClientAuth != tls.RequireAndVerifyClientCert || tlsConfig.ClientCAs == nil || len(tlsConfig.Certificates) == 0 || tlsConfig.VerifyConnection == nil {
		return nil, fmt.Errorf("image runtime listener requires configured TLS 1.3 mutual authentication")
	}
	s := kratosgrpc.NewServer(
		kratosgrpc.CustomHealth(),
		kratosgrpc.Network(c.Network), kratosgrpc.Address(c.Addr), kratosgrpc.Timeout(c.Timeout.AsDuration()),
		kratosgrpc.TLSConfig(tlsConfig.Clone()),
		kratosgrpc.UnaryInterceptor(ImageRuntimeUnary()),
		kratosgrpc.StreamInterceptor(func(any, grpc.ServerStream, *grpc.StreamServerInfo, grpc.StreamHandler) error {
			return status.Error(codes.PermissionDenied, "image runtime streams are not permitted")
		}),
		kratosgrpc.Options(grpc.UnknownServiceHandler(func(any, grpc.ServerStream) error {
			return status.Error(codes.PermissionDenied, "image runtime RPC is not registered")
		})),
		kratosgrpc.Middleware(middlewares...), kratosgrpc.DisableReflection(),
	)
	imagev1.RegisterImageRuntimeServiceServer(s, runtime)
	return s, nil
}
