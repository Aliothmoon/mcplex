package sandbox

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// extendableDeadlineCtx is a context.Context whose initial deadline can be extended up to maxDeadline
// when the caller omitted an explicit outer timeoutMs and an inner operation (such as sh.exec/sh.run/sh.checks)
// explicitly requests a longer timeout.
type extendableDeadlineCtx struct {
	parent      context.Context
	startedAt   time.Time
	maxDeadline time.Time
	explicit    bool

	mu          sync.Mutex
	deadline    time.Time
	done        chan struct{}
	err         error
	timer       *time.Timer
	gen         uint64
	stopParent  func() bool
	closed      bool
	clippedNote string
}

func newExtendableDeadlineCtx(parent context.Context, initial, maxDur time.Duration, explicit bool) (*extendableDeadlineCtx, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	now := time.Now()
	dl := now.Add(initial)
	maxDL := now.Add(maxDur)
	if pDL, ok := parent.Deadline(); ok && pDL.Before(maxDL) {
		maxDL = pDL
	}
	if dl.After(maxDL) {
		dl = maxDL
	}
	c := &extendableDeadlineCtx{
		parent:      parent,
		startedAt:   now,
		maxDeadline: maxDL,
		explicit:    explicit,
		deadline:    dl,
		done:        make(chan struct{}),
	}
	if pErr := parent.Err(); pErr != nil {
		c.closed = true
		c.err = pErr
		close(c.done)
		return c, func() {}
	}
	c.mu.Lock()
	c.stopParent = context.AfterFunc(parent, func() {
		c.finish(parent.Err())
	})
	d := time.Until(dl)
	if d <= 0 {
		stopParent := c.finishLocked(context.DeadlineExceeded)
		c.mu.Unlock()
		if stopParent != nil {
			stopParent()
		}
	} else {
		c.gen++
		gen := c.gen
		c.timer = time.AfterFunc(d, func() { c.onTimer(gen) })
		c.mu.Unlock()
	}
	return c, func() { c.finish(context.Canceled) }
}

func (c *extendableDeadlineCtx) onTimer(gen uint64) {
	c.mu.Lock()
	if c.closed || c.gen != gen {
		c.mu.Unlock()
		return
	}
	stopParent := c.finishLocked(context.DeadlineExceeded)
	c.mu.Unlock()
	if stopParent != nil {
		stopParent()
	}
}

func (c *extendableDeadlineCtx) finish(err error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	stopParent := c.finishLocked(err)
	c.mu.Unlock()
	if stopParent != nil {
		stopParent()
	}
}

func (c *extendableDeadlineCtx) finishLocked(err error) func() bool {
	c.closed = true
	if err == nil {
		err = context.Canceled
	}
	c.err = err
	if c.timer != nil {
		c.timer.Stop()
		c.timer = nil
	}
	stopParent := c.stopParent
	c.stopParent = nil
	close(c.done)
	return stopParent
}

func (c *extendableDeadlineCtx) Deadline() (time.Time, bool) {
	c.mu.Lock()
	dl := c.deadline
	c.mu.Unlock()
	return dl, true
}

func (c *extendableDeadlineCtx) Done() <-chan struct{} {
	return c.done
}

func (c *extendableDeadlineCtx) Err() error {
	c.mu.Lock()
	err := c.err
	c.mu.Unlock()
	return err
}

func (c *extendableDeadlineCtx) Value(key any) any {
	return c.parent.Value(key)
}

func (c *extendableDeadlineCtx) extendOrNote(op string, need time.Duration) bool {
	if c == nil || need <= 0 {
		return false
	}
	grace := need / 5
	if grace < 200*time.Millisecond {
		grace = 200 * time.Millisecond
	}
	if grace > 2*time.Second {
		grace = 2 * time.Second
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return false
	}
	if c.explicit {
		if time.Now().Add(need).After(c.deadline) && c.clippedNote == "" {
			c.clippedNote = fmt.Sprintf("%s requested timeoutMs=%d", op, need.Milliseconds())
		}
		return false
	}
	want := time.Now().Add(need + grace)
	if want.After(c.maxDeadline) {
		want = c.maxDeadline
	}
	if !want.After(c.deadline) {
		return false
	}
	c.deadline = want
	c.gen++
	gen := c.gen
	if c.timer != nil {
		c.timer.Stop()
	}
	c.timer = time.AfterFunc(time.Until(want), func() { c.onTimer(gen) })
	return true
}

func (c *extendableDeadlineCtx) effectiveTimeout(fallback time.Duration) time.Duration {
	if c == nil {
		return fallback
	}
	c.mu.Lock()
	dl := c.deadline
	c.mu.Unlock()
	d := dl.Sub(c.startedAt).Round(time.Millisecond)
	if d <= 0 {
		return fallback
	}
	return d
}

func (c *extendableDeadlineCtx) clippedTimeoutNote() string {
	if c == nil {
		return ""
	}
	c.mu.Lock()
	note := c.clippedNote
	c.mu.Unlock()
	return note
}

// ExtendDefaultDeadline extends the VM's execution deadline (up to EffectiveMaxTimeout) when
// the caller omitted an explicit outer timeoutMs and an inner op explicitly requested a longer timeout.
// When an explicit outer timeoutMs was set, it does not extend and instead records a note for timeout diagnostics.
func (env *ExecutionEnv) ExtendDefaultDeadline(op string, need time.Duration) bool {
	if env == nil || env.deadlineCtx == nil {
		return false
	}
	return env.deadlineCtx.extendOrNote(op, need)
}
