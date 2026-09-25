package consumer

import (
	"sort"
	"sync"
)

// Position identifies an offset in a topic partition.
type Position struct {
	Topic     string
	Partition int
	Offset    int64
}

type partKey struct {
	topic     string
	partition int
}

type partState struct {
	pending   []int64 // tracked offsets not yet contiguous-done, in fetch order
	done      map[int64]bool
	ready     int64 // highest offset with everything before it done
	committed int64 // highest offset handed out for commit
}

// Tracker turns out-of-order completions into in-order commit positions. Offsets must be
// tracked in the order they were fetched (Kafka delivers each partition in offset order).
type Tracker struct {
	mu    sync.Mutex
	parts map[partKey]*partState
}

func NewTracker() *Tracker { return &Tracker{parts: map[partKey]*partState{}} }

func (t *Tracker) state(topic string, partition int) *partState {
	k := partKey{topic, partition}
	s := t.parts[k]
	if s == nil {
		s = &partState{done: map[int64]bool{}, ready: -1, committed: -1}
		t.parts[k] = s
	}
	return s
}

// Track registers a fetched offset.
func (t *Tracker) Track(topic string, partition int, offset int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.state(topic, partition)
	s.pending = append(s.pending, offset)
}

// Done marks an offset finished.
func (t *Tracker) Done(topic string, partition int, offset int64) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.state(topic, partition)
	s.done[offset] = true
	for len(s.pending) > 0 && s.done[s.pending[0]] {
		s.ready = s.pending[0]
		delete(s.done, s.pending[0])
		s.pending = s.pending[1:]
	}
}

// Committable returns, per partition, the newest offset that can be committed and has not
// been returned before.
func (t *Tracker) Committable() []Position {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []Position
	for k, s := range t.parts {
		if s.ready > s.committed {
			out = append(out, Position{Topic: k.topic, Partition: k.partition, Offset: s.ready})
			s.committed = s.ready
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Topic != out[j].Topic {
			return out[i].Topic < out[j].Topic
		}
		return out[i].Partition < out[j].Partition
	})
	return out
}

// Rewind makes a position committable again after a failed commit.
func (t *Tracker) Rewind(p Position) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := t.state(p.Topic, p.Partition)
	if s.committed >= p.Offset {
		s.committed = p.Offset - 1
	}
}
