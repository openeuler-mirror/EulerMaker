package server

import (
	"context"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"

	"k8s.io/apiserver/pkg/authentication/authenticator"
	"k8s.io/apiserver/pkg/authentication/user"
	"k8s.io/apiserver/pkg/authorization/authorizer"
	genericapiserver "k8s.io/apiserver/pkg/server"
	"k8s.io/apiserver/pkg/server/dynamiccertificates"
)

const (
	gatewayIdentity    = "spiffe://eulermaker/internal/ebs-gateway"
	schedulerIdentity  = "spiffe://eulermaker/internal/scheduler"
	controllerIdentity = "spiffe://eulermaker/internal/controller-manager"
	healthIdentity     = "system:ebs-health"
)

func configureInternalClientAuthentication(config *genericapiserver.Config, caFile string) error {
	data, err := os.ReadFile(caFile)
	if err != nil {
		return fmt.Errorf("read internal client CA: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(data) {
		return fmt.Errorf("internal client CA contains no valid certificates")
	}
	provider, err := dynamiccertificates.NewStaticCAContent("internal-client-ca", data)
	if err != nil {
		return fmt.Errorf("load internal client CA: %w", err)
	}
	config.SecureServing.ClientCA = provider
	config.Authentication.Authenticator = internalClientAuthenticator(roots)
	// Operation-level authorization is a separate rollout. Until then, only the
	// three recognized component identities can reach the resource and IAM APIs.
	config.Authorization.Authorizer = internalComponentAuthorizer()
	return nil
}

func internalClientAuthenticator(roots *x509.CertPool) authenticator.Request {
	return authenticator.RequestFunc(func(req *http.Request) (*authenticator.Response, bool, error) {
		if isHealthPath(req.URL.Path) && req.Method == http.MethodGet {
			return &authenticator.Response{User: &user.DefaultInfo{Name: healthIdentity}}, true, nil
		}
		if req.TLS == nil || len(req.TLS.PeerCertificates) == 0 {
			return nil, false, nil
		}
		leaf := req.TLS.PeerCertificates[0]
		if len(leaf.ExtKeyUsage) != 1 || leaf.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
			return nil, false, fmt.Errorf("internal client certificate must be restricted to clientAuth")
		}
		intermediates := x509.NewCertPool()
		for _, cert := range req.TLS.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{
			Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		}); err != nil {
			return nil, false, fmt.Errorf("verify internal client certificate: %w", err)
		}
		if len(leaf.URIs) != 1 {
			return nil, false, fmt.Errorf("internal client certificate must contain exactly one URI SAN")
		}
		identity := leaf.URIs[0].String()
		if !isInternalComponent(identity) {
			return nil, false, fmt.Errorf("unrecognized internal component identity")
		}
		return &authenticator.Response{User: &user.DefaultInfo{Name: identity}}, true, nil
	})
}

func internalComponentAuthorizer() authorizer.Authorizer {
	return authorizer.AuthorizerFunc(func(_ context.Context, attrs authorizer.Attributes) (authorizer.Decision, string, error) {
		principal := attrs.GetUser()
		if principal == nil {
			return authorizer.DecisionDeny, "missing identity", nil
		}
		identity := principal.GetName()
		if identity == user.APIServerUser {
			return authorizer.DecisionAllow, "apiserver loopback", nil
		}
		if attrs.GetVerb() == "impersonate" {
			return authorizer.DecisionDeny, "impersonation is disabled", nil
		}
		if isInternalComponent(identity) {
			return authorizer.DecisionAllow, "authenticated internal component", nil
		}
		if identity == healthIdentity && attrs.GetVerb() == "get" && isHealthPath(attrs.GetPath()) {
			return authorizer.DecisionAllow, "health endpoint", nil
		}
		return authorizer.DecisionDeny, "unknown internal identity", nil
	})
}

func isInternalComponent(identity string) bool {
	switch identity {
	case gatewayIdentity, schedulerIdentity, controllerIdentity:
		return true
	default:
		return false
	}
}

func isHealthPath(path string) bool {
	switch path {
	case "/healthz", "/livez", "/readyz":
		return true
	default:
		return false
	}
}
