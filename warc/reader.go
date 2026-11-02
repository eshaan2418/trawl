package warc

import (
	"bufio"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"net/textproto"
	"strconv"
	"strings"
)

// Record is one parsed WARC record.
type Record struct {
	Header textproto.MIMEHeader
	Block  []byte
}

// Type returns the WARC-Type.
func (r *Record) Type() string { return r.Header.Get("WARC-Type") }

// Reader iterates over the records of a (gzip, multi-member) WARC file.
type Reader struct {
	br *bufio.Reader
}

// NewReader reads gzip-compressed WARC data.
func NewReader(r io.Reader) (*Reader, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, err
	}
	gz.Multistream(true)
	return &Reader{br: bufio.NewReader(gz)}, nil
}

// Next returns the next record, or io.EOF. It verifies WARC-Block-Digest when present.
func (r *Reader) Next() (*Record, error) {
	line, err := r.br.ReadString('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && line == "" {
			return nil, io.EOF
		}
		return nil, err
	}
	if strings.TrimRight(line, "\r\n") != "WARC/1.1" {
		return nil, fmt.Errorf("warc: bad version line %q", line)
	}
	hdr, err := textproto.NewReader(r.br).ReadMIMEHeader()
	if err != nil {
		return nil, err
	}
	n, err := strconv.Atoi(hdr.Get("Content-Length"))
	if err != nil || n < 0 {
		return nil, fmt.Errorf("warc: bad Content-Length %q", hdr.Get("Content-Length"))
	}
	block := make([]byte, n)
	if _, err := io.ReadFull(r.br, block); err != nil {
		return nil, err
	}
	var tail [4]byte
	if _, err := io.ReadFull(r.br, tail[:]); err != nil || string(tail[:]) != "\r\n\r\n" {
		return nil, errors.New("warc: record not terminated by CRLF CRLF")
	}
	if d := hdr.Get("WARC-Block-Digest"); d != "" && d != PayloadDigest(block) {
		return nil, fmt.Errorf("warc: block digest mismatch for %s", hdr.Get("WARC-Record-ID"))
	}
	return &Record{Header: hdr, Block: block}, nil
}
