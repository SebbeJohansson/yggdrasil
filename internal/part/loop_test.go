package part

import (
	"context"
	"errors"
	"io"
	"log"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A failed run is tried again after RetryAfter, not after the loop's hour.
func TestLoopRetriesSoonAfterAFailure(t *testing.T) {
	saved := RetryAfter
	RetryAfter = 10 * time.Millisecond
	defer func() { RetryAfter = saved }()

	var runs atomic.Int32
	done := make(chan struct{})
	p := New("test", nil, nil)
	loop := p.Loop("plan", time.Hour, func(context.Context) error {
		if runs.Add(1) == 1 {
			return errors.New("connection refused")
		}
		close(done)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	loop.start(ctx, log.New(io.Discard, "", 0), &wg)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the failed run was not tried again")
	}
	cancel()
	wg.Wait()
	loop.mu.Lock()
	defer loop.mu.Unlock()
	if loop.lastError != "" {
		t.Fatalf("lastError = %q after a good run, want none", loop.lastError)
	}
}

// A run that works waits the loop's full interval.
func TestLoopWaitsItsIntervalAfterASuccess(t *testing.T) {
	saved := RetryAfter
	RetryAfter = 10 * time.Millisecond
	defer func() { RetryAfter = saved }()

	var runs atomic.Int32
	p := New("test", nil, nil)
	loop := p.Loop("plan", time.Hour, func(context.Context) error {
		runs.Add(1)
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	loop.start(ctx, log.New(io.Discard, "", 0), &wg)
	time.Sleep(100 * time.Millisecond)
	cancel()
	wg.Wait()
	if got := runs.Load(); got != 1 {
		t.Fatalf("runs = %d, want 1", got)
	}
}
