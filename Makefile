J ?= $(shell n=$$(sysctl -n hw.ncpu 2>/dev/null || nproc); echo $$(( n / 2 > 2 ? n / 2 : 2 )))
export GOMAXPROCS ?= $(J)

.PHONY: build test fuzz lint bench demo warc-check clean

build:
	go build -o bin/trawl ./cmd/trawl
	go build -o bin/synthweb ./cmd/synthweb

test:
	go test -race ./...

# Time-boxed fuzzing (each target 30 s by default).
FUZZTIME ?= 30s
fuzz:
	go test ./robots -run '^$$' -fuzz FuzzMatchAgainstRegexp -fuzztime $(FUZZTIME)
	go test ./robots -run '^$$' -fuzz '^FuzzParse$$' -fuzztime $(FUZZTIME)
	go test ./urlnorm -run '^$$' -fuzz FuzzIdempotent -fuzztime $(FUZZTIME)

lint:
	gofmt -l . | (! grep .)
	go vet ./...
	golangci-lint run ./...

bench:
	bench/run.sh bench/results/crawl.jsonl 5
	python3 bench/summarize.py bench/results/crawl.jsonl > bench/results/crawl-summary.md

demo: build
	@bench/demo.sh

# Crawl the synthetic web and validate the archive with warcio (the Python reference library).
warc-check: build
	@bench/warc_check.sh

clean:
	rm -rf bin *.warc.gz
