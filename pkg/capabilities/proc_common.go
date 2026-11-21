package capabilities

import (
	"bytes"
	"sync"
)

// limitedSyncBuffer is a goroutine-safe bounded output buffer used for subprocess stdout/stderr.
// It prevents both OOM on runaway log floods and data races if os/exec's WaitDelay closes pipes early.
type limitedSyncBuffer struct {
	mu       sync.Mutex
	maxBytes int
	buf      bytes.Buffer
	dropped  int
}

func newLimitedSyncBuffer(maxBytes int) *limitedSyncBuffer {
	if maxBytes <= 0 {
		maxBytes = 256 * 1024
	}
	return &limitedSyncBuffer{maxBytes: maxBytes}
}

func (b *limitedSyncBuffer) Write(p []byte) (int, error) {
	total := len(p)
	if total == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.buf.Len()+total <= b.maxBytes {
		_, _ = b.buf.Write(p)
		return total, nil
	}

	// Keep head half + rolling latest tail half so both startup banners and final error lines survive!
	half := b.maxBytes / 2
	if half < 1024 {
		rem := b.maxBytes - b.buf.Len()
		if rem > 0 {
			_, _ = b.buf.Write(p[:rem])
		}
		b.dropped += total - max(0, rem)
		return total, nil
	}

	combined := append(b.buf.Bytes(), p...)
	excess := len(combined) - b.maxBytes
	b.dropped += excess
	head := append([]byte(nil), combined[:half]...)
	tail := append([]byte(nil), combined[len(combined)-(b.maxBytes-half):]...)
	b.buf.Reset()
	_, _ = b.buf.Write(head)
	_, _ = b.buf.Write(tail)
	return total, nil
}

func (b *limitedSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *limitedSyncBuffer) droppedBytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}
