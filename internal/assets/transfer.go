package assets

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"
)

const StallTimeout = 30 * time.Second

// Transfer cancels a transport when no bytes have moved for the idle interval.
// A progressing stream has no total duration limit. Finish must always be called.
func Transfer(parent context.Context, input io.Reader, idle time.Duration, progress func(int64) error) (context.Context, io.Reader, func()) {
	ctx, cancel := context.WithCancelCause(parent)
	var mu sync.Mutex
	last := time.Now()
	stopped := false
	var timer *time.Timer
	mu.Lock()
	timer = time.AfterFunc(idle, func() {
		mu.Lock()
		defer mu.Unlock()
		if stopped {
			return
		}
		remaining := idle - time.Since(last)
		if remaining > 0 {
			timer.Reset(remaining)
		} else {
			cancel(fmt.Errorf("file transfer stalled for %s", idle))
		}
	})
	mu.Unlock()
	reader := &transferReader{ctx: ctx, input: input, progress: func(n int64) error {
		mu.Lock()
		last = time.Now()
		mu.Unlock()
		if progress != nil {
			return progress(n)
		}
		return nil
	}}
	return ctx, reader, func() { mu.Lock(); stopped = true; timer.Stop(); cancel(context.Canceled); mu.Unlock() }
}

type transferReader struct {
	ctx      context.Context
	input    io.Reader
	progress func(int64) error
	count    int64
}

func (r *transferReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, context.Cause(r.ctx)
	}
	if len(p) > 64*1024 {
		p = p[:64*1024]
	}
	n, err := r.input.Read(p)
	if n > 0 {
		r.count += int64(n)
		if e := r.progress(r.count); e != nil {
			return n, e
		}
	}
	return n, err
}
