package capabilities

import (
	"fmt"
	"strings"
	"sync"
	"unicode/utf8"
)

// limitedSyncBuffer is a goroutine-safe O(1)-amortized bounded output buffer used for subprocess stdout/stderr.
// It freezes the initial head half and maintains a circular ring buffer for the rolling tail half so runaway
// log floods stay strictly linear-time and bounded in memory, with an explicit inline omission marker.
type limitedSyncBuffer struct {
	mu       sync.Mutex
	maxBytes int
	headCap  int
	tailCap  int
	head     []byte
	tailRing []byte
	tailPos  int
	tailLen  int
	dropped  int
}

func newLimitedSyncBuffer(maxBytes int) *limitedSyncBuffer {
	if maxBytes <= 0 {
		maxBytes = 256 * 1024
	}
	half := maxBytes / 2
	if half < 1024 {
		return &limitedSyncBuffer{
			maxBytes: maxBytes,
			headCap:  maxBytes,
			tailCap:  0,
		}
	}
	return &limitedSyncBuffer{
		maxBytes: maxBytes,
		headCap:  half,
		tailCap:  maxBytes - half,
	}
}

func (b *limitedSyncBuffer) Write(p []byte) (int, error) {
	total := len(p)
	if total == 0 {
		return 0, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	rem := p
	if len(b.head) < b.headCap {
		need := b.headCap - len(b.head)
		take := min(len(rem), need)
		b.head = append(b.head, rem[:take]...)
		rem = rem[take:]
		if len(rem) == 0 {
			return total, nil
		}
	}

	if b.tailCap == 0 {
		b.dropped += len(rem)
		return total, nil
	}

	if b.tailRing == nil {
		b.tailRing = make([]byte, b.tailCap)
	}

	if len(rem) >= b.tailCap {
		b.dropped += b.tailLen + (len(rem) - b.tailCap)
		copy(b.tailRing, rem[len(rem)-b.tailCap:])
		b.tailPos = 0
		b.tailLen = b.tailCap
		return total, nil
	}

	if b.tailLen+len(rem) > b.tailCap {
		b.dropped += b.tailLen + len(rem) - b.tailCap
		b.tailLen = b.tailCap
	} else {
		b.tailLen += len(rem)
	}

	first := min(len(rem), b.tailCap-b.tailPos)
	copy(b.tailRing[b.tailPos:], rem[:first])
	if first < len(rem) {
		copy(b.tailRing[0:], rem[first:])
	}
	b.tailPos = (b.tailPos + len(rem)) % b.tailCap
	return total, nil
}

func (b *limitedSyncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()

	head := b.head
	var tail []byte
	if b.tailLen > 0 {
		tail = make([]byte, b.tailLen)
		start := (b.tailPos - b.tailLen + b.tailCap) % b.tailCap
		if start+b.tailLen <= b.tailCap {
			copy(tail, b.tailRing[start:start+b.tailLen])
		} else {
			first := b.tailCap - start
			copy(tail, b.tailRing[start:])
			copy(tail[first:], b.tailRing[:b.tailLen-first])
		}
	}

	if b.dropped == 0 {
		if len(tail) == 0 {
			return string(head)
		}
		var sb strings.Builder
		sb.Grow(len(head) + len(tail))
		sb.Write(head)
		sb.Write(tail)
		return sb.String()
	}

	// Trim any split multi-byte UTF-8 boundary around the omitted gap.
	head = trimTrailingPartialUTF8(head)
	tail = trimLeadingPartialUTF8(tail)

	var sb strings.Builder
	if len(tail) == 0 {
		marker := fmt.Sprintf("\n... [%d bytes omitted] ...\n", b.dropped)
		sb.Grow(len(head) + len(marker))
		sb.Write(head)
		sb.WriteString(marker)
		return sb.String()
	}
	marker := fmt.Sprintf("\n... [%d bytes omitted from middle] ...\n", b.dropped)
	sb.Grow(len(head) + len(marker) + len(tail))
	sb.Write(head)
	sb.WriteString(marker)
	sb.Write(tail)
	return sb.String()
}

func (b *limitedSyncBuffer) droppedBytes() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.dropped
}

func trimTrailingPartialUTF8(b []byte) []byte {
	if len(b) == 0 {
		return b
	}
	i := len(b) - 1
	limit := max(0, len(b)-utf8.UTFMax)
	for i > limit && !utf8.RuneStart(b[i]) {
		i--
	}
	if !utf8.RuneStart(b[i]) {
		return b[:i]
	}
	if !utf8.FullRune(b[i:]) {
		return b[:i]
	}
	return b
}

func trimLeadingPartialUTF8(b []byte) []byte {
	for len(b) > 0 && !utf8.RuneStart(b[0]) {
		b = b[1:]
	}
	return b
}
