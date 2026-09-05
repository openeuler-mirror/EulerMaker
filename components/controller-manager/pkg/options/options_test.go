package options

import "testing"

func TestParseDevelopmentOptions(t *testing.T) {
	o, err := Parse([]string{"--apiserver=https://api:8443", "--insecure-skip-verify=true"})
	if err != nil {
		t.Fatal(err)
	}
	if o.Workers != 2 || o.PollPageSize != 500 {
		t.Fatalf("unexpected defaults: %+v", o)
	}
}
func TestParseRequiresTLSConfiguration(t *testing.T) {
	if _, err := Parse([]string{"--apiserver=https://api:8443"}); err == nil {
		t.Fatal("expected TLS validation error")
	}
}
