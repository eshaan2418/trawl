// Package frontier schedules URLs so that each host is fetched at most once at a time and no
// sooner than its politeness delay after the previous fetch finished.
//
// Structure: a FIFO queue per host, plus a min-heap of hosts ordered by the time they may
// next be fetched. A host is on the heap only while it has queued URLs and no fetch in
// flight, so Next is O(log hosts) and an idle or busy host costs nothing per call. Total
// queued URLs are bounded (backpressure): Push reports false when the frontier is full.
package frontier

import (
	"container/heap"
	"context"
	"sync"
	"time"
)

// Item is a URL waiting to be fetched.
type Item struct {
	URL   string
	Host  string
	Depth int
}

type hostState struct {
	name     string
	queue    []Item
	head     int
	readyAt  time.Time
	inFlight bool
	index    int // position in the heap, -1 when absent
}

func (h *hostState) pending() int { return len(h.queue) - h.head }

type readyHeap []*hostState

func (r readyHeap) Len() int           { return len(r) }
func (r readyHeap) Less(i, j int) bool { return r[i].readyAt.Before(r[j].readyAt) }
func (r readyHeap) Swap(i, j int) {
	r[i], r[j] = r[j], r[i]
	r[i].index, r[j].index = i, j
}
func (r *readyHeap) Push(x any) {
	h := x.(*hostState)
	h.index = len(*r)
	*r = append(*r, h)
}
func (r *readyHeap) Pop() any {
	old := *r
	h := old[len(old)-1]
	old[len(old)-1] = nil
	h.index = -1
	*r = old[:len(old)-1]
	return h
}

// Frontier is safe for concurrent use.
type Frontier struct {
	mu       sync.Mutex
	hosts    map[string]*hostState
	ready    readyHeap
	queued   int
	inFlight int
	max      int
	closed   bool
	wake     chan struct{} // capacity 1: "something changed"
	now      func() time.Time
}

// New returns a frontier holding at most maxQueued URLs.
func New(maxQueued int) *Frontier {
	return &Frontier{hosts: map[string]*hostState{}, max: maxQueued, wake: make(chan struct{}, 1), now: time.Now}
}

func (f *Frontier) signal() {
	select {
	case f.wake <- struct{}{}:
	default:
	}
}

// Push enqueues an item. It returns false if the frontier is full or closed.
func (f *Frontier) Push(it Item) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed || f.queued >= f.max {
		return false
	}
	h := f.hosts[it.Host]
	if h == nil {
		h = &hostState{name: it.Host, index: -1, readyAt: f.now()}
		f.hosts[it.Host] = h
	}
	h.queue = append(h.queue, it)
	f.queued++
	if !h.inFlight && h.index < 0 {
		heap.Push(&f.ready, h)
	}
	f.signal()
	return true
}

// PushFront puts an item back at the head of its host's queue, ignoring the capacity limit.
// Used when a host's slot was spent on something else (fetching robots.txt) and the item
// must keep its place. The host must currently be in flight; Done reschedules it.
func (f *Frontier) PushFront(it Item) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	h := f.hosts[it.Host]
	if h == nil {
		return
	}
	if h.head > 0 {
		h.head--
		h.queue[h.head] = it
	} else {
		h.queue = append([]Item{it}, h.queue...)
	}
	f.queued++
	if !h.inFlight && h.index < 0 {
		heap.Push(&f.ready, h)
	}
}

// Next blocks until some host is due, then returns its oldest URL and marks the host busy.
// The caller must call Done for that host when the fetch completes. Next returns false when
// the context is cancelled, or when the frontier is closed or drained (nothing queued and
// nothing in flight that could add more).
func (f *Frontier) Next(ctx context.Context) (Item, bool) {
	for {
		if ctx.Err() != nil {
			return Item{}, false // cancellation wins even when a host is due right now
		}
		f.mu.Lock()
		if f.closed || (f.queued == 0 && f.inFlight == 0) {
			f.mu.Unlock()
			f.signal() // let other waiters observe the drained state too
			return Item{}, false
		}
		var wait time.Duration = -1
		if len(f.ready) > 0 {
			h := f.ready[0]
			if d := h.readyAt.Sub(f.now()); d <= 0 {
				heap.Pop(&f.ready)
				it := h.queue[h.head]
				h.queue[h.head] = Item{}
				h.head++
				if h.head == len(h.queue) {
					h.queue, h.head = h.queue[:0], 0
				}
				h.inFlight = true
				f.queued--
				f.inFlight++
				if len(f.ready) > 0 {
					f.signal() // the wake token is single-slot: pass it on if more work is due
				}
				f.mu.Unlock()
				return it, true
			} else {
				wait = d
			}
		}
		f.mu.Unlock()

		var timer <-chan time.Time
		if wait >= 0 {
			t := time.NewTimer(wait)
			timer = t.C
			select {
			case <-ctx.Done():
				t.Stop()
				return Item{}, false
			case <-f.wake:
				t.Stop()
			case <-timer:
			}
			continue
		}
		select {
		case <-ctx.Done():
			return Item{}, false
		case <-f.wake:
		}
	}
}

// Done marks host's fetch as finished; the host becomes eligible again after delay.
func (f *Frontier) Done(host string, delay time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := f.hosts[host]
	if h == nil || !h.inFlight {
		return
	}
	h.inFlight = false
	f.inFlight--
	h.readyAt = f.now().Add(delay)
	if h.pending() > 0 {
		heap.Push(&f.ready, h)
	} else {
		// Keep politeness state for hosts that may get more URLs later, but free the slice.
		h.queue, h.head = nil, 0
	}
	f.signal()
}

// Close makes every current and future Next call return false. Waiters wake one another in
// a chain (each returning waiter re-signals), so the channel is never closed and a late
// Push or Done cannot panic.
func (f *Frontier) Close() {
	f.mu.Lock()
	f.closed = true
	f.mu.Unlock()
	f.signal()
}

// Stats is a snapshot for metrics.
type Stats struct {
	Queued, InFlight, Hosts int
}

func (f *Frontier) Stats() Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	return Stats{Queued: f.queued, InFlight: f.inFlight, Hosts: len(f.hosts)}
}
