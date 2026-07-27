package download

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestQueueCoalescesSameRequestAndFansOutResult(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	var calls atomic.Int32
	q := NewQueue(Options{Workers: 1, Capacity: 8})
	t.Cleanup(q.Close)
	request := Request{
		URL: "https://example.test/readme", Profile: "document", Priority: Current,
		Fetch: func(context.Context) (any, error) {
			calls.Add(1)
			close(started)
			<-release
			return "body", nil
		},
	}
	first, err := q.Enqueue(request)
	if err != nil {
		t.Fatal(err)
	}
	<-started
	second, err := q.Enqueue(request)
	if err != nil {
		t.Fatal(err)
	}
	if got := q.Progress(); got.Active != 1 || got.Pending != 0 {
		t.Fatalf("progress while active = %+v", got)
	}
	close(release)
	for index, result := range []Result{<-first, <-second} {
		if result.Err != nil || result.Value != "body" {
			t.Fatalf("result %d = %#v", index, result)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("fetch calls = %d, want 1", calls.Load())
	}
	eventually(t, func() bool {
		progress := q.Progress()
		return progress.Active == 0 && progress.Pending == 0
	})
}

func TestQueuePromotesExistingPendingRequest(t *testing.T) {
	block := make(chan struct{})
	order := make(chan string, 3)
	q := NewQueue(Options{Workers: 1, Capacity: 8})
	t.Cleanup(q.Close)
	enqueue := func(url string, priority Priority) <-chan Result {
		result, err := q.Enqueue(Request{URL: url, Profile: "document", Priority: priority, Fetch: func(context.Context) (any, error) {
			order <- url
			if url == "https://example.test/active" {
				<-block
			}
			return url, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	active := enqueue("https://example.test/active", Current)
	if got := <-order; got != "https://example.test/active" {
		t.Fatal(got)
	}
	low := enqueue("https://example.test/low", Speculative)
	promoted := enqueue("https://example.test/promoted", Speculative)
	secondConsumer := enqueue("https://example.test/promoted", Current)
	close(block)
	<-active
	if got := <-order; got != "https://example.test/promoted" {
		t.Fatalf("first pending start = %q, want promoted", got)
	}
	<-promoted
	<-secondConsumer
	if got := <-order; got != "https://example.test/low" {
		t.Fatalf("second pending start = %q, want low", got)
	}
	<-low
}

func TestQueueDisplacesOnlyLowerPriorityPendingWork(t *testing.T) {
	block := make(chan struct{})
	q := NewQueue(Options{Workers: 1, Capacity: 1, HostInterval: -1})
	t.Cleanup(q.Close)
	active, err := q.Enqueue(Request{URL: "https://one.test/active", Priority: Current, Fetch: func(context.Context) (any, error) {
		<-block
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return q.Progress().Active == 1 })
	low, err := q.Enqueue(Request{URL: "https://two.test/low", Priority: Speculative, Fetch: func(context.Context) (any, error) { return nil, nil }})
	if err != nil {
		t.Fatal(err)
	}
	high, err := q.Enqueue(Request{URL: "https://three.test/high", Priority: Current, Fetch: func(context.Context) (any, error) { return "high", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if result := <-low; !errors.Is(result.Err, ErrDropped) {
		t.Fatalf("displaced result = %#v", result)
	}
	close(block)
	<-active
	if result := <-high; result.Err != nil || result.Value != "high" {
		t.Fatalf("high result = %#v", result)
	}
}

func TestQueueCancelsActiveRequestWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	stopped := make(chan struct{})
	q := NewQueue(Options{Workers: 1, Capacity: 8, HostInterval: -1})
	t.Cleanup(q.Close)
	result, err := q.Enqueue(Request{
		URL: "https://example.test/readme", Context: ctx,
		Fetch: func(fetchCtx context.Context) (any, error) {
			close(started)
			<-fetchCtx.Done()
			close(stopped)
			return nil, fetchCtx.Err()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	select {
	case completed := <-result:
		if !errors.Is(completed.Err, context.Canceled) {
			t.Fatalf("canceled result = %#v", completed)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("active request kept running after its context ended")
	}
	select {
	case <-stopped:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("active fetch did not receive cancellation")
	}
}

func TestQueueRemovesPendingRequestWhenItsContextEnds(t *testing.T) {
	activeRelease := make(chan struct{})
	q := NewQueue(Options{Workers: 1, Capacity: 8, HostInterval: -1})
	t.Cleanup(q.Close)
	t.Cleanup(func() {
		select {
		case <-activeRelease:
		default:
			close(activeRelease)
		}
	})
	active, err := q.Enqueue(Request{URL: "https://example.test/active", Fetch: func(context.Context) (any, error) {
		<-activeRelease
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, func() bool { return q.Progress().Active == 1 })

	ctx, cancel := context.WithCancel(context.Background())
	pending, err := q.Enqueue(Request{URL: "https://example.test/pending", Context: ctx, Fetch: func(context.Context) (any, error) {
		t.Fatal("canceled pending request started")
		return nil, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case completed := <-pending:
		if !errors.Is(completed.Err, context.Canceled) {
			t.Fatalf("canceled pending result = %#v", completed)
		}
	case <-time.After(100 * time.Millisecond):
		t.Fatal("pending request survived cancellation")
	}
	if got := q.Progress().Pending; got != 0 {
		t.Fatalf("pending count = %d, want 0", got)
	}
	close(activeRelease)
	<-active
}

func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition did not become true")
		}
		time.Sleep(time.Millisecond)
	}
}
