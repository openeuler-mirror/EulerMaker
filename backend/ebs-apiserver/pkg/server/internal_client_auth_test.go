package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
)

func testClientCertificate(t *testing.T, names []string, usages []x509.ExtKeyUsage) (*x509.CertPool, *x509.Certificate, []byte) {
	t.Helper()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: usages,
	}
	for _, name := range names {
		uri, err := url.Parse(name)
		if err != nil {
			t.Fatal(err)
		}
		leafTemplate.URIs = append(leafTemplate.URIs, uri)
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTemplate, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(leafDER)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	return roots, leaf, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
}

func TestInternalClientAuthenticator(t *testing.T) {
	for _, name := range []string{gatewayIdentity, schedulerIdentity, controllerIdentity} {
		t.Run(name, func(t *testing.T) {
			roots, leaf, _ := testClientCertificate(t, []string{name}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
			req, _ := http.NewRequest(http.MethodGet, "https://api.example/apis/ebs/v1/jobs", nil)
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
			response, ok, err := internalClientAuthenticator(roots).AuthenticateRequest(req)
			if err != nil || !ok || response.User.GetName() != name {
				t.Fatalf("authenticate %q: response=%v ok=%v err=%v", name, response, ok, err)
			}
		})
	}
	for _, test := range []struct {
		name   string
		uris   []string
		usages []x509.ExtKeyUsage
	}{
		{name: "unknown component", uris: []string{"spiffe://eulermaker/internal/other"}, usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
		{name: "server certificate", uris: []string{gatewayIdentity}, usages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}},
		{name: "dual-use certificate", uris: []string{gatewayIdentity}, usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}},
		{name: "multiple identities", uris: []string{gatewayIdentity, schedulerIdentity}, usages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}},
	} {
		t.Run(test.name, func(t *testing.T) {
			roots, leaf, _ := testClientCertificate(t, test.uris, test.usages)
			req, _ := http.NewRequest(http.MethodGet, "https://api.example/apis/ebs/v1/jobs", nil)
			req.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}
			if _, ok, _ := internalClientAuthenticator(roots).AuthenticateRequest(req); ok {
				t.Fatal("unexpectedly authenticated")
			}
		})
	}
	resourceRequest, _ := http.NewRequest(http.MethodGet, "https://api.example/apis/ebs/v1/jobs", nil)
	if _, ok, _ := internalClientAuthenticator(x509.NewCertPool()).AuthenticateRequest(resourceRequest); ok {
		t.Fatal("resource request without a client certificate was authenticated")
	}
	_, untrustedLeaf, _ := testClientCertificate(t, []string{gatewayIdentity}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	resourceRequest.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{untrustedLeaf}}
	if _, ok, _ := internalClientAuthenticator(x509.NewCertPool()).AuthenticateRequest(resourceRequest); ok {
		t.Fatal("client certificate from an untrusted CA was authenticated")
	}
	request, _ := http.NewRequest(http.MethodGet, "https://api.example/healthz", nil)
	response, ok, err := internalClientAuthenticator(x509.NewCertPool()).AuthenticateRequest(request)
	if err != nil || !ok || response.User.GetName() != healthIdentity {
		t.Fatalf("health authentication: response=%v ok=%v err=%v", response, ok, err)
	}
}

func TestInternalClientAuthenticationConfig(t *testing.T) {
	_, _, caPEM := testClientCertificate(t, []string{gatewayIdentity}, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	caPath := filepath.Join(t.TempDir(), "client-ca.crt")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	opts := NewEulerMakerServerOptions()
	opts.InternalClientCAFile = caPath
	opts.RecommendedOptions.SecureServing.Listener = listener
	config, err := opts.Config()
	if err != nil {
		t.Fatal(err)
	}
	if config.Authentication.Authenticator == nil || config.Authorization.Authorizer == nil || config.SecureServing.ClientCA == nil {
		t.Fatal("internal client authentication was not installed")
	}
	if config.LoopbackClientConfig == nil || config.LoopbackClientConfig.BearerToken == "" {
		t.Fatal("apiserver loopback credentials were not generated")
	}
	completed := config.Complete()
	request, _ := http.NewRequest(http.MethodGet, "https://api.example/readyz", nil)
	request.Header.Set("Authorization", "Bearer "+config.LoopbackClientConfig.BearerToken)
	response, ok, err := completed.Authentication.Authenticator.AuthenticateRequest(request)
	if err != nil || !ok || response.User.GetName() != user.APIServerUser {
		t.Fatalf("loopback authentication: response=%v ok=%v err=%v", response, ok, err)
	}
}

func TestInternalComponentAuthorizer(t *testing.T) {
	a := internalComponentAuthorizer()
	for _, identity := range []string{gatewayIdentity, schedulerIdentity, controllerIdentity, user.APIServerUser} {
		decision, _, err := a.Authorize(t.Context(), authorizer.AttributesRecord{User: &user.DefaultInfo{Name: identity}})
		if err != nil || decision != authorizer.DecisionAllow {
			t.Fatalf("identity %q: decision=%v err=%v", identity, decision, err)
		}
	}
	for _, path := range []string{"/healthz", "/apis/ebs/v1/jobs"} {
		decision, _, err := a.Authorize(t.Context(), authorizer.AttributesRecord{
			User: &user.DefaultInfo{Name: healthIdentity}, Verb: "get", Path: path,
		})
		if err != nil || (decision == authorizer.DecisionAllow) != (path == "/healthz") {
			t.Fatalf("health path %q: decision=%v err=%v", path, decision, err)
		}
	}
	decision, _, err := a.Authorize(t.Context(), authorizer.AttributesRecord{
		User: &user.DefaultInfo{Name: gatewayIdentity}, Verb: "impersonate", Path: "/apis/ebs/v1/jobs",
	})
	if err != nil || decision != authorizer.DecisionDeny {
		t.Fatalf("component impersonation: decision=%v err=%v", decision, err)
	}
}
