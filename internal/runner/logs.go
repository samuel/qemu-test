package runner

import "bytes"

type logParser struct {
	fn  func(key, line []byte) error
	buf []byte
}

func (l *logParser) Write(b []byte) (int, error) {
	l.buf = append(l.buf, b...)
	// Drain every buffered line: a single Write often delivers several at once,
	// and the last may be the verdict marker. The read offset is compacted once at
	// the end rather than copy-shifting after every line.
	var off int
	for {
		ix := bytes.IndexByte(l.buf[off:], '\n')
		if ix == -1 {
			break
		}
		if err := l.line(l.buf[off : off+ix+1]); err != nil {
			return len(b), err
		}
		off += ix + 1
	}
	if off != 0 {
		n := copy(l.buf, l.buf[off:])
		l.buf = l.buf[:n]
	}
	return len(b), nil
}

// flush processes any buffered bytes left without a trailing newline, e.g. a
// final marker emitted just before the VM powers off. Call once after qemu exits.
func (l *logParser) flush() error {
	if len(l.buf) == 0 {
		return nil
	}
	line := l.buf
	l.buf = nil
	return l.line(line)
}

// line dispatches one output line to fn, splitting off the optional
// "[TESTS:KEY] " prefix as key (nil for ordinary output).
func (l *logParser) line(line []byte) error {
	if !bytes.HasPrefix(line, []byte("[TESTS:")) {
		return l.fn(nil, line)
	}
	ix := bytes.IndexByte(line, ']')
	if ix == -1 {
		return l.fn(nil, line)
	}
	return l.fn(line[7:ix], bytes.TrimPrefix(line[ix+1:], []byte(" ")))
}
