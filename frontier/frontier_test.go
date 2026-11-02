package frontier

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

// Many workers, many hosts: no host is ever fetched concurrently, the gap between the end
// of one fetch and the start of the next on a host is at least the delay, and every URL is
// delivered exactly once.
func TestPolitenessAndExactlyOnce(t *testing.T) {
	const hosts, perHost, workers = 20, 15, 8
	const delay = 3 * time.Millisecond
	f := New(1 << 20)
	for h := 0; h < hosts; h++ {
		for i := 0; i < perHost; i++ {
			f.Push(Item{URL: fmt.Sprintf("u%d-%d", h, i), Host: fmt.Sprintf("h%d", h)})
		}
	}
	var mu sync.Mutex
	busy := map[string]bool{}
	lastEnd := map[string]time.Time{}
	got := map[string]int{}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				it, ok := f.Next(context.Background())
				if !ok {
					return
				}
				mu.Lock()
				if busy[it.Host] {
					t.Errorf("host %s fetched concurrently", it.Host)
				}
				if e, ok := lastEnd[it.Host]; ok && time.Since(e) < delay {
					t.Errorf("host %s: gap %v < %v", it.Host, time.Since(e), delay)
				}
				busy[it.Host] = true
				got[it.URL]++
				mu.Unlock()
				time.Sleep(200 * time.Microsecond)
				mu.Lock()
				busy[it.Host] = false
				lastEnd[it.Host] = time.Now()
				mu.Unlock()
				f.Done(it.Host, delay)
			}
		}()
	}
	wg.Wait()
	if len(got) != hosts*perHost {
		t.Fatalf("delivered %d distinct URLs, want %d", len(got), hosts*perHost)
	}
	for u, n := range got {
		if n != 1 {
			t.Errorf("%s delivered %d times", u, n)
		}
	}
}

func TestBackpressure(t *testing.T) {
	f := New(2)
	if !f.Push(Item{URL: "a", Host: "h"}) || !f.Push(Item{URL: "b", Host: "h"}) {
		t.Fatal("first two pushes should fit")
	}
	if f.Push(Item{URL: "c", Host: "h"}) {
		t.Fatal("third push should be rejected")
	}
}

func TestNextReturnsWhenDrainedOrCancelled(t *testing.T) {
	f := New(10)
	if _, ok := f.Next(context.Background()); ok {
		t.Fatal("empty frontier must report drained")
	}
	f.Push(Item{URL: "a", Host: "h"})
	it, _ := f.Next(context.Background())
	// One item in flight, none queued: Next must block until Done or cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, ok := f.Next(ctx); ok || time.Since(start) < 25*time.Millisecond {
		t.Fatal("Next should block while a fetch is in flight, then honour cancellation")
	}
	f.Done(it.Host, 0)
	if _, ok := f.Next(context.Background()); ok {
		t.Fatal("drained after Done")
	}
}

func TestPushFrontKeepsOrderAndClose(t *testing.T) {
	f := New(10)
	f.Push(Item{URL: "a", Host: "h"})
	f.Push(Item{URL: "b", Host: "h"})
	it, _ := f.Next(context.Background())
	f.PushFront(it)
	f.Done("h", 0)
	if it2, _ := f.Next(context.Background()); it2.URL != "a" {
		t.Fatalf("PushFront lost its place: got %s", it2.URL)
	}
	f.Close()
	if _, ok := f.Next(context.Background()); ok {
		t.Fatal("closed frontier")
	}
	f.Done("h", 0) // must not panic after Close
	f.Push(Item{URL: "z", Host: "h"})
}

func TestCancelledContextStopsEvenWhenWorkIsDue(t *testing.T) {
	f := New(10)
	f.Push(Item{URL: "a", Host: "h"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, ok := f.Next(ctx); ok {
		t.Fatal("Next handed out work after cancellation")
	}
}
