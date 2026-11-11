package runner

import (
	"bytes"
	"io"
)

// qmpPrefix marks a stdout line the guest prints to have forwarded to QMP.
var qmpPrefix = []byte("[QMP]")

// streamWriter reassembles one of the binary's output streams from the guest's
// console records and writes it to w byte for byte. A record is either a
// complete line or a chunk with no newline (see the guest's recordWriter).
//
// With onQMP set, a line starting with "[QMP]" is handed to onQMP instead of
// being written. Since such a line may arrive split over several chunks, output
// at the start of a line is held back for as long as it could still be one.
type streamWriter struct {
	w     io.Writer
	onQMP func(cmd string)

	midLine bool   // w's output is part-way through a line
	held    []byte // start of a line that may yet prove to be a [QMP] command
}

// record consumes one console record: text with its trailing newline for a
// complete line, or a chunk that had none (partial).
func (s *streamWriter) record(text []byte, partial bool) {
	if partial {
		text = bytes.TrimSuffix(bytes.TrimSuffix(text, []byte("\n")), []byte("\r"))
	}
	if s.onQMP == nil || s.midLine {
		_, _ = s.w.Write(text)
		s.midLine = partial
		return
	}
	s.held = append(s.held, text...)
	switch {
	case !partial && bytes.HasPrefix(s.held, qmpPrefix):
		s.onQMP(string(bytes.TrimRight(s.held[len(qmpPrefix):], "\r\n")))
		s.held = s.held[:0]
	case partial && (bytes.HasPrefix(s.held, qmpPrefix) || bytes.HasPrefix(qmpPrefix, s.held)):
		// Still undecided; keep holding.
	default:
		s.flush()
		s.midLine = partial
	}
}

// flush writes out any held text. Call once after the guest exits.
func (s *streamWriter) flush() {
	if len(s.held) != 0 {
		_, _ = s.w.Write(s.held)
		s.held = s.held[:0]
	}
}
