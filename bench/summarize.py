"""Summarise bench/run.sh: medians per tool and mode, plus the server's compliance audit."""

import json
import statistics
import sys
from collections import defaultdict

rows = [json.loads(line) for line in open(sys.argv[1])]
g = defaultdict(list)
for r in rows:
    g[(r["mode"], r["tool"])].append(r)
print("| mode | tool | pages/s (median) | range | pages | peak RSS MB | robots violations (max) | duplicate fetches (max) | nofollow fetches (max) | min per-host gap ms (min over runs) | load avg |")
print("|---|---|---:|---|---:|---:|---:|---:|---:|---:|---:|")
for (mode, tool), rs in sorted(g.items()):
    pps = [r["crawl"]["pages_per_second"] for r in rs]
    med = lambda f: statistics.median(f(r) for r in rs)  # noqa: E731
    print(
        f"| {mode} ({rs[0]['delay']}) | {tool} | {statistics.median(pps):,.0f} | {min(pps):,.0f}–{max(pps):,.0f} | "
        f"{med(lambda r: r['crawl']['pages']):.0f} | {med(lambda r: r['peak_rss_bytes'] / 2**20):.0f} | "
        f"{max(r['server']['robots_violations'] for r in rs)} | {max(r['server']['duplicate_page_fetches'] for r in rs)} | "
        f"{max(r['server']['nofollow_fetches'] for r in rs)} | {min(r['server']['min_gap_ms'] for r in rs):.1f} | "
        f"{med(lambda r: r['load1']):.1f} |"
    )
