package cpio

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// trailerName is the standard newc end-of-archive marker. Writer.Close emits one
// and ReadRecord stops at it.
const trailerName = "TRAILER!!!"

// NewReader returns a Reader decoding newc cpio records from r, the inverse of
// Writer.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: r}
}

// Reader decodes newc cpio records, padding on the same absolute-stream-position
// scheme the Writer uses.
type Reader struct {
	r   io.Reader
	pos int64
}

// read fills b and advances pos. A record is in flight, so an EOF before b is
// full is reported as io.ErrUnexpectedEOF.
func (r *Reader) read(b []byte) error {
	n, err := io.ReadFull(r.r, b)
	r.pos += int64(n)
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// pad discards bytes up to the next 4-byte boundary, mirroring Writer.pad.
func (r *Reader) pad() error {
	o := round4(r.pos)
	if o == r.pos {
		return nil
	}
	n, err := io.CopyN(io.Discard, r.r, o-r.pos)
	r.pos += n
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// ReadRecord reads the next record. It returns io.EOF at the end of the archive
// (a clean record boundary, or a TRAILER!!! record).
func (r *Reader) ReadRecord() (Record, error) {
	h, err := r.readHeader()
	if err != nil {
		return Record{}, err
	}
	name, err := r.readName(h.NameLength)
	if err != nil {
		return Record{}, err
	}
	if name == trailerName {
		return Record{}, io.EOF
	}
	content, err := r.readContent(h.FileSize)
	if err != nil {
		return Record{}, err
	}
	info := infoFromHeader(h)
	info.Name = name
	return Record{Reader: bytes.NewReader(content), Info: info}, nil
}

// readHeader reads a record's magic and fixed-size header. Unlike the other
// reads it tolerates a clean EOF, reporting an exhausted archive as io.EOF.
func (r *Reader) readHeader() (header, error) {
	buf := make([]byte, len(magic))
	n, err := io.ReadFull(r.r, buf)
	r.pos += int64(n)
	if err != nil {
		if errors.Is(err, io.EOF) && n == 0 {
			return header{}, io.EOF // clean end of archive
		}
		return header{}, fmt.Errorf("cpio: read magic: %w", err)
	}
	if string(buf) != magic {
		return header{}, fmt.Errorf("cpio: bad magic %q", buf)
	}

	// The Writer encodes the header as upper-case hex; hex.Decode accepts those digits.
	size := binary.Size(header{})
	hexBuf := make([]byte, hex.EncodedLen(size))
	if err := r.read(hexBuf); err != nil {
		return header{}, fmt.Errorf("cpio: read header: %w", err)
	}
	raw := make([]byte, size)
	if _, err := hex.Decode(raw, hexBuf); err != nil {
		return header{}, fmt.Errorf("cpio: decode header: %w", err)
	}
	var h header
	if err := binary.Read(bytes.NewReader(raw), binary.BigEndian, &h); err != nil {
		return header{}, fmt.Errorf("cpio: parse header: %w", err)
	}
	return h, nil
}

// readName reads the NUL-terminated record name (nameLen counts the NUL) and
// discards the header+name alignment padding.
func (r *Reader) readName(nameLen uint32) (string, error) {
	if nameLen == 0 {
		return "", errors.New("cpio: zero-length name")
	}
	name := make([]byte, nameLen)
	if err := r.read(name); err != nil {
		return "", fmt.Errorf("cpio: read name: %w", err)
	}
	if err := r.pad(); err != nil {
		return "", fmt.Errorf("cpio: skip name padding: %w", err)
	}
	return string(name[:nameLen-1]), nil
}

// readContent reads size bytes of file data and discards the trailing alignment
// padding, which the Writer emits only when size > 0.
func (r *Reader) readContent(size uint32) ([]byte, error) {
	if size == 0 {
		return nil, nil
	}
	content := make([]byte, size)
	if err := r.read(content); err != nil {
		return nil, fmt.Errorf("cpio: read content: %w", err)
	}
	if err := r.pad(); err != nil {
		return nil, fmt.Errorf("cpio: skip content padding: %w", err)
	}
	return content, nil
}

// ReadAll reads every record from src until the end of the archive.
func ReadAll(src io.Reader) ([]Record, error) {
	r := NewReader(src)
	var recs []Record
	for {
		rec, err := r.ReadRecord()
		if errors.Is(err, io.EOF) {
			return recs, nil
		}
		if err != nil {
			return recs, err
		}
		recs = append(recs, rec)
	}
}

// infoFromHeader is the inverse of headerFromInfo, dropping NameLength (consumed
// while reading the name) and CRC (always 0). The caller fills in Name.
func infoFromHeader(h header) Info {
	return Info{
		Ino:      h.Ino,
		Mode:     h.Mode,
		UID:      h.UID,
		GID:      h.GID,
		NLink:    h.NLink,
		MTime:    h.MTime,
		FileSize: h.FileSize,
		Major:    h.Major,
		Minor:    h.Minor,
		Rmajor:   h.Rmajor,
		Rminor:   h.Rminor,
	}
}
