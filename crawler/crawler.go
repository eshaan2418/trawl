// Package crawler fetches pages politely, archives them to WARC, and follows links.
package crawler

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/eshaan2418/trawl/frontier"
	"github.com/eshaan2418/trawl/robots"
	"github.com/eshaan2418/trawl/urlnorm"
	"github.com/eshaan2418/trawl/warc"
)

// Config controls a crawl. Zero values get the defaults noted below.
type Config struct {
	UserAgent    string        // default "trawl/0.1 (+https://github.com/eshaan2418/trawl)"
	RobotsToken  string        // product token matched against robots.txt groups; default "trawl"
	Concurrency  int           // simultaneous fetches across hosts; default 16
	DefaultDelay time.Duration // gap between fetches to one host; default 1s
	MaxDelay     time.Duration // cap on robots.txt Crawl-delay; default 30s
	MaxPages     int           // stop after this many page fetches; default 1000
	MaxDepth     int           // link depth from seeds; default 3 (negative: unlimited)
	MaxBodyBytes int64         // bodies are truncated beyond this; default 10 MiB
	MaxQueued    int           // frontier capacity (backpressure); default 1,000,000
	Timeout      time.Duration // per request; default 20s
	RobotsTTL    time.Duration // how long a fetched robots.txt is trusted; default 24h
	SameHostOnly bool          // only follow links to the seeds' hosts
	WARC         *warc.Writer  // optional archive output
	Client       *http.Client  // optional; tests inject one with a custom dialer
	Metrics      *Metrics      // optional
	Logger       *slog.Logger  // optional
}

// Stats summarises a finished crawl.
type Stats struct {
	Pages         int64 // page fetches that produced an HTTP response
	Errors        int64 // network errors and timeouts
	RobotsFetches int64
	RobotsBlocked int64 // URLs skipped because robots.txt disallows them
	Bytes         int64
	Status        map[int]int64
	Duration      time.Duration
}

// Crawler runs one crawl. Create with New; Run may be called once.
type Crawler struct {
	cfg      Config
	client   *http.Client
	front    *frontier.Frontier
	seenMu   sync.Mutex
	seen     map[string]struct{}
	scope    map[string]bool
	robotsMu sync.Mutex
	robots   map[string]*robotsEntry
	pages    atomic.Int64
	stats    Stats
	statMu   sync.Mutex
	log      *slog.Logger
}

type robotsEntry struct {
	rules   *robots.Rules
	expires time.Time
	delay   time.Duration
}

// New applies defaults and builds a crawler.
func New(cfg Config) *Crawler {
	if cfg.UserAgent == "" {
		cfg.UserAgent = "trawl/0.1 (+https://github.com/eshaan2418/trawl)"
	}
	if cfg.RobotsToken == "" {
		cfg.RobotsToken = "trawl"
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 16
	}
	if cfg.DefaultDelay == 0 {
		cfg.DefaultDelay = time.Second
	}
	if cfg.MaxDelay == 0 {
		cfg.MaxDelay = 30 * time.Second
	}
	if cfg.MaxPages == 0 {
		cfg.MaxPages = 1000
	}
	if cfg.MaxDepth == 0 {
		cfg.MaxDepth = 3
	}
	if cfg.MaxBodyBytes == 0 {
		cfg.MaxBodyBytes = 10 << 20
	}
	if cfg.MaxQueued == 0 {
		cfg.MaxQueued = 1_000_000
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.RobotsTTL == 0 {
		cfg.RobotsTTL = 24 * time.Hour
	}
	client := cfg.Client
	if client == nil {
		client = &http.Client{Transport: DefaultTransport(cfg.Concurrency)}
	}
	// Redirects are queued as new URLs instead of being followed inline, so the target gets
	// its own robots check, de-duplication, and politeness slot.
	c := *client
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	log := cfg.Logger
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Crawler{
		cfg:    cfg,
		client: &c,
		front:  frontier.New(cfg.MaxQueued),
		seen:   map[string]struct{}{},
		scope:  map[string]bool{},
		robots: map[string]*robotsEntry{},
		stats:  Stats{Status: map[int]int64{}},
		log:    log,
	}
}

// DefaultTransport disables transparent gzip so WARC records hold the bytes as transferred.
func DefaultTransport(concurrency int) *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DisableCompression = true
	t.MaxIdleConnsPerHost = 2
	t.MaxIdleConns = concurrency * 2
	t.ResponseHeaderTimeout = 20 * time.Second
	return t
}

// Run crawls from seeds until MaxPages, an exhausted frontier, or ctx cancellation. On
// cancellation it stops taking new URLs and waits for in-flight fetches to finish (each is
// bounded by Timeout), so the WARC file is never left with a half-written exchange.
func (c *Crawler) Run(ctx context.Context, seeds []string) (Stats, error) {
	start := time.Now()
	for _, s := range seeds {
		u, err := urlnorm.Normalize(nil, s)
		if err != nil {
			return Stats{}, fmt.Errorf("seed %q: %w", s, err)
		}
		c.scope[u.Host] = true
		c.enqueue(u, 0)
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	for i := 0; i < c.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				it, ok := c.front.Next(ctx)
				if !ok {
					return
				}
				delay := c.process(ctx, it)
				c.front.Done(it.Host, delay)
				if c.pages.Load() >= int64(c.cfg.MaxPages) {
					cancel()
				}
				c.cfg.Metrics.observeFrontier(c.front.Stats())
			}
		}()
	}
	wg.Wait()
	c.statMu.Lock()
	defer c.statMu.Unlock()
	st := c.stats
	st.Status = make(map[int]int64, len(c.stats.Status))
	for k, v := range c.stats.Status {
		st.Status[k] = v
	}
	st.Duration = time.Since(start)
	return st, nil
}

// enqueue de-duplicates and pushes a URL. Returns false if it was not queued.
func (c *Crawler) enqueue(u *url.URL, depth int) bool {
	if c.cfg.MaxDepth >= 0 && depth > c.cfg.MaxDepth {
		return false
	}
	if c.cfg.SameHostOnly && !c.scope[u.Host] {
		return false
	}
	key := urlnorm.Key(u)
	c.seenMu.Lock()
	if _, dup := c.seen[key]; dup {
		c.seenMu.Unlock()
		return false
	}
	c.seen[key] = struct{}{}
	c.seenMu.Unlock()
	if !c.front.Push(frontier.Item{URL: key, Host: hostKey(u), Depth: depth}) {
		c.cfg.Metrics.incDropped()
		return false
	}
	return true
}

// hostKey groups politeness by scheme + host: http://a and https://a are separate origins
// for robots.txt, but most sites serve both from the same machine, so we key on host only.
func hostKey(u *url.URL) string { return u.Host }

// process handles one frontier item and returns the delay before the host's next fetch.
func (c *Crawler) process(ctx context.Context, it frontier.Item) time.Duration {
	u, err := url.Parse(it.URL)
	if err != nil {
		return 0
	}
	entry, fetched := c.robotsFor(ctx, u)
	if fetched {
		// The robots.txt fetch used this host's slot. Put the page back at the front of the
		// host's queue and wait a politeness delay before fetching it.
		c.front.PushFront(it)
		return entry.delay
	}
	if !entry.rules.AllowedURL(u) {
		c.addStat(func(s *Stats) { s.RobotsBlocked++ })
		c.cfg.Metrics.incRobotsBlocked()
		return 0 // nothing was fetched, so the host's next URL may go immediately
	}
	if c.pages.Add(1) > int64(c.cfg.MaxPages) {
		return 0
	}
	c.fetchPage(ctx, u, it.Depth)
	return entry.delay
}

func (c *Crawler) addStat(f func(*Stats)) {
	c.statMu.Lock()
	f(&c.stats)
	c.statMu.Unlock()
}

// robotsFor returns the cached rules for u's host, fetching robots.txt if needed. The bool is
// true when this call performed the fetch.
func (c *Crawler) robotsFor(ctx context.Context, u *url.URL) (*robotsEntry, bool) {
	key := u.Scheme + "://" + u.Host
	c.robotsMu.Lock()
	e := c.robots[key]
	c.robotsMu.Unlock()
	if e != nil && time.Now().Before(e.expires) {
		return e, false
	}
	rules, ttl := c.fetchRobots(ctx, u)
	delay := c.cfg.DefaultDelay
	if d, ok := rules.CrawlDelay(); ok {
		delay = max(delay, min(d, c.cfg.MaxDelay))
	}
	e = &robotsEntry{rules: rules, expires: time.Now().Add(ttl), delay: delay}
	c.robotsMu.Lock()
	c.robots[key] = e
	c.robotsMu.Unlock()
	return e, true
}

// fetchRobots applies RFC 9309 section 2.3.1: 2xx -> parse; 4xx -> allow all; 5xx, 429, or
// network failure -> disallow all, retried sooner. Redirects are followed (up to 5).
func (c *Crawler) fetchRobots(ctx context.Context, u *url.URL) (*robots.Rules, time.Duration) {
	c.addStat(func(s *Stats) { s.RobotsFetches++ })
	target := &url.URL{Scheme: u.Scheme, Host: u.Host, Path: "/robots.txt"}
	cl := *c.client
	cl.CheckRedirect = func(_ *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		return nil
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.Timeout)
	defer cancel()
	req, _ := http.NewRequestWithContext(rctx, http.MethodGet, target.String(), nil)
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	resp, err := cl.Do(req)
	if err != nil {
		c.log.Debug("robots fetch failed", "url", target, "err", err)
		return robots.DisallowAll, 10 * time.Minute
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, robots.MaxSize))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return robots.Parse(body, c.cfg.RobotsToken), c.cfg.RobotsTTL
	case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
		return robots.DisallowAll, 10 * time.Minute
	default:
		return robots.AllowAll, c.cfg.RobotsTTL
	}
}

func (c *Crawler) fetchPage(ctx context.Context, u *url.URL, depth int) {
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), c.cfg.Timeout)
	defer cancel()
	var remoteIP string
	trace := &httptrace.ClientTrace{GotConn: func(info httptrace.GotConnInfo) {
		if a, ok := info.Conn.RemoteAddr().(*net.TCPAddr); ok {
			remoteIP = a.IP.String()
		}
	}}
	req, _ := http.NewRequestWithContext(httptrace.WithClientTrace(rctx, trace), http.MethodGet, u.String(), nil)
	req.Header.Set("User-Agent", c.cfg.UserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml;q=0.9,*/*;q=0.5")
	started := time.Now()
	resp, err := c.client.Do(req)
	if err != nil {
		c.addStat(func(s *Stats) { s.Errors++ })
		c.cfg.Metrics.incErrors()
		c.log.Debug("fetch failed", "url", u, "err", err)
		return
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, c.cfg.MaxBodyBytes+1))
	truncated := int64(len(body)) > c.cfg.MaxBodyBytes
	if truncated {
		body = body[:c.cfg.MaxBodyBytes]
	}
	if err != nil {
		c.addStat(func(s *Stats) { s.Errors++ })
		c.cfg.Metrics.incErrors()
		return
	}
	c.addStat(func(s *Stats) {
		s.Pages++
		s.Bytes += int64(len(body))
		s.Status[resp.StatusCode]++
	})
	c.cfg.Metrics.observeFetch(resp.StatusCode, len(body), time.Since(started))

	if c.cfg.WARC != nil {
		if err := c.archive(req, resp, body, remoteIP, truncated, started); err != nil {
			c.log.Error("warc write failed", "err", err)
		}
	}

	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		if loc := resp.Header.Get("Location"); loc != "" {
			if t, err := urlnorm.Normalize(u, loc); err == nil {
				c.enqueue(t, depth) // a redirect does not deepen the crawl
			}
		}
		return
	}
	if resp.StatusCode != http.StatusOK || !isHTML(resp.Header.Get("Content-Type")) {
		return
	}
	if headerNofollow(resp.Header.Values("X-Robots-Tag")) {
		return
	}
	links, nofollow := ExtractLinks(u, body)
	if nofollow {
		return
	}
	for _, l := range links {
		c.enqueue(l, depth+1)
	}
}

func (c *Crawler) archive(req *http.Request, resp *http.Response, body []byte, ip string, truncated bool, at time.Time) error {
	rawReq, err := httputil.DumpRequestOut(req.Clone(context.Background()), false)
	if err != nil {
		return err
	}
	// Go decodes chunked transfer encoding before we see the body, so the stored headers
	// describe the stored body: drop Transfer-Encoding and state the real length.
	h := resp.Header.Clone()
	h.Del("Transfer-Encoding")
	h.Set("Content-Length", strconv.Itoa(len(body)))
	var raw bytes.Buffer
	fmt.Fprintf(&raw, "HTTP/%d.%d %s\r\n", resp.ProtoMajor, resp.ProtoMinor, resp.Status)
	if err := h.Write(&raw); err != nil {
		return err
	}
	raw.WriteString("\r\n")
	raw.Write(body)
	_, err = c.cfg.WARC.WriteExchange(warc.Exchange{
		TargetURI: req.URL.String(),
		IP:        ip,
		Date:      at,
		Request:   rawReq,
		Response:  raw.Bytes(),
		Payload:   body,
		Truncated: truncated,
	})
	return err
}

func isHTML(ct string) bool {
	ct = strings.ToLower(ct)
	return strings.HasPrefix(ct, "text/html") || strings.HasPrefix(ct, "application/xhtml+xml")
}

func headerNofollow(values []string) bool {
	for _, v := range values {
		for _, d := range strings.Split(strings.ToLower(v), ",") {
			d = strings.TrimSpace(d)
			if d == "nofollow" || d == "none" {
				return true
			}
		}
	}
	return false
}
