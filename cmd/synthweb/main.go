// Command synthweb serves the synthetic web from package synthweb for crawler benchmarks.
// Point a crawler at it with a resolver override, e.g. trawl -resolve '*.synth.test=127.0.0.1:8080'.
package main

import (
	"flag"
	"log"
	"net/http"
	"time"

	"github.com/eshaan2418/trawl/synthweb"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "address for the synthetic hosts")
	control := flag.String("control", "127.0.0.1:8081", "address for GET /report and POST /reset")
	hosts := flag.Int("hosts", 50, "number of hosts")
	pages := flag.Int("pages", 200, "pages per host")
	latency := flag.Duration("latency", 0, "added latency per response")
	cross := flag.Float64("cross", 0.3, "probability a page links to another host")
	pad := flag.Int("pad", 2000, "filler bytes per page")
	flag.Parse()
	s := synthweb.New(synthweb.Config{Hosts: *hosts, Pages: *pages, CrossProb: *cross, Latency: *latency, BodyPad: *pad})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /report", s.ReportHandler)
	mux.HandleFunc("POST /reset", func(w http.ResponseWriter, _ *http.Request) { s.Reset(); w.WriteHeader(http.StatusNoContent) })
	go func() {
		srv := &http.Server{Addr: *control, Handler: mux, ReadHeaderTimeout: 5 * time.Second}
		log.Fatal(srv.ListenAndServe())
	}()
	srv := &http.Server{Addr: *addr, Handler: s, ReadHeaderTimeout: 5 * time.Second}
	log.Printf("synthweb: %d hosts x %d pages on %s, report on %s", *hosts, *pages, *addr, *control)
	log.Fatal(srv.ListenAndServe())
}
