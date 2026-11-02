#!/usr/bin/env bash
# trawl vs colly on the synthetic web (cmd/synthweb). For each mode and repetition, both
# crawlers run against a freshly reset server; the server's own report (robots violations,
# duplicate fetches, smallest per-host gap) is recorded next to each crawler's output.
# Usage: bench/run.sh <out.jsonl> [reps]
set -euo pipefail
OUT=${1:?out}; REPS=${2:-5}
HOSTS=${HOSTS:-50}; PAGES=${PAGES:-200}; LATENCY=${LATENCY:-2ms}; MAXPAGES=${MAXPAGES:-3000}; CONC=${CONC:-32}
BIN=$(mktemp -d)
go build -o "$BIN/trawl" ./cmd/trawl
go build -o "$BIN/synthweb" ./cmd/synthweb
(cd bench/colly && go build -o "$BIN/colly" .)
"$BIN/synthweb" -addr 127.0.0.1:18080 -control 127.0.0.1:18081 -hosts "$HOSTS" -pages "$PAGES" -latency "$LATENCY" 2>/dev/null &
SW=$!
trap 'kill $SW 2>/dev/null; rm -rf "$BIN"' EXIT
sleep 1
SEEDS=$(for i in $(seq 0 $((HOSTS - 1))); do printf 'http://site%d.synth.test/p/0 ' "$i"; done)
load1() { sysctl -n vm.loadavg 2>/dev/null | awk '{print $2}' || cut -d' ' -f1 /proc/loadavg; }
rss() { awk '/maximum resident set size/ {print $1}' "$1"; }
: > "$OUT"
for mode in fast polite; do
  delay=0s; [[ $mode == polite ]] && delay=50ms
  for rep in $(seq 1 "$REPS"); do
    for tool in trawl colly; do
      curl -s -X POST http://127.0.0.1:18081/reset
      l=$(load1)
      if [[ $tool == trawl ]]; then
        /usr/bin/time -l "$BIN/trawl" -resolve '*.synth.test=127.0.0.1:18080' -max-pages "$MAXPAGES" -max-depth -1 \
          -concurrency "$CONC" -delay "$delay" $SEEDS > "$BIN/out.json" 2> "$BIN/time.txt"
      else
        /usr/bin/time -l "$BIN/colly" -resolve '*.synth.test=127.0.0.1:18080' -max-pages "$MAXPAGES" \
          -concurrency "$CONC" -delay "$delay" $SEEDS > "$BIN/out.json" 2> "$BIN/time.txt"
      fi
      report=$(curl -s http://127.0.0.1:18081/report)
      printf '{"tool":"%s","mode":"%s","delay":"%s","rep":%d,"load1":%s,"peak_rss_bytes":%s,"crawl":%s,"server":%s}\n' \
        "$tool" "$mode" "$delay" "$rep" "$l" "$(rss "$BIN/time.txt")" "$(cat "$BIN/out.json")" "$report" >> "$OUT"
    done
  done
done
echo "wrote $(wc -l < "$OUT") results to $OUT"
