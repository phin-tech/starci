package hatchet

import (
	"strings"
	"sync"
	"testing"
	"time"
)

func TestBatcherKeepsLinesInOrderAndTogether(t *testing.T) {
	var mu sync.Mutex
	var sent []string
	b := newBatcher(func(m string) { mu.Lock(); sent = append(sent, m); mu.Unlock() })

	b.Line("one")
	b.Line("")
	b.Line("three")
	time.Sleep(logEvery + 100*time.Millisecond)
	mu.Lock()
	if len(sent) != 1 || sent[0] != "one\n\nthree" {
		t.Errorf("after a pause, sent %q; want the three lines as one message", sent)
	}
	mu.Unlock()

	// More than one message's worth is split, in order, with nothing lost.
	long := strings.Repeat("x", 3000)
	for range 5 {
		b.Line(long)
	}
	b.Line(strings.Repeat("y", logAtMost+500))
	b.Close()
	b.Close()
	mu.Lock()
	defer mu.Unlock()
	if len(sent) != 5 {
		t.Fatalf("sent %d messages, want 5", len(sent))
	}
	for i, m := range sent {
		if len(m) > logAtMost+10 {
			t.Errorf("message %d is %d bytes, more than Hatchet keeps", i, len(m))
		}
	}
	if got := strings.Count(strings.Join(sent[1:4], "\n"), long); got != 5 {
		t.Errorf("%d of the 5 long lines arrived", got)
	}
	if !strings.HasPrefix(sent[4], "yyy") || !strings.HasSuffix(sent[4], " …") {
		t.Errorf("an overlong line should arrive cut short and marked")
	}
}
