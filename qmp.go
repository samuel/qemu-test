package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"
)

// qmpConn is a minimal QMP (QEMU Machine Protocol) client over a unix socket. It
// completes the capabilities handshake on connect and forwards raw command lines
// (the guest builds the JSON); a background goroutine logs responses and async
// events so a failed command is diagnosable.
type qmpConn struct {
	conn net.Conn
	mu   sync.Mutex // guards w so concurrent send() callers can't interleave a command
	w    *bufio.Writer
}

// dialQMP connects to the QMP unix socket at path (retrying until it appears or
// the deadline passes), reads the greeting, negotiates capabilities, and starts
// the response-logging loop.
func dialQMP(path string, timeout time.Duration) (*qmpConn, error) {
	deadline := time.Now().Add(timeout)
	var conn net.Conn
	for {
		var err error
		conn, err = net.Dial("unix", path)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("dial qmp socket %s: %w", path, err)
		}
		time.Sleep(20 * time.Millisecond)
	}

	q := &qmpConn{conn: conn, w: bufio.NewWriter(conn)}
	r := bufio.NewReader(conn)
	// The server sends a greeting line ({"QMP": {...}}) on connect.
	if _, err := r.ReadBytes('\n'); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read qmp greeting: %w", err)
	}
	// Leave capabilities negotiation / enter command mode.
	if err := q.send(`{"execute":"qmp_capabilities"}`); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("qmp_capabilities: %w", err)
	}
	if _, err := r.ReadBytes('\n'); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read qmp capabilities reply: %w", err)
	}
	go q.readLoop(r)
	return q, nil
}

// send writes one QMP command line (a complete JSON object). It is safe for
// concurrent use; the mutex keeps two callers from interleaving bytes on the
// shared writer.
func (q *qmpConn) send(line string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	if _, err := q.w.WriteString(line); err != nil {
		return err
	}
	if err := q.w.WriteByte('\n'); err != nil {
		return err
	}
	return q.w.Flush()
}

// readLoop logs QMP responses and async events until the connection closes.
func (q *qmpConn) readLoop(r *bufio.Reader) {
	for {
		line, err := r.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) != 0 {
			// A QMP command failure is a top-level {"error": {...}} object. Parse
			// for that member instead of substring-matching "error", which would
			// misclassify an event or return payload that merely contains the word.
			var msg struct {
				Error json.RawMessage `json:"error"`
			}
			if err := json.Unmarshal(t, &msg); err == nil && len(msg.Error) != 0 {
				slog.Error("qmp", "msg", string(t))
			} else {
				slog.Debug("qmp", "msg", string(t))
			}
		}
		if err != nil {
			return
		}
	}
}

// Close closes the underlying connection (ending readLoop).
func (q *qmpConn) Close() error {
	return q.conn.Close()
}
