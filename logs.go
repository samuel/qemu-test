package main

import "bytes"

type logParser struct {
	fn  func(key, line []byte) error
	buf []byte
}

func (l *logParser) Write(b []byte) (int, error) {
	l.buf = append(l.buf, b...)
	// Drain every complete line currently buffered. A single Write often
	// delivers several newline-terminated lines at once; processing only the
	// first would strand the rest (potentially the final [TESTS:PASS]/[TESTS:FAIL]
	// marker) until a later write that may never come.
	//
	// Advance a read offset per line and compact the buffer once at the end,
	// rather than copy-shifting the remainder after every line (O(n²) for many
	// lines in one write).
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

// flush processes any buffered bytes left without a trailing newline (e.g. a
// final marker emitted just before the VM powers off). Call once after the
// qemu process exits; it is a no-op when the buffer is empty.
func (l *logParser) flush() error {
	if len(l.buf) == 0 {
		return nil
	}
	line := l.buf
	l.buf = nil
	return l.line(line)
}

// line dispatches a single output line to fn, splitting off the optional
// "[TESTS:KEY] " control prefix as key (nil for ordinary output).
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
