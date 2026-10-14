package runner

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

func TestTimestampLogWriterPrefixesLinesAcrossWrites(t *testing.T) {
	var output bytes.Buffer
	now := func() time.Time { return time.Date(2026, 9, 28, 10, 30, 15, 123000000, time.UTC) }
	writer := newTimestampLogWriter(&output, now)
	for _, chunk := range []string{"first", " line\n\nsecond\nlast", " without newline"} {
		if _, err := io.WriteString(writer, chunk); err != nil {
			t.Fatal(err)
		}
	}
	want := strings.Join([]string{
		"[2026-09-28T10:30:15.123Z] first line",
		"[2026-09-28T10:30:15.123Z] ",
		"[2026-09-28T10:30:15.123Z] second",
		"[2026-09-28T10:30:15.123Z] last without newline",
	}, "\n")
	if got := output.String(); got != want {
		t.Fatalf("log output = %q, want %q", got, want)
	}
}
