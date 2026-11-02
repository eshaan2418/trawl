// Package warc writes WARC 1.1 files (ISO 28500:2017): a warcinfo record, then a request and
// a response record per fetch. Each record is its own gzip member, so files can be appended
// to, split, and indexed by byte offset (the convention used by web archives and CDX indexes).
package warc

import (
	"bufio"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // WARC payload digests are conventionally SHA-1
	"encoding/base32"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// Writer appends records to a WARC file. It is safe for concurrent use.
type Writer struct {
	mu     sync.Mutex
	f      *os.File
	bw     *bufio.Writer
	offset int64
	count  int
}

// Create opens path for writing and emits a warcinfo record describing the crawl.
func Create(path string, info map[string]string) (*Writer, error) {
	f, err := os.Create(path)
	if err != nil {
		return nil, err
	}
	w := &Writer{f: f, bw: bufio.NewWriterSize(f, 1<<20)}
	keys := make([]string, 0, len(info))
	for k := range info {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var body strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&body, "%s: %s\r\n", k, info[k])
	}
	_, err = w.write("warcinfo", "", "", "application/warc-fields", []byte(body.String()), nil)
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return w, nil
}

// RecordID returns a new "<urn:uuid:...>" identifier (random, version 4).
func RecordID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err) // crypto/rand never fails on supported platforms
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("<urn:uuid:%x-%x-%x-%x-%x>", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// PayloadDigest is the WARC-Payload-Digest convention: "sha1:" + base32(SHA-1(payload)).
func PayloadDigest(payload []byte) string {
	sum := sha1.Sum(payload) //nolint:gosec // see import comment
	return "sha1:" + base32.StdEncoding.EncodeToString(sum[:])
}

// Exchange is one HTTP fetch: the raw request and response bytes as sent and received
// (headers plus body), and the entity body for the payload digest.
type Exchange struct {
	TargetURI string
	IP        string
	Date      time.Time
	Request   []byte
	Response  []byte
	Payload   []byte
	Truncated bool // body cut at the size limit: adds "WARC-Truncated: length"
}

// WriteExchange writes a response record and a request record that refers to it
// (WARC-Concurrent-To). Returns the response record's byte offset in the file.
func (w *Writer) WriteExchange(ex Exchange) (int64, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	date := ex.Date.UTC().Format(time.RFC3339)
	respID := RecordID()
	extra := []string{
		"WARC-Record-ID: " + respID,
		"WARC-Payload-Digest: " + PayloadDigest(ex.Payload),
	}
	if ex.IP != "" {
		extra = append(extra, "WARC-IP-Address: "+ex.IP)
	}
	if ex.Truncated {
		extra = append(extra, "WARC-Truncated: length")
	}
	off, err := w.write("response", ex.TargetURI, date, "application/http; msgtype=response", ex.Response, extra)
	if err != nil {
		return 0, err
	}
	_, err = w.write("request", ex.TargetURI, date, "application/http; msgtype=request", ex.Request,
		[]string{"WARC-Record-ID: " + RecordID(), "WARC-Concurrent-To: " + respID})
	return off, err
}

// write emits one gzip member containing one record. Caller holds mu (or is Create).
func (w *Writer) write(typ, target, date, ctype string, block []byte, extra []string) (int64, error) {
	if date == "" {
		date = time.Now().UTC().Format(time.RFC3339)
	}
	hasID := false
	for _, e := range extra {
		if strings.HasPrefix(e, "WARC-Record-ID:") {
			hasID = true
		}
	}
	var hdr strings.Builder
	hdr.WriteString("WARC/1.1\r\n")
	hdr.WriteString("WARC-Type: " + typ + "\r\n")
	if !hasID {
		hdr.WriteString("WARC-Record-ID: " + RecordID() + "\r\n")
	}
	hdr.WriteString("WARC-Date: " + date + "\r\n")
	if target != "" {
		hdr.WriteString("WARC-Target-URI: " + target + "\r\n")
	}
	for _, e := range extra {
		hdr.WriteString(e + "\r\n")
	}
	hdr.WriteString("WARC-Block-Digest: " + PayloadDigest(block) + "\r\n")
	hdr.WriteString("Content-Type: " + ctype + "\r\n")
	fmt.Fprintf(&hdr, "Content-Length: %d\r\n\r\n", len(block))

	start := w.offset
	cw := &countingWriter{w: w.bw}
	gz, _ := gzip.NewWriterLevel(cw, gzip.BestSpeed)
	if _, err := io.WriteString(gz, hdr.String()); err != nil {
		return 0, err
	}
	if _, err := gz.Write(block); err != nil {
		return 0, err
	}
	if _, err := io.WriteString(gz, "\r\n\r\n"); err != nil {
		return 0, err
	}
	if err := gz.Close(); err != nil {
		return 0, err
	}
	w.offset += cw.n
	w.count++
	return start, nil
}

// Records returns the number of records written so far (including warcinfo).
func (w *Writer) Records() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.count
}

// Close flushes and closes the file.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.bw.Flush(); err != nil {
		_ = w.f.Close()
		return err
	}
	return w.f.Close()
}

type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}
