package hatchet

import (
	"strings"
	"sync"
	"time"
)

// Hatchet stamps a log message when it arrives, and each is sent on its own,
// so lines logged one by one in quick succession are shown in whatever order
// they happened to land. A step's output is nothing if not in order. Lines
// are therefore gathered for a moment and sent together, as one message of
// several lines, with messages far enough apart to keep their own order.

const (
	// logEvery is how long lines are gathered for before being sent.
	logEvery = 250 * time.Millisecond
	// logAtMost is the size at which a message is sent without waiting.
	// Hatchet cuts a message off at about ten thousand characters.
	logAtMost = 8000
)

// batcher gathers lines and sends them in order.
type batcher struct {
	send func(message string)

	mu      sync.Mutex
	pending []string
	size    int
	last    time.Time
	timer   *time.Timer
}

func newBatcher(send func(string)) *batcher { return &batcher{send: send} }

// Line adds one line of output.
func (b *batcher) Line(line string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.size+len(line)+1 > logAtMost && len(b.pending) > 0 {
		b.flush()
	}
	// One line longer than a message can be is sent cut short rather than
	// lost in Hatchet's own cutting.
	if len(line) > logAtMost {
		line = line[:logAtMost] + " …"
	}
	b.pending = append(b.pending, line)
	b.size += len(line) + 1
	if b.timer == nil {
		b.timer = time.AfterFunc(logEvery, func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			b.flush()
		})
	}
}

// Close sends whatever is still waiting.
func (b *batcher) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.flush()
}

// flush sends the gathered lines as one message. Called with mu held.
func (b *batcher) flush() {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	if len(b.pending) == 0 {
		return
	}
	// Two messages sent back to back can still swap; a moment between them
	// costs nothing next to a step.
	if wait := 20*time.Millisecond - time.Since(b.last); wait > 0 {
		time.Sleep(wait)
	}
	message := strings.Join(b.pending, "\n")
	// Hatchet rejects an empty message; a blank line still matters for
	// reading test output.
	if strings.TrimSpace(message) == "" {
		message = " "
	}
	b.send(message)
	b.last = time.Now()
	b.pending, b.size = nil, 0
}
