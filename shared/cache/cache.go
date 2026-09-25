// Package cache has the two pieces that keep a short-TTL read-through cache from turning into
// a thundering herd against Elasticsearch:
//
//   - Group (single-flight): when a hot key expires, N concurrent requests for it would all
//     miss and all query ES. Group lets the first request run the query and hands its result
//     to the others waiting on the same key, so a miss costs one query per instance.
//   - JitterTTL: entries written at the same moment with the same TTL also expire together;
//     spreading the TTL by a random fraction de-synchronises them.
package cache

import (
	"math/rand"
	"sync"
	"time"
)

type call struct {
	wg  sync.WaitGroup
	val interface{}
	err error
	dup int
}

// Group de-duplicates concurrent calls with the same key.
type Group struct {
	mu sync.Mutex
	m  map[string]*call
}

// Do runs fn once for all concurrent callers of key. shared reports whether the result was
// handed to more than one caller.
func (g *Group) Do(key string, fn func() (interface{}, error)) (v interface{}, err error, shared bool) {
	g.mu.Lock()
	if g.m == nil {
		g.m = make(map[string]*call)
	}
	if c, ok := g.m[key]; ok {
		c.dup++
		g.mu.Unlock()
		c.wg.Wait()
		return c.val, c.err, true
	}
	c := &call{}
	c.wg.Add(1)
	g.m[key] = c
	g.mu.Unlock()

	func() {
		defer func() {
			if r := recover(); r != nil {
				c.err = panicError{r}
			}
		}()
		c.val, c.err = fn()
	}()

	g.mu.Lock()
	delete(g.m, key)
	dup := c.dup
	g.mu.Unlock()
	c.wg.Done()
	return c.val, c.err, dup > 0
}

type panicError struct{ v interface{} }

func (p panicError) Error() string { return "cache: loader panicked" }

var (
	rndMu sync.Mutex
	rnd   = rand.New(rand.NewSource(time.Now().UnixNano()))
)

// JitterTTL returns base ± frac*base, e.g. JitterTTL(10s, 0.2) is uniform in [8s, 12s].
func JitterTTL(base time.Duration, frac float64) time.Duration {
	if frac <= 0 || base <= 0 {
		return base
	}
	rndMu.Lock()
	r := rnd.Float64()
	rndMu.Unlock()
	delta := (r*2 - 1) * frac * float64(base)
	return base + time.Duration(delta)
}
