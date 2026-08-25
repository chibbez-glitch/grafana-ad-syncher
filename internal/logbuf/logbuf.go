// Package logbuf keeps the most recent log lines in memory so they can be
// served over the API. The standard logger keeps writing to stderr as well, so
// `docker logs` is unaffected — this is a second reader, not a replacement.
package logbuf

import (
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

// Line is one captured log line. Text keeps whatever the standard logger
// produced (including its own date/time prefix); At is when we captured it,
// which is what the `since` filter works on.
type Line struct {
	Seq  uint64    `json:"seq"`
	At   time.Time `json:"at"`
	Text string    `json:"text"`
}

// Buffer is a fixed-size ring of log lines. The zero value is not usable; use
// New. It implements io.Writer so it can be handed to log.SetOutput.
type Buffer struct {
	mu    sync.RWMutex
	lines []Line
	next  int
	full  bool
	seq   uint64
	part  strings.Builder
}

// New returns a buffer holding the last `size` lines. Sizes below 1 fall back
// to a small default rather than panicking on a bad env value.
func New(size int) *Buffer {
	if size < 1 {
		size = 1000
	}
	return &Buffer{lines: make([]Line, size)}
}

// Install wires the buffer into the standard logger alongside stderr and
// returns it, so callers can do `buf := logbuf.New(n).Install()`.
func (b *Buffer) Install() *Buffer {
	log.SetOutput(io.MultiWriter(os.Stderr, b))
	return b
}

// Write implements io.Writer. The standard logger hands us one complete line
// per call, but we split on newlines anyway and hold an unterminated tail until
// the rest arrives, so a direct writer that chunks differently still works.
func (b *Buffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now().UTC()
	rest := string(p)
	for {
		idx := strings.IndexByte(rest, '\n')
		if idx < 0 {
			b.part.WriteString(rest)
			break
		}
		b.part.WriteString(rest[:idx])
		b.appendLocked(b.part.String(), now)
		b.part.Reset()
		rest = rest[idx+1:]
	}
	return len(p), nil
}

func (b *Buffer) appendLocked(text string, at time.Time) {
	b.seq++
	b.lines[b.next] = Line{Seq: b.seq, At: at, Text: strings.TrimRight(text, "\r")}
	b.next++
	if b.next == len(b.lines) {
		b.next = 0
		b.full = true
	}
}

// Capacity is how many lines the ring holds before it starts overwriting.
func (b *Buffer) Capacity() int { return len(b.lines) }

// Snapshot returns the buffered lines oldest-first, after filtering.
//
// since zero means no time filter; substr empty means no text filter; tail <= 0
// means every line that passed the filters. Filtering happens before the tail
// cut, so `tail=50&q=grafana` yields the last 50 *matching* lines rather than
// the matches among the last 50.
func (b *Buffer) Snapshot(tail int, since time.Time, substr string) []Line {
	b.mu.RLock()
	defer b.mu.RUnlock()

	count := b.next
	start := 0
	if b.full {
		count = len(b.lines)
		start = b.next
	}

	needle := strings.ToLower(substr)
	out := make([]Line, 0, count)
	for i := 0; i < count; i++ {
		ln := b.lines[(start+i)%len(b.lines)]
		if !since.IsZero() && ln.At.Before(since) {
			continue
		}
		if needle != "" && !strings.Contains(strings.ToLower(ln.Text), needle) {
			continue
		}
		out = append(out, ln)
	}
	if tail > 0 && len(out) > tail {
		out = out[len(out)-tail:]
	}
	return out
}
