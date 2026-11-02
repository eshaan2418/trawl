// Command trawl crawls the web politely and archives what it fetches to WARC.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof" //nolint:gosec // served only on the explicit -metrics address
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/eshaan2418/trawl/crawler"
	"github.com/eshaan2418/trawl/warc"
)

type resolveFlag map[string]string

func (r resolveFlag) String() string { return fmt.Sprint(map[string]string(r)) }
func (r resolveFlag) Set(v string) error {
	host, addr, ok := strings.Cut(v, "=")
	if !ok {
		return errors.New("want host=addr (host may be *.suffix)")
	}
	r[host] = addr
	return nil
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "trawl:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		maxPages    = flag.Int("max-pages", 1000, "stop after this many page fetches")
		maxDepth    = flag.Int("max-depth", 3, "maximum link depth from the seeds (-1 for unlimited)")
		concurrency = flag.Int("concurrency", 16, "simultaneous fetches (at most one per host)")
		delay       = flag.Duration("delay", time.Second, "minimum gap between fetches to one host")
		maxDelay    = flag.Duration("max-delay", 30*time.Second, "cap on robots.txt Crawl-delay")
		sameHost    = flag.Bool("same-host", false, "only follow links to the seeds' hosts")
		warcPath    = flag.String("warc", "", "write fetched pages to this .warc.gz file")
		ua          = flag.String("user-agent", "", "User-Agent header (default trawl/0.1 (+repo URL))")
		metrics     = flag.String("metrics", "", "serve Prometheus /metrics and /debug/pprof on this address")
		timeout     = flag.Duration("timeout", 20*time.Second, "per-request timeout")
		verbose     = flag.Bool("v", false, "log every fetch error")
		resolve     = resolveFlag{}
	)
	flag.Var(resolve, "resolve", "connect host (or *.suffix) to addr instead of DNS, e.g. '*.synth.test=127.0.0.1:8080' (repeatable)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: trawl [flags] seed-url...\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		return errors.New("no seed URLs")
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := crawler.NewMetrics(reg)
	if *metrics != "" {
		http.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
		srv := &http.Server{Addr: *metrics, ReadHeaderTimeout: 5 * time.Second}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics server", "err", err)
			}
		}()
		defer func() { _ = srv.Close() }()
	}

	cfg := crawler.Config{
		UserAgent: *ua, Concurrency: *concurrency, DefaultDelay: *delay, MaxDelay: *maxDelay,
		MaxPages: *maxPages, MaxDepth: *maxDepth, SameHostOnly: *sameHost, Timeout: *timeout,
		Metrics: m, Logger: log,
	}
	if len(resolve) > 0 {
		tr := crawler.DefaultTransport(*concurrency)
		dialer := &net.Dialer{Timeout: 10 * time.Second}
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, _, _ := net.SplitHostPort(addr)
			for pat, to := range resolve {
				if pat == host || (strings.HasPrefix(pat, "*.") && strings.HasSuffix(host, pat[1:])) {
					return dialer.DialContext(ctx, network, to)
				}
			}
			return dialer.DialContext(ctx, network, addr)
		}
		cfg.Client = &http.Client{Transport: tr}
	}
	var w *warc.Writer
	if *warcPath != "" {
		var err error
		w, err = warc.Create(*warcPath, map[string]string{
			"software": "trawl/0.1", "format": "WARC File Format 1.1",
			"robots": "obey", "isPartOf": strings.Join(flag.Args(), " "),
		})
		if err != nil {
			return err
		}
		cfg.WARC = w
	}

	st, err := crawler.New(cfg).Run(ctx, flag.Args())
	if w != nil {
		if cerr := w.Close(); cerr != nil && err == nil {
			err = cerr
		}
	}
	if err != nil {
		return err
	}
	out := map[string]any{
		"pages": st.Pages, "errors": st.Errors, "robots_fetches": st.RobotsFetches,
		"robots_blocked": st.RobotsBlocked, "bytes": st.Bytes, "status": st.Status,
		"seconds": st.Duration.Seconds(), "pages_per_second": float64(st.Pages) / st.Duration.Seconds(),
		"interrupted": ctx.Err() != nil,
	}
	if w != nil {
		out["warc_records"] = w.Records()
	}
	return json.NewEncoder(os.Stdout).Encode(out)
}
