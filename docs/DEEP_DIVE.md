# trawl deep dive

### 1. What does "polite" mean precisely, and how is it enforced?

Two guarantees per host, enforced by the frontier rather than by sleeps in workers:

1. **No concurrent requests to a host.** A host leaves the ready heap when `Next` hands out
   one of its URLs, and returns only on `Done`.
2. **A minimum gap.** `Done(host, delay)` sets `readyAt = now + delay`, so the next request
   starts at least `delay` after the previous one *finished*. The delay is the configured
   default, raised by robots.txt `Crawl-delay` (capped at 30 s).

The benchmark checks this from the server side. The synthetic web timestamps every request,
and trawl's smallest observed gap on any host is never below the configured delay
(`bench/results/crawl-summary.md`). `TestPolitenessAndExactlyOnce` checks it against the
frontier directly, with 8 workers and 20 hosts.

### 2. Why a per-host queue plus a heap of hosts, instead of one priority queue of URLs?

A single URL priority queue keyed by "earliest allowed time" has to re-key every queued URL of
a host whenever that host is fetched: O(k log n) per fetch for k URLs on the host. With per-host
FIFOs, only the host's single heap entry changes, so every operation is O(log hosts)
regardless of queue depth. Hosts with nothing queued, or a fetch in flight, are off the heap
entirely, so 10,000 idle hosts cost nothing per `Next`.

### 3. Why does fetching robots.txt consume the host's politeness slot?

Because it is a request to the host. Many crawlers fetch robots.txt and then the first page
immediately: two requests back to back, and a server that throttles on the first sees the
second as a violation. trawl puts the page back at the front of the host's queue
(`PushFront`) and calls `Done(host, delay)`, so the page waits one full delay after the
robots.txt request. The cost is one delay of latency per new host, which is small in a long
crawl.

### 4. Why is a 503 on robots.txt "crawl nothing", but a 404 "crawl everything"?

RFC 9309 §2.3.1. A 4xx means the file is *unavailable*: the site has no rules, so everything
is allowed, and that includes 401 and 403. A 5xx or a network failure means it is
*unreachable*: the rules exist but can't be read, and crawling anyway could violate them, so
the crawler must assume complete disallow. trawl retries such hosts after 10 minutes. It
treats 429 like 5xx: a server that says "too many requests" should not be read as "no rules".
In the benchmark, every robots violation colly made was a page fetched from a host whose
robots.txt returned 503: the server attributes each violation to its cause.

### 5. Why longest-match, and why does allow win ties?

Order-based matching ("first rule wins") makes files fragile: reordering two lines changes
behaviour. Longest match makes the most specific rule win regardless of order, so
`Disallow: /private` plus `Allow: /private/press` does the intuitive thing. On a tie (equal
pattern length), allow wins because that is the least restrictive reading of an ambiguous
file; the RFC specifies it. Tested in `TestLongestMatchAndTieGoesToAllow`.

### 6. How is wildcard matching kept linear, and how do you know it's right?

`match` is the classic two-pointer glob matcher. On a mismatch after a `*`, it backtracks
only to the most recent `*`, advancing the text position by one, so it runs in O(pattern ×
path) in the worst case and in linear time on realistic inputs. It needs no recursion, no
regexp compilation per rule, and no allocation. Correctness comes from a *differential* fuzz
test: `FuzzMatchAgainstRegexp` translates the same pattern into an anchored regular expression
and checks the two agree on random ASCII patterns and paths.

### 7. Why normalise URLs, and why not sort query parameters?

Without normalisation the same page is crawled several times (`HTTP://Example.com:80/a/./b`,
`http://example.com/a/b`, `http://example.com/a/b#top`). That wastes the site's capacity and
yours. trawl lower-cases the scheme and host, removes default ports and trailing dots, drops
fragments, resolves dot segments, and normalises percent-escapes. It does *not* sort query
parameters: on real sites `?a=1&b=2` and `?b=2&a=1` can differ (signed URLs, order-dependent
routing), and an archive must not merge them. The idempotence fuzzer found four bugs (DESIGN
§URL normalisation). Each one would have produced duplicate fetches.

### 8. Why aren't redirects followed inline?

An inline follow (Go's default) fetches the target immediately, within the *source* host's
slot. If the target is on another host, that skips its robots check and its politeness gap;
if it was already crawled, it is a duplicate. trawl archives the 3xx and queues `Location` as
a new URL at the same depth, so the target goes through normalisation, de-duplication, robots,
and scheduling like any other link.

The benchmark's duplicate fetches show why normalisation and de-duplication need to sit in
one place. colly fetched 222–254 pages twice per run. The diagnostic run
(`bench/colly_diag.sh`) shows the duplicates disappear when the synthetic web has no
cross-host links. Those links are the only ones that carry a `#frag` suffix, so colly treats
`/p/7#frag` and `/p/7` as different URLs. trawl drops fragments during normalisation and fetched
no page twice.

### 9. How does shutdown avoid a corrupt WARC?

SIGINT or SIGTERM cancels the crawl context. `frontier.Next` checks cancellation first, so
workers take no new URLs. In-flight fetches run on `context.WithoutCancel` with the
per-request timeout, so they complete and are written as a whole response + request pair
before the worker exits. `main` then closes the WARC, which flushes it. `TestGracefulCancellation`
cancels mid-crawl and checks the archive parses completely, with exactly `1 + 2 × pages`
records. That test also caught a real bug: `Next` used to hand out already-due work after
cancellation, and the crawl ran to completion.

### 10. Why gzip each WARC record separately?

Standard practice for web archives. A multi-member gzip file is still a valid gzip stream
(`zcat` reads it whole), but each record starts at a member boundary, so an index (CDX) can
store a byte offset and a reader can seek straight to one record and decompress only it.
Compressing the whole file as one stream would make random access impossible.

### 11. What is in the response record, and what is not exactly "as transferred"?

The status line, headers, and body as received, with transparent gzip disabled so bodies are
the bytes the server sent. One exception: Go's HTTP client removes chunked transfer encoding
before user code sees the body. Writing the original `Transfer-Encoding: chunked` header in
front of an unchunked body would produce a record that WARC readers mis-parse. trawl drops
that header and writes the real `Content-Length`, so the record is self-consistent. CI runs
`warcio check` on a crawl's output. A crawler that needs byte-exact capture would wrap the
connection to record the raw stream.

### 12. How does the crawler bound memory?

- The frontier holds at most `MaxQueued` URLs; `Push` refuses beyond that, and the drop is
  counted in a metric.
- Bodies are read through `io.LimitReader(MaxBodyBytes + 1)` and truncated, with the WARC
  record marked `WARC-Truncated: length`.
- robots.txt is read up to 500 KiB.

The remaining unbounded structure is the `seen` set (one string per discovered URL). A crawl
of tens of millions of URLs would replace it with a Bloom filter or an on-disk set.

### 13. How does trawl compare with colly, and is the comparison fair?

colly is configured as close to trawl as it allows: one `LimitRule` per host with
`Parallelism: 1` and the same delay, robots.txt obeyed, and the same global concurrency and
page cap. A first version used a single `DomainGlob: "*"` rule. That caps parallelism across
*all* hosts combined and made colly serial (about 360 pages/s), which was unfair to it, so it
was replaced.

On the same synthetic web (`bench/results/crawl-summary.md`) trawl is faster: about 10,000 vs
2,700 pages/s with no delay, and 503 vs 188 pages/s with a 50 ms delay. It is also compliant
where colly is not:
- colly fetched pages from hosts whose robots.txt returned 503;
- with a 50 ms delay, colly's smallest gap between two page requests to one host was 2.7 ms,
  even excluding robots.txt requests (`bench/results/colly-diagnosis.jsonl`);
- colly ignores `Crawl-delay`;
- colly re-fetched fragment-only URL variants.

colly also fetched more pages than the cap, because it checks the cap when a request starts,
while about 32 requests are in flight. So compare its pages-per-second figures, not its totals.
Caveat: this is one synthetic workload on loopback with 2 ms server latency; it measures
scheduling overhead and policy, not network behaviour.

### 14. What's missing for production?

- A persistent frontier and seen set, so a crawl survives restarts.
- DNS caching and IP-level politeness: many hosts on one IP should share a budget.
- Adaptive delays (back off on 429/503 and on rising latency).
- `Retry-After`, sitemap ingestion, conditional re-crawls (`If-Modified-Since`).
- JavaScript rendering, which is out of scope by design.
