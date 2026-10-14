package signing

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestDisabledSignerDoesNotSign(t *testing.T) {
	signer, err := New(Config{Mode: Disabled})
	if err != nil {
		t.Fatal(err)
	}
	if signer.Fingerprint() != "" {
		t.Fatalf("disabled fingerprint = %q", signer.Fingerprint())
	}
	if err := signer.Sign(context.Background(), "unused.rpm"); err == nil {
		t.Fatal("disabled signer accepted a package")
	}
}

func TestSigningConfigurationRequiresKey(t *testing.T) {
	for _, mode := range []string{LocalGPG, Signatrust} {
		if _, err := New(Config{Mode: mode}); err == nil {
			t.Fatalf("%s accepted missing key", mode)
		}
	}
}

func TestPublicKeyFingerprintRejectsMalformedKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "public.asc")
	if err := os.WriteFile(path, []byte("not a PGP key"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := PublicKeyFingerprint(path); err == nil {
		t.Fatal("malformed key was accepted")
	}
}

func TestSignatureOutputRequiresSignatureLine(t *testing.T) {
	good := []byte("package.rpm:\n    Header V4 RSA/SHA256 Signature, key ID abcdef01: OK\n    Payload SHA256 digest: OK\n")
	if !signatureOK.Match(good) {
		t.Fatal("valid signature line was rejected")
	}
	misleadingName := []byte("Signature-OK.rpm:\n    Header SHA256 digest: OK\n    Payload SHA256 digest: OK\n")
	if signatureOK.Match(misleadingName) {
		t.Fatal("unsigned RPM filename was treated as a signature")
	}
}
