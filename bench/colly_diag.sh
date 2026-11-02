#!/usr/bin/env bash
# Diagnose colly's duplicates and politeness gaps (bench/results/colly-diagnosis.jsonl):
# run both crawlers with and without cross-host links (the only links carrying #fragments)
# and record the page-to-page gap, which excludes robots.txt requests.
set -euo pipefail
OUT=${1:-bench/results/colly-diagnosis.jsonl}
BIN=$(mktemp -d); trap 'rm -rf "$BIN"' EXIT
go build -o "$BIN/sw" ./cmd/synthweb; go build -o "$BIN/trawl" ./cmd/trawl; (cd bench/colly && go build -o "$BIN/colly" .)
SEEDS=$(for i in $(seq 0 49); do printf 'http://site%d.synth.test/p/0 ' "$i"; done)
: > "$OUT"
for cross in 0.3 0; do
  "$BIN/sw" -addr 127.0.0.1:18380 -control 127.0.0.1:18381 -hosts 50 -pages 200 -latency 2ms -cross "$cross" 2>/dev/null &
  SW=$!; sleep 0.7
  for tool in colly trawl; do
    curl -s -X POST http://127.0.0.1:18381/reset
    if [[ $tool == colly ]]; then
      "$BIN/colly" -resolve '*.synth.test=127.0.0.1:18380' -max-pages 3000 -concurrency 32 -delay 50ms $SEEDS > "$BIN/o.json"
    else
      "$BIN/trawl" -resolve '*.synth.test=127.0.0.1:18380' -max-pages 3000 -max-depth -1 -concurrency 32 -delay 50ms $SEEDS > "$BIN/o.json"
    fi
    printf '{"tool":"%s","cross":%s,"delay":"50ms","crawl":%s,"server":%s}\n' "$tool" "$cross" "$(cat "$BIN/o.json")" \
      "$(curl -s http://127.0.0.1:18381/report)" >> "$OUT"
  done
  kill $SW; wait $SW 2>/dev/null || true
done
