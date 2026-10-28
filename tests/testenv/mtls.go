package testenv

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ServiceTLS creates isolated credentials for the actual Resource composition.
// Its CA is private to one test and is never accepted by a deployed service.
type ServiceTLS struct {
	CAFile, CertFile, KeyFile                                                                                   string
	ClientCertFile, ClientKeyFile                                                                               string
	Client                                                                                                      *tls.Config
	SGClientCertFile, SGClientKeyFile, SGServerCertFile, SGServerKeyFile, GovControlCertFile, GovControlKeyFile string
}

func NewServiceTLS(t testing.TB) ServiceTLS {
	return newServiceTLS(t, false)
}
func NewInstanceServiceTLS(t testing.TB) ServiceTLS { return newServiceTLS(t, true) }
func newServiceTLS(t testing.TB, instance bool) ServiceTLS {
	return serviceTLSForClient(t, instance, "ani-governance")
}

func NewImageRuntimeServiceTLS(t testing.TB) ServiceTLS {
	return serviceTLSForClient(t, false, "ani-inference-service")
}

func serviceTLSForClient(t testing.TB, instance bool, clientName string) ServiceTLS {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "isolated service test CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name string, body []byte) string {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, body, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	caFile := write("ca.pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}))
	issue := func(name string, serial int64, usage x509.ExtKeyUsage) (tls.Certificate, string, string) {
		t.Helper()
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), DNSNames: []string{name}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}}
		identity := ""
		if name == "ani-session-gateway" {
			identity = "ani-session-gateway"
		}
		if name == "ani-governance-control" {
			identity = "ani-governance"
		}
		if identity != "" {
			uri, _ := url.Parse("spiffe://ani.internal/service/" + identity)
			template.URIs = []*url.URL{uri}
		}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalECPrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certFile := write(name+".pem", pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
		keyFile := write(name+".key", pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
		cert, err := tls.LoadX509KeyPair(certFile, keyFile)
		if err != nil {
			t.Fatal(err)
		}
		return cert, certFile, keyFile
	}
	_, certFile, keyFile := issue("ani-network-service", 2, x509.ExtKeyUsageServerAuth)
	client, clientCertFile, clientKeyFile := issue(clientName, 3, x509.ExtKeyUsageClientAuth)
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	result := ServiceTLS{CAFile: caFile, CertFile: certFile, KeyFile: keyFile, ClientCertFile: clientCertFile, ClientKeyFile: clientKeyFile, Client: &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: roots, ServerName: "ani-network-service", Certificates: []tls.Certificate{client}}}
	if instance {
		_, result.SGClientCertFile, result.SGClientKeyFile = issue("ani-session-gateway", 4, x509.ExtKeyUsageClientAuth)
		_, result.SGServerCertFile, result.SGServerKeyFile = issue("ani-session-gateway-server", 5, x509.ExtKeyUsageServerAuth)
		_, result.GovControlCertFile, result.GovControlKeyFile = issue("ani-governance-control", 6, x509.ExtKeyUsageClientAuth)
	}
	return result
}
