#!/usr/bin/env bash
# Crawl a 20-host synthetic web politely and print the crawler's and the server's view.
set -euo pipefail
bin/synthweb -addr 127.0.0.1:18180 -control 127.0.0.1:18181 -hosts 20 -pages 40 -latency 2ms 2>/dev/null &
SW=$!; trap 'kill $SW 2>/dev/null; rm -f demo.warc.gz' EXIT; sleep 0.5
SEEDS=$(for i in $(seq 0 19); do printf 'http://site%d.synth.test/p/0 ' "$i"; done)
echo "$ trawl -concurrency 16 -delay 100ms -max-pages 400 -warc demo.warc.gz <20 seeds>"
bin/trawl -resolve '*.synth.test=127.0.0.1:18180' -concurrency 16 -delay 100ms -max-pages 400 -max-depth -1 \
  -warc demo.warc.gz $SEEDS | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps({k: d[k] for k in ("pages","pages_per_second","robots_fetches","robots_blocked","status","warc_records","errors")}, indent=1))'
echo "server-side audit:"
curl -s http://127.0.0.1:18181/report | python3 -c 'import json,sys; d=json.load(sys.stdin); print(json.dumps({k: d[k] for k in ("page_requests","robots_violations","nofollow_fetches","duplicate_page_fetches","min_gap_ms","min_gap_ms_by_class")}, indent=1))'
