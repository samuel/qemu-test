// Package qmp is a minimal QMP (QEMU Machine Protocol) client, used to drive the
// running guest's qemu from the host.
package qmp

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

// Conn is a QMP client over a unix socket. It completes the capabilities
// handshake on connect and forwards raw command lines built by the caller; a
// background goroutine logs responses and async events.
type Conn struct {
	conn net.Conn
	mu   sync.Mutex // keeps concurrent Send callers from interleaving a command
	w    *bufio.Writer
}

// Dial connects to the QMP socket at path, retrying until it appears or the
// deadline passes, then negotiates capabilities and starts the logging loop.
func Dial(path string, timeout time.Duration) (*Conn, error) {
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

	q := &Conn{conn: conn, w: bufio.NewWriter(conn)}
	r := bufio.NewReader(conn)
	// Bound the handshake reads by the same deadline as the dial: a peer that
	// accepts the connection but never speaks must not block the run past its
	// budget. Cleared once command mode is entered.
	if err := conn.SetReadDeadline(deadline); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("set read deadline: %w", err)
	}
	// The server sends a greeting line on connect.
	if _, err := r.ReadBytes('\n'); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read qmp greeting: %w", err)
	}
	// Leave negotiation and enter command mode.
	if err := q.Send(`{"execute":"qmp_capabilities"}`); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("qmp_capabilities: %w", err)
	}
	if _, err := r.ReadBytes('\n'); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("read qmp capabilities reply: %w", err)
	}
	if err := conn.SetReadDeadline(time.Time{}); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("clear read deadline: %w", err)
	}
	go q.readLoop(r)
	return q, nil
}

// Send writes one QMP command line, a complete JSON object. Safe for concurrent
// use.
func (q *Conn) Send(line string) error {
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
func (q *Conn) readLoop(r *bufio.Reader) {
	for {
		line, err := r.ReadBytes('\n')
		if t := bytes.TrimSpace(line); len(t) != 0 {
			// A command failure is a top-level {"error": {...}} object; parse for
			// that member rather than substring-matching "error".
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
func (q *Conn) Close() error {
	return q.conn.Close()
}
