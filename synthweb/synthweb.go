// Package synthweb serves a deterministic synthetic web for testing and benchmarking crawlers.
//
// Hosts are named site<i>.synth.test. Each has pages /p/0 .. /p/<pages-1>; every page links to
// a few pages on the same host and, with some probability, to other hosts. Host behaviour
// varies by index so one crawl exercises the interesting cases:
//
//	i % 5 == 1  robots.txt disallows /p/1* (and allows /p/1/ok)
//	i % 5 == 2  robots.txt sets Crawl-delay: 0.2 (seconds)
//	i % 5 == 3  robots.txt returns 404 (allow everything)
//	i % 5 == 4  robots.txt returns 503 (crawl nothing)
//	i % 7 == 0  /p/3 redirects (301) to /p/4; one link per page is rel="nofollow" to /nofollow/<j>
//
// Every request is recorded with its arrival time, so the server can report politeness
// (smallest gap between requests to one host) and robots violations independently of the
// crawler under test.
package synthweb

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Config shapes the synthetic web.
type Config struct {
	Hosts     int
	Pages     int           // per host
	Links     int           // same-host links per page
	CrossProb float64       // probability that a page also links to another host
	Latency   time.Duration // added to every response
	BodyPad   int           // filler bytes per page
}

// Server is an http.Handler that routes on the Host header.
type Server struct {
	cfg  Config
	mu   sync.Mutex
	hits map[string][]hit
}

type hit struct {
	at   time.Time
	path string
}

// New builds a server.
func New(cfg Config) *Server {
	if cfg.Links == 0 {
		cfg.Links = 4
	}
	return &Server{cfg: cfg, hits: map[string][]hit{}}
}

// HostName returns the name of host i.
func HostName(i int) string { return fmt.Sprintf("site%d.synth.test", i) }

func hostIndex(host string) (int, bool) {
	host = strings.Split(host, ":")[0]
	if !strings.HasPrefix(host, "site") || !strings.HasSuffix(host, ".synth.test") {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(host, "site"), ".synth.test"))
	return n, err == nil
}

func rnd(parts ...int) uint64 {
	h := fnv.New64a()
	for _, p := range parts {
		_, _ = fmt.Fprintf(h, "%d/", p)
	}
	return h.Sum64()
}

// Disallowed reports whether host i's robots.txt forbids path for every agent.
func Disallowed(i int, path string) bool {
	switch i % 5 {
	case 1:
		return strings.HasPrefix(path, "/p/1") && !strings.HasPrefix(path, "/p/1/ok")
	case 4:
		return true // 503 robots.txt: a compliant crawler fetches nothing
	}
	return false
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	i, ok := hostIndex(r.Host)
	if !ok || i >= s.cfg.Hosts {
		http.NotFound(w, r)
		return
	}
	host := HostName(i)
	s.mu.Lock()
	s.hits[host] = append(s.hits[host], hit{at: time.Now(), path: r.URL.Path})
	s.mu.Unlock()
	if s.cfg.Latency > 0 {
		time.Sleep(s.cfg.Latency)
	}
	if r.URL.Path == "/robots.txt" {
		switch i % 5 {
		case 1:
			_, _ = fmt.Fprint(w, "User-agent: *\nDisallow: /p/1\nAllow: /p/1/ok\n")
		case 2:
			_, _ = fmt.Fprint(w, "User-agent: *\nCrawl-delay: 0.2\n")
		case 3:
			http.NotFound(w, r)
		case 4:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		default:
			_, _ = fmt.Fprint(w, "User-agent: *\nDisallow:\n")
		}
		return
	}
	var j int
	switch {
	case strings.HasPrefix(r.URL.Path, "/p/"):
		n, err := strconv.Atoi(strings.TrimPrefix(r.URL.Path, "/p/"))
		if err != nil || n < 0 || n >= s.cfg.Pages {
			http.NotFound(w, r)
			return
		}
		j = n
	case strings.HasPrefix(r.URL.Path, "/nofollow/"):
		_, _ = fmt.Fprint(w, "<html><body>should never be fetched</body></html>")
		return
	default:
		http.NotFound(w, r)
		return
	}
	if i%7 == 0 && j == 3 {
		http.Redirect(w, r, "/p/4", http.StatusMovedPermanently)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "<!doctype html><html><head><title>%s page %d</title></head><body><h1>page %d</h1>\n", host, j, j)
	for k := 0; k < s.cfg.Links; k++ {
		t := int(rnd(i, j, k) % uint64(s.cfg.Pages))
		_, _ = fmt.Fprintf(&b, "<a href=\"/p/%d\">p%d</a>\n", t, t)
	}
	if float64(rnd(i, j, -1)%1000)/1000 < s.cfg.CrossProb {
		oi := int(rnd(i, j, -2) % uint64(s.cfg.Hosts))
		oj := int(rnd(i, j, -3) % uint64(s.cfg.Pages))
		_, _ = fmt.Fprintf(&b, "<a href=\"http://%s/p/%d#frag\">x</a>\n", HostName(oi), oj)
	}
	if i%7 == 0 {
		_, _ = fmt.Fprintf(&b, "<a rel=\"nofollow\" href=\"/nofollow/%d\">nf</a>\n", j)
	}
	if s.cfg.BodyPad > 0 {
		b.WriteString("<p>" + strings.Repeat("lorem ipsum ", s.cfg.BodyPad/12) + "</p>")
	}
	b.WriteString("</body></html>\n")
	_, _ = w.Write([]byte(b.String()))
}

// Report summarises what the server observed.
type Report struct {
	Requests        int                      `json:"requests"`
	PageRequests    int                      `json:"page_requests"`
	RobotsRequests  int                      `json:"robots_requests"`
	Violations      int                      `json:"robots_violations"`
	ViolationsBy    map[string]int           `json:"robots_violations_by_reason"`
	NofollowFetches int                      `json:"nofollow_fetches"`
	DuplicatesBy    map[string]int           `json:"duplicate_fetches_by_reason"`
	DuplicatePages  int                      `json:"duplicate_page_fetches"`
	MinGap          map[string]time.Duration `json:"-"`
	MinPageGapMs    float64                  `json:"min_page_to_page_gap_ms"` // excludes robots.txt requests
	MinGapMs        float64                  `json:"min_gap_ms"`              // smallest gap on any host
	MinGapMsByClass map[string]float64       `json:"min_gap_ms_by_class"`
	Hosts           int                      `json:"hosts_contacted"`
}

// Report computes politeness and compliance from the recorded requests.
func (s *Server) Report() Report {
	s.mu.Lock()
	defer s.mu.Unlock()
	rep := Report{MinGap: map[string]time.Duration{}, MinGapMs: -1, MinPageGapMs: -1, MinGapMsByClass: map[string]float64{},
		ViolationsBy: map[string]int{}, DuplicatesBy: map[string]int{}}
	for host, hs := range s.hits {
		i, _ := hostIndex(host)
		rep.Hosts++
		sort.Slice(hs, func(a, b int) bool { return hs[a].at.Before(hs[b].at) })
		seen := map[string]bool{}
		var lastPage time.Time
		for k, h := range hs {
			if h.path != "/robots.txt" {
				if !lastPage.IsZero() {
					ms := float64(h.at.Sub(lastPage)) / float64(time.Millisecond)
					if rep.MinPageGapMs < 0 || ms < rep.MinPageGapMs {
						rep.MinPageGapMs = ms
					}
				}
				lastPage = h.at
			}
			rep.Requests++
			switch h.path {
			case "/robots.txt":
				rep.RobotsRequests++
			default:
				rep.PageRequests++
				if Disallowed(i, h.path) {
					rep.Violations++
					if i%5 == 4 {
						rep.ViolationsBy["page_on_host_with_503_robots"]++
					} else {
						rep.ViolationsBy["disallowed_path"]++
					}
				}
				if strings.HasPrefix(h.path, "/nofollow/") {
					rep.NofollowFetches++
				}
				if seen[h.path] {
					rep.DuplicatePages++
					if i%7 == 0 && h.path == "/p/4" {
						rep.DuplicatesBy["redirect_target_p4"]++
					} else {
						rep.DuplicatesBy["other"]++
					}
				}
				seen[h.path] = true
			}
			if k > 0 {
				gap := h.at.Sub(hs[k-1].at)
				if old, ok := rep.MinGap[host]; !ok || gap < old {
					rep.MinGap[host] = gap
				}
			}
		}
		if g, ok := rep.MinGap[host]; ok {
			ms := float64(g) / float64(time.Millisecond)
			if rep.MinGapMs < 0 || ms < rep.MinGapMs {
				rep.MinGapMs = ms
			}
			class := "default"
			if i%5 == 2 {
				class = "crawl-delay-0.2s"
			}
			if old, ok := rep.MinGapMsByClass[class]; !ok || ms < old {
				rep.MinGapMsByClass[class] = ms
			}
		}
	}
	return rep
}

// ReportHandler serves Report as JSON (mounted on a separate port by cmd/synthweb).
func (s *Server) ReportHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.Report())
}

// Reset forgets recorded requests.
func (s *Server) Reset() {
	s.mu.Lock()
	s.hits = map[string][]hit{}
	s.mu.Unlock()
}
