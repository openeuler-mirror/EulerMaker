package options

import "testing"

func TestDevelopmentModeWithoutClientCertificate(t *testing.T) {
	o, err := Parse([]string{
		"--apiserver=https://localhost:8443",
		"--insecure-skip-verify=true",
	})
	if err != nil {
		t.Fatal(err)
	}
	config, err := o.RESTConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.TLSClientConfig.CertFile != "" || config.TLSClientConfig.KeyFile != "" {
		t.Fatal("scheduler must not configure a client certificate")
	}
}

func TestServerCARequiredByDefault(t *testing.T) {
	if _, err := Parse([]string{"--apiserver=https://localhost:8443"}); err == nil {
		t.Fatal("expected missing server CA error")
	}
}

func TestClientCertificateOptions(t *testing.T) {
	if _, err := Parse([]string{
		"--apiserver=https://localhost:8443",
		"--insecure-skip-verify=true",
		"--apiserver-client-cert=client.crt",
	}); err == nil {
		t.Fatal("unpaired client certificate was accepted")
	}
	o, err := Parse([]string{
		"--apiserver=https://localhost:8443", "--insecure-skip-verify=true",
		"--apiserver-client-cert=client.crt", "--apiserver-client-key=client.key",
	})
	if err != nil {
		t.Fatal(err)
	}
	if o.APIServerClientCert != "client.crt" || o.APIServerClientKey != "client.key" {
		t.Fatalf("unexpected client certificate options: %+v", o)
	}
}
