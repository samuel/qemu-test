// Copyright 2013-2017 the u-root Authors. All rights reserved
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package cpio implements a reader and writer for newc cpio files to be used
// with initramfs. It is not intended to be for general use.
package cpio

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

const (
	magic = "070701"
)

// errWriterClosed is returned by WriteRecord after the archive has been
// finalized by Close.
var errWriterClosed = errors.New("cpio: write to closed Writer")

// Record represents a CPIO record, which represents a Unix file.
type Record struct {
	io.Reader
	Info
}

// Info holds metadata about files.
type Info struct {
	Ino      uint32
	Mode     uint32
	UID      uint32
	GID      uint32
	NLink    uint32
	MTime    uint32
	FileSize uint32
	Major    uint32
	Minor    uint32
	Rmajor   uint32
	Rminor   uint32
	Name     string
}

type header struct {
	Ino        uint32
	Mode       uint32
	UID        uint32
	GID        uint32
	NLink      uint32
	MTime      uint32
	FileSize   uint32
	Major      uint32
	Minor      uint32
	Rmajor     uint32
	Rminor     uint32
	NameLength uint32
	CRC        uint32
}

func headerFromInfo(i Info) header {
	return header{
		Ino:        i.Ino,
		Mode:       i.Mode,
		UID:        i.UID,
		GID:        i.GID,
		NLink:      i.NLink,
		MTime:      i.MTime,
		FileSize:   i.FileSize,
		Major:      i.Major,
		Minor:      i.Minor,
		Rmajor:     i.Rmajor,
		Rminor:     i.Rminor,
		NameLength: uint32(len(i.Name)) + 1,
	}
}

func NewWriter(w io.Writer) *Writer {
	return &Writer{w: w}
}

type Writer struct {
	w      io.Writer
	pos    int64
	closed bool
}

func (w *Writer) Write(b []byte) (int, error) {
	n, err := w.w.Write(b)
	if err != nil {
		return n, err
	}
	w.pos += int64(n)
	return n, nil
}

func (w *Writer) pad() error {
	if o := round4(w.pos); o != w.pos {
		var pad [3]byte
		if _, err := w.Write(pad[:o-w.pos]); err != nil {
			return err
		}
	}
	return nil
}

// WriteRecord writes newc cpio records. It pads the header+name write to 4
// byte alignment and pads the data write as well.
func (w *Writer) WriteRecord(f Record) error {
	if w.closed {
		return errWriterClosed
	}
	if _, err := w.Write([]byte(magic)); err != nil {
		return err
	}

	buf := &bytes.Buffer{}
	hdr := headerFromInfo(f.Info)
	if f.Reader == nil {
		hdr.FileSize = 0
	}
	if err := binary.Write(buf, binary.BigEndian, hdr); err != nil {
		return err
	}

	hexBuf := make([]byte, hex.EncodedLen(buf.Len()))
	n := hex.Encode(hexBuf, buf.Bytes())
	// It's much easier to debug if we match GNU output format.
	hexBuf = bytes.ToUpper(hexBuf)

	// Write header.
	if _, err := w.Write(hexBuf[:n]); err != nil {
		return err
	}

	// Append NULL char.
	cstr := append([]byte(f.Name), 0)
	if _, err := w.Write(cstr); err != nil {
		return err
	}

	// Pad to a multiple of 4.
	if err := w.pad(); err != nil {
		return err
	}

	// Some files do not have any content.
	if f.Reader == nil {
		return nil
	}

	// Write file contents.
	m, err := io.Copy(w, f)
	if err != nil {
		return err
	}
	if m != int64(f.FileSize) {
		return fmt.Errorf("cpio.WriteRecord: %s: wrote %d bytes of file instead of %d bytes; archive is now corrupt",
			f.Name, m, f.FileSize)
	}
	return w.pad()
}

// Close finalizes the archive by writing the TRAILER!!! end-of-archive record,
// which the Reader stops at. It is idempotent and does not close the underlying
// io.Writer. After Close, WriteRecord returns an error. The returned error must
// be checked: writing the trailer can fail like any other write.
func (w *Writer) Close() error {
	if w.closed {
		return nil
	}
	// Write the trailer before marking closed so WriteRecord's guard permits it.
	err := w.WriteRecord(Record{Info: Info{NLink: 1, Name: trailerName}})
	w.closed = true
	return err
}
