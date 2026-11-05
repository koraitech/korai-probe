package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	bufferFile = "results.jsonl"
	bufferAge  = 7 * 24 * time.Hour // Korai refuses older results
	bufferMax  = 500_000            // results kept at most, the oldest go first
)

// Buffer holds the results not yet accepted by Korai, in order, on disk: one JSON line per result.
type Buffer struct {
	mu   sync.Mutex
	path string
	q    []Result
}

func openBuffer(dir string) (*Buffer, error) {
	b := &Buffer{path: filepath.Join(dir, bufferFile)}
	f, err := os.Open(b.path)
	if os.IsNotExist(err) {
		return b, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		var r Result
		if json.Unmarshal(sc.Bytes(), &r) == nil && r.Check != "" { // a torn last line is skipped
			b.q = append(b.q, r)
		}
	}
	b.prune(time.Now())
	return b, b.rewrite()
}

// Add appends results and writes them to disk before returning.
func (b *Buffer) Add(rs []Result) error {
	if len(rs) == 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.q = append(b.q, rs...)
	if b.prune(time.Now()) {
		return b.rewrite()
	}
	f, err := os.OpenFile(b.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range rs {
		_ = enc.Encode(r)
	}
	if _, err := f.Write(buf.Bytes()); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Peek returns up to n of the oldest results and how many are waiting in all.
func (b *Buffer) Peek(n int) ([]Result, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > len(b.q) {
		n = len(b.q)
	}
	return append([]Result(nil), b.q[:n]...), len(b.q)
}

// Drop removes the n oldest results (Korai has them).
func (b *Buffer) Drop(n int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if n > len(b.q) {
		n = len(b.q)
	}
	b.q = append([]Result(nil), b.q[n:]...)
	return b.rewrite()
}

func (b *Buffer) Len() int { b.mu.Lock(); defer b.mu.Unlock(); return len(b.q) }

// prune drops results Korai would refuse anyway, and the oldest beyond bufferMax. True if anything went.
func (b *Buffer) prune(now time.Time) bool {
	cut := now.Add(-bufferAge + time.Hour).Unix()
	i := 0
	for i < len(b.q) && b.q[i].Minute < cut {
		i++
	}
	if over := len(b.q) - i - bufferMax; over > 0 {
		i += over
	}
	if i == 0 {
		return false
	}
	b.q = append([]Result(nil), b.q[i:]...)
	return true
}

func (b *Buffer) rewrite() error {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, r := range b.q {
		_ = enc.Encode(r)
	}
	return writeAtomic(b.path, buf.Bytes())
}
