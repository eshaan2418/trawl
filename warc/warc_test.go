package warc

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestRoundTripConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.warc.gz")
	w, err := Create(path, map[string]string{"software": "test"})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := strings.Repeat("x", i*37)
			_, err := w.WriteExchange(Exchange{
				TargetURI: "http://h/" + strings.Repeat("p", i%5),
				Date:      time.Unix(1700000000, 0),
				Request:   []byte("GET / HTTP/1.1\r\nHost: h\r\n\r\n"),
				Response:  []byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n" + body),
				Payload:   []byte(body),
				Truncated: i == 7,
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	defer func() { _ = f.Close() }()
	r, err := NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	responses := map[string]bool{}
	truncated := 0
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		counts[rec.Type()]++
		switch rec.Type() {
		case "response":
			responses[rec.Header.Get("WARC-Record-ID")] = true
			if rec.Header.Get("WARC-Truncated") == "length" {
				truncated++
			}
		case "request":
			if !responses[rec.Header.Get("WARC-Concurrent-To")] {
				t.Error("request does not follow its response")
			}
		}
	}
	if counts["warcinfo"] != 1 || counts["response"] != 50 || counts["request"] != 50 || truncated != 1 {
		t.Fatalf("counts %v truncated %d", counts, truncated)
	}
}

func TestPayloadDigestKnownValue(t *testing.T) {
	// SHA-1("") = da39a3ee5e6b4b0d3255bfef95601890afd80709, base32 below.
	if got := PayloadDigest(nil); got != "sha1:3I42H3S6NNFQ2MSVX7XZKYAYSCX5QBYJ" {
		t.Fatal(got)
	}
}
