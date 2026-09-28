package runner

import (
	"bytes"
	"io"
	"sync"
	"time"
)

// timestampLogWriter prefixes each container log line when Runner receives it.
// It keeps line state across writes because Docker may split a line into chunks.
type timestampLogWriter struct {
	mu        sync.Mutex
	dst       io.Writer
	now       func() time.Time
	lineStart bool
}

func newTimestampLogWriter(dst io.Writer, now func() time.Time) *timestampLogWriter {
	return &timestampLogWriter{dst: dst, now: now, lineStart: true}
}

func (w *timestampLogWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	var output bytes.Buffer
	for _, b := range p {
		if w.lineStart {
			output.WriteByte('[')
			output.WriteString(w.now().UTC().Format("2006-01-02T15:04:05.000Z"))
			output.WriteString("] ")
			w.lineStart = false
		}
		output.WriteByte(b)
		if b == '\n' {
			w.lineStart = true
		}
	}

	if _, err := io.Copy(w.dst, &output); err != nil {
		return 0, err
	}
	return len(p), nil
}
