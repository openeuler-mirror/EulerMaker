package specname

import (
	"testing"

	kvalidation "k8s.io/apimachinery/pkg/util/validation"
)

func TestCodecRoundTrip(t *testing.T) {
	for _, name := range []string{"gcc", "dvd+rw-tools", "with_under_score", "-leading", "trailing.", "s_2B", "中文+包"} {
		encoded := Encode(name)
		if len(encoded) > 63 {
			t.Fatalf("test label too long: %q", encoded)
		}
		if reasons := kvalidation.IsValidLabelValue(encoded); len(reasons) != 0 {
			t.Fatalf("Encode(%q) produced invalid label %q: %v", name, encoded, reasons)
		}
		if decoded, ok := Decode(encoded); !ok || decoded != name {
			t.Fatalf("Decode(Encode(%q)) = %q, %v", name, decoded, ok)
		}
	}
	if got := Encode("dvd+rw-tools"); got != "sdvd_2Brw-tools" {
		t.Fatalf("Encode(dvd+rw-tools) = %q", got)
	}
	for _, value := range []string{"", "gcc", "sbad_", "sbad_2b", "sdvd+rw-tools", "sa."} {
		if _, ok := Decode(value); ok {
			t.Fatalf("Decode(%q) accepted malformed label", value)
		}
	}
}
