package crawler

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/eshaan2418/trawl/synthweb"
	"github.com/eshaan2418/trawl/warc"
)

// synthClient routes every *.synth.test host to the in-process test server.
func synthClient(srv *httptest.Server) *http.Client {
	addr := srv.Listener.Addr().String()
	tr := DefaultTransport(16)
	tr.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	return &http.Client{Transport: tr}
}

func seeds(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = "http://" + synthweb.HostName(i) + "/p/0"
	}
	return out
}

func TestCrawlIsPoliteAndCompliant(t *testing.T) {
	sw := synthweb.New(synthweb.Config{Hosts: 10, Pages: 12, CrossProb: 0.5})
	srv := httptest.NewServer(sw)
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "crawl.warc.gz")
	w, err := warc.Create(path, map[string]string{"software": "trawl-test"})
	if err != nil {
		t.Fatal(err)
	}
	const delay = 25 * time.Millisecond
	c := New(Config{
		Client: synthClient(srv), Concurrency: 8, DefaultDelay: delay, MaxPages: 10_000,
		MaxDepth: -1, WARC: w, SameHostOnly: false,
	})
	st, err := c.Run(context.Background(), seeds(10))
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	rep := sw.Report()
	t.Logf("stats %+v report %+v", st, rep)

	if rep.Violations != 0 {
		t.Errorf("%d requests to robots-disallowed paths", rep.Violations)
	}
	if rep.NofollowFetches != 0 {
		t.Errorf("%d rel=nofollow links fetched", rep.NofollowFetches)
	}
	if rep.DuplicatePages != 0 {
		t.Errorf("%d duplicate page fetches", rep.DuplicatePages)
	}
	if rep.MinGapMs < float64(delay/time.Millisecond) {
		t.Errorf("min gap %.1f ms < delay %v", rep.MinGapMs, delay)
	}
	if g := rep.MinGapMsByClass["crawl-delay-0.2s"]; g < 200 {
		t.Errorf("Crawl-delay: 0.2 not honoured: min gap %.1f ms", g)
	}
	if st.Errors != 0 {
		t.Errorf("%d fetch errors", st.Errors)
	}
	// 503 robots.txt hosts (i % 5 == 4) must get no page requests at all: Violations covers it.
	// Robots: exactly one fetch per contacted host.
	if int(st.RobotsFetches) != rep.Hosts || rep.RobotsRequests != rep.Hosts {
		t.Errorf("robots fetches %d / requests %d for %d hosts", st.RobotsFetches, rep.RobotsRequests, rep.Hosts)
	}
	if st.Pages != int64(rep.PageRequests) {
		t.Errorf("crawler counted %d pages, server saw %d", st.Pages, rep.PageRequests)
	}
	if st.Status[http.StatusMovedPermanently] == 0 {
		t.Error("expected redirects from hosts with i % 7 == 0")
	}

	// Every page fetch is archived as a response + request pair that parses and verifies.
	f, _ := os.Open(path)
	defer func() { _ = f.Close() }()
	r, err := warc.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	responses := 0
	for {
		rec, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if rec.Type() == "response" {
			responses++
			if !strings.HasPrefix(string(rec.Block), "HTTP/1.1 ") {
				t.Fatalf("response block does not start with a status line: %.40q", rec.Block)
			}
		}
	}
	if int64(responses) != st.Pages {
		t.Errorf("%d response records for %d pages", responses, st.Pages)
	}
}

func TestMaxPagesIsExact(t *testing.T) {
	srv := httptest.NewServer(synthweb.New(synthweb.Config{Hosts: 20, Pages: 50, CrossProb: 0.9}))
	defer srv.Close()
	c := New(Config{Client: synthClient(srv), Concurrency: 16, DefaultDelay: time.Millisecond, MaxPages: 37, MaxDepth: -1})
	st, err := c.Run(context.Background(), seeds(20))
	if err != nil {
		t.Fatal(err)
	}
	if st.Pages != 37 {
		t.Fatalf("fetched %d pages, want exactly 37", st.Pages)
	}
}

func TestDepthAndSameHostScope(t *testing.T) {
	sw := synthweb.New(synthweb.Config{Hosts: 5, Pages: 100, CrossProb: 1})
	srv := httptest.NewServer(sw)
	defer srv.Close()
	c := New(Config{Client: synthClient(srv), Concurrency: 4, DefaultDelay: time.Millisecond, MaxDepth: 1, SameHostOnly: true})
	if _, err := c.Run(context.Background(), []string{"http://" + synthweb.HostName(0) + "/p/0"}); err != nil {
		t.Fatal(err)
	}
	rep := sw.Report()
	if rep.Hosts != 1 {
		t.Errorf("same-host crawl contacted %d hosts", rep.Hosts)
	}
	// Depth 1 from /p/0: the seed plus at most its 4 same-host links (one may redirect).
	if rep.PageRequests > 6 {
		t.Errorf("depth limit ignored: %d page requests", rep.PageRequests)
	}
}

func TestGracefulCancellation(t *testing.T) {
	srv := httptest.NewServer(synthweb.New(synthweb.Config{Hosts: 30, Pages: 200, CrossProb: 0.5, Latency: 5 * time.Millisecond}))
	defer srv.Close()
	path := filepath.Join(t.TempDir(), "c.warc.gz")
	w, _ := warc.Create(path, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	c := New(Config{Client: synthClient(srv), Concurrency: 16, DefaultDelay: 10 * time.Millisecond, MaxDepth: -1, MaxPages: 1 << 30, WARC: w})
	start := time.Now()
	st, err := c.Run(ctx, seeds(30))
	if err != nil {
		t.Fatal(err)
	}
	if el := time.Since(start); el > 2*time.Second {
		t.Fatalf("cancellation took %v", el)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	f, _ := os.Open(path)
	defer func() { _ = f.Close() }()
	r, _ := warc.NewReader(f)
	n := 0
	for {
		_, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("archive corrupt after cancellation: %v", err)
		}
		n++
	}
	if want := 1 + 2*int(st.Pages); n != want {
		t.Fatalf("%d records, want %d (warcinfo + 2 per page)", n, want)
	}
}

func TestRobotsStatusHandling(t *testing.T) {
	mux := http.NewServeMux()
	statuses := map[string]int{"a.test": 404, "b.test": 503, "c.test": 429, "d.test": 401}
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		host := strings.Split(r.Host, ":")[0]
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(statuses[host])
			return
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html>ok</html>")
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := New(Config{Client: synthClient(srv), Concurrency: 4, DefaultDelay: time.Millisecond})
	st, err := c.Run(context.Background(), []string{"http://a.test/", "http://b.test/", "http://c.test/", "http://d.test/"})
	if err != nil {
		t.Fatal(err)
	}
	// RFC 9309: 4xx (including 401) -> allow; 5xx -> disallow. 429 is treated like 5xx.
	if st.Pages != 2 || st.RobotsBlocked != 2 {
		t.Fatalf("pages %d blocked %d, want 2 and 2", st.Pages, st.RobotsBlocked)
	}
}

func TestExtractLinks(t *testing.T) {
	page, _ := url.Parse("http://h.test/dir/page.html")
	body := `<html><head><base href="/base/"></head><body>
<a href="x">x</a><a href="x#frag">dup</a><a href="/abs">abs</a><a rel="nofollow ugc" href="/nf">nf</a>
<a href="mailto:a@b">m</a><a href="http://Other.test:80/o">o</a><area href="map"></body></html>`
	links, nf := ExtractLinks(page, []byte(body))
	var got []string
	for _, l := range links {
		got = append(got, l.String())
	}
	want := "http://h.test/base/x http://h.test/abs http://other.test/o http://h.test/base/map"
	if nf || strings.Join(got, " ") != want {
		t.Fatalf("got %v nofollow=%v", got, nf)
	}
	if _, nf := ExtractLinks(page, []byte(`<meta name="ROBOTS" content="noindex, nofollow"><a href="/x">`)); !nf {
		t.Fatal("meta robots nofollow ignored")
	}
}
