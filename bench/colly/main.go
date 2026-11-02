// Baseline: the same crawl with gocolly/colly v2, configured as close to trawl as colly allows
// (one request at a time per host, fixed per-host delay, depth limit, robots.txt obeyed).
// Prints one JSON line like `trawl`.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gocolly/colly/v2"
)

func main() {
	maxPages := flag.Int64("max-pages", 5000, "stop after this many responses")
	depth := flag.Int("max-depth", 0, "colly MaxDepth (0 = unlimited)")
	delay := flag.Duration("delay", 0, "per-host delay")
	par := flag.Int("concurrency", 32, "total parallel requests")
	resolve := flag.String("resolve", "", "*.suffix=addr")
	robots := flag.Bool("robots", true, "obey robots.txt")
	flag.Parse()

	c := colly.NewCollector(colly.Async(true), colly.MaxDepth(*depth))
	c.IgnoreRobotsTxt = !*robots
	c.AllowURLRevisit = false
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 2
	if pat, addr, ok := strings.Cut(*resolve, "="); ok {
		suffix := strings.TrimPrefix(pat, "*")
		tr.DialContext = func(ctx context.Context, network, a string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(a)
			if strings.HasSuffix(host, suffix) {
				a = addr
			}
			return (&net.Dialer{}).DialContext(ctx, network, a)
		}
	}
	c.WithTransport(tr)
	// One rule per seed host: a single DomainGlob "*" rule would cap parallelism across all
	// domains combined (making colly serial), not per host as trawl does.
	for _, s := range flag.Args() {
		if u, err := url.Parse(s); err == nil {
			_ = c.Limit(&colly.LimitRule{DomainGlob: u.Hostname(), Parallelism: 1, Delay: *delay})
		}
	}
	// Global cap like trawl's -concurrency.
	sem := make(chan struct{}, *par)
	var pages, errs atomic.Int64
	c.OnRequest(func(r *colly.Request) {
		if pages.Load() >= *maxPages {
			r.Abort()
			return
		}
		sem <- struct{}{}
	})
	c.OnResponse(func(*colly.Response) { <-sem; pages.Add(1) })
	c.OnError(func(r *colly.Response, _ error) {
		if r != nil && r.StatusCode == 0 {
			errs.Add(1)
		}
		<-sem
	})
	c.OnHTML("a[href]", func(e *colly.HTMLElement) {
		if strings.Contains(e.Attr("rel"), "nofollow") {
			return
		}
		_ = e.Request.Visit(e.Attr("href"))
	})
	start := time.Now()
	for _, s := range flag.Args() {
		_ = c.Visit(s)
	}
	c.Wait()
	el := time.Since(start).Seconds()
	_ = json.NewEncoder(os.Stdout).Encode(map[string]any{
		"pages": pages.Load(), "errors": errs.Load(), "seconds": el, "pages_per_second": float64(pages.Load()) / el,
	})
}
