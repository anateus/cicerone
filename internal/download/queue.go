package download

import (
	"container/heap"
	"context"
	"errors"
	"net/url"
	"strings"
	"sync"
	"time"
)

type Priority int

const (
	Speculative Priority = iota
	Current
)

type Request struct {
	URL, Profile string
	Priority     Priority
	Context      context.Context
	Fetch        func(context.Context) (any, error)
}

type Result struct {
	Value any
	Err   error
}

type Progress struct {
	Active, Pending int
	Sequence        uint64
}

type Options struct {
	Workers, Capacity int
	Context           context.Context
	OnProgress        func(Progress)
	HostInterval      time.Duration
}

type job struct {
	key, url, profile string
	priority          Priority
	sequence          uint64
	index             int
	fetch             func(context.Context) (any, error)
	ctx               context.Context
	cancel            context.CancelFunc
	waiters           []*requestWaiter
	active            bool
}

type requestWaiter struct {
	result   chan Result
	done     chan struct{}
	ctx      context.Context
	finished bool
}

type jobHeap []*job

func (h jobHeap) Len() int { return len(h) }
func (h jobHeap) Less(i, j int) bool {
	if h[i].priority != h[j].priority {
		return h[i].priority > h[j].priority
	}
	return h[i].sequence < h[j].sequence
}
func (h jobHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i]; h[i].index, h[j].index = i, j }
func (h *jobHeap) Push(value any) {
	item := value.(*job)
	item.index = len(*h)
	*h = append(*h, item)
}
func (h *jobHeap) Pop() any {
	old := *h
	item := old[len(old)-1]
	item.index = -1
	*h = old[:len(old)-1]
	return item
}

var ErrClosed = errors.New("download queue is closed")
var ErrFull = errors.New("download queue is full")
var ErrDropped = errors.New("download request was displaced by higher-priority work")

type Queue struct {
	mu           sync.Mutex
	cond         *sync.Cond
	ctx          context.Context
	cancel       context.CancelFunc
	capacity     int
	next         uint64
	pending      jobHeap
	jobs         map[string]*job
	active       int
	closed       bool
	onProgress   func(Progress)
	progressSeq  uint64
	workers      sync.WaitGroup
	hostMu       sync.Mutex
	hostLast     map[string]time.Time
	hostInterval time.Duration
}

func NewQueue(options Options) *Queue {
	workers := options.Workers
	if workers <= 0 {
		workers = 4
	}
	capacity := options.Capacity
	if capacity <= 0 {
		capacity = 128
	}
	parent := options.Context
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	hostInterval := options.HostInterval
	if hostInterval == 0 {
		hostInterval = 100 * time.Millisecond
	}
	q := &Queue{ctx: ctx, cancel: cancel, capacity: capacity, jobs: make(map[string]*job), onProgress: options.OnProgress,
		hostLast: make(map[string]time.Time), hostInterval: hostInterval}
	q.cond = sync.NewCond(&q.mu)
	for range workers {
		q.workers.Add(1)
		go q.worker()
	}
	return q
}

func (q *Queue) Enqueue(request Request) (<-chan Result, error) {
	canonical, err := canonicalURL(request.URL)
	if err != nil {
		return nil, err
	}
	if request.Fetch == nil {
		return nil, errors.New("download fetch function is nil")
	}
	requestContext := request.Context
	if requestContext == nil {
		requestContext = context.Background()
	}
	waiter := &requestWaiter{result: make(chan Result, 1), done: make(chan struct{}), ctx: requestContext}
	if err := requestContext.Err(); err != nil {
		waiter.result <- Result{Err: err}
		close(waiter.result)
		return waiter.result, nil
	}
	key := canonical + "\x00" + request.Profile
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return nil, ErrClosed
	}
	if existing := q.jobs[key]; existing != nil {
		existing.waiters = append(existing.waiters, waiter)
		if !existing.active && request.Priority > existing.priority {
			existing.priority = request.Priority
			heap.Fix(&q.pending, existing.index)
		}
		go q.watchWaiter(existing, waiter)
		return waiter.result, nil
	}
	if len(q.pending) >= q.capacity {
		worst := -1
		for index, pending := range q.pending {
			if worst < 0 || pending.priority < q.pending[worst].priority ||
				(pending.priority == q.pending[worst].priority && pending.sequence > q.pending[worst].sequence) {
				worst = index
			}
		}
		if worst < 0 || request.Priority <= q.pending[worst].priority {
			return nil, ErrFull
		}
		displaced := heap.Remove(&q.pending, worst).(*job)
		delete(q.jobs, displaced.key)
		for _, displacedWaiter := range displaced.waiters {
			q.finishWaiterLocked(displacedWaiter, Result{Err: ErrDropped})
		}
		displaced.cancel()
	}
	q.next++
	jobContext, cancel := context.WithCancel(q.ctx)
	item := &job{key: key, url: canonical, profile: request.Profile, priority: request.Priority, sequence: q.next,
		fetch: request.Fetch, ctx: jobContext, cancel: cancel, waiters: []*requestWaiter{waiter}}
	q.jobs[key] = item
	heap.Push(&q.pending, item)
	q.emitProgressLocked()
	q.cond.Signal()
	go q.watchWaiter(item, waiter)
	return waiter.result, nil
}

func (q *Queue) watchWaiter(item *job, waiter *requestWaiter) {
	select {
	case <-waiter.ctx.Done():
		q.cancelWaiter(item, waiter, waiter.ctx.Err())
	case <-waiter.done:
	}
}

func (q *Queue) cancelWaiter(item *job, waiter *requestWaiter, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if waiter.finished {
		return
	}
	for index, candidate := range item.waiters {
		if candidate == waiter {
			item.waiters = append(item.waiters[:index], item.waiters[index+1:]...)
			break
		}
	}
	q.finishWaiterLocked(waiter, Result{Err: err})
	if len(item.waiters) != 0 {
		return
	}
	if item.active {
		item.cancel()
	} else if item.index >= 0 {
		heap.Remove(&q.pending, item.index)
	}
	if q.jobs[item.key] == item {
		delete(q.jobs, item.key)
	}
	q.emitProgressLocked()
}

func (q *Queue) finishWaiterLocked(waiter *requestWaiter, result Result) {
	if waiter.finished {
		return
	}
	waiter.finished = true
	close(waiter.done)
	waiter.result <- result
	close(waiter.result)
}

func (q *Queue) Progress() Progress {
	q.mu.Lock()
	defer q.mu.Unlock()
	return Progress{Active: q.active, Pending: len(q.pending), Sequence: q.progressSeq}
}

func (q *Queue) Close() {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return
	}
	q.closed = true
	q.cancel()
	for q.pending.Len() > 0 {
		item := heap.Pop(&q.pending).(*job)
		delete(q.jobs, item.key)
		for _, waiter := range item.waiters {
			q.finishWaiterLocked(waiter, Result{Err: ErrClosed})
		}
		item.cancel()
	}
	q.emitProgressLocked()
	q.cond.Broadcast()
	q.mu.Unlock()
	q.workers.Wait()
}

func (q *Queue) worker() {
	defer q.workers.Done()
	for {
		q.mu.Lock()
		for !q.closed && q.pending.Len() == 0 {
			q.cond.Wait()
		}
		if q.closed {
			q.mu.Unlock()
			return
		}
		item := heap.Pop(&q.pending).(*job)
		item.active = true
		q.active++
		q.emitProgressLocked()
		q.mu.Unlock()

		err := q.waitForHost(item.ctx, item.url)
		var value any
		if err == nil {
			value, err = item.fetch(item.ctx)
		}

		q.mu.Lock()
		q.active--
		if q.jobs[item.key] == item {
			delete(q.jobs, item.key)
		}
		for _, waiter := range item.waiters {
			q.finishWaiterLocked(waiter, Result{Value: value, Err: err})
		}
		item.cancel()
		q.emitProgressLocked()
		q.mu.Unlock()
	}
}

func (q *Queue) waitForHost(ctx context.Context, rawURL string) error {
	if q.hostInterval < 0 {
		return nil
	}
	parsed, _ := url.Parse(rawURL)
	host := parsed.Hostname()
	q.hostMu.Lock()
	wait := time.Until(q.hostLast[host].Add(q.hostInterval))
	if wait < 0 {
		wait = 0
	}
	q.hostLast[host] = time.Now().Add(wait)
	q.hostMu.Unlock()
	if wait == 0 {
		return nil
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (q *Queue) emitProgressLocked() {
	if q.onProgress != nil {
		q.progressSeq++
		progress := Progress{Active: q.active, Pending: len(q.pending), Sequence: q.progressSeq}
		go q.onProgress(progress)
	}
}

func canonicalURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.User != nil {
		return "", errors.New("unsafe download URL")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Fragment = ""
	return parsed.String(), nil
}
