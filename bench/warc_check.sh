#!/usr/bin/env bash
# End-to-end: crawl the synthetic web, then validate every record with warcio.
set -euo pipefail
bin/synthweb -addr 127.0.0.1:18280 -control 127.0.0.1:18281 -hosts 10 -pages 30 2>/dev/null &
SW=$!; trap 'kill $SW 2>/dev/null' EXIT; sleep 0.5
SEEDS=$(for i in $(seq 0 9); do printf 'http://site%d.synth.test/p/0 ' "$i"; done)
bin/trawl -resolve '*.synth.test=127.0.0.1:18280' -delay 5ms -max-depth -1 -warc check.warc.gz $SEEDS > /dev/null
uvx --quiet --from warcio warcio check -v check.warc.gz | tail -3
uvx --quiet --from warcio warcio index check.warc.gz | wc -l | xargs echo "records indexed:"
rm -f check.warc.gz
