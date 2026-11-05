package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const (
	configFile  = "checks.json"
	batchMax    = 2000 // Korai's MAX_RESULTS per request
	parallel    = 16   // checks running at once
	minuteSkew  = time.Second
	configEvery = time.Minute
)

type agent struct {
	dir   string
	st    *State
	buf   *Buffer
	mu    sync.Mutex
	conf  []Check
	etag  string
	kick  chan struct{}
	batch int
	now   func() time.Time
}

func newAgent(dir string, st *State) (*agent, error) {
	buf, err := openBuffer(dir)
	if err != nil {
		return nil, err
	}
	a := &agent{dir: dir, st: st, buf: buf, kick: make(chan struct{}, 1), batch: batchMax, now: time.Now}
	// The last checks Korai sent: the probe keeps running them while Korai cannot be reached, even after a restart.
	if b, err := os.ReadFile(filepath.Join(dir, configFile)); err == nil {
		var c Config
		if json.Unmarshal(b, &c) == nil {
			a.conf, a.etag = c.Checks, c.ETag
		}
	}
	return a, nil
}

func (a *agent) checks() []Check { a.mu.Lock(); defer a.mu.Unlock(); return a.conf }

func (a *agent) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); a.configLoop(ctx) }()
	go func() { defer wg.Done(); a.sendLoop(ctx) }()
	a.minuteLoop(ctx)
	wg.Wait()
	// One last try to deliver what is waiting; whatever is not delivered stays on disk for the next start.
	fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a.flush(fctx)
}

func (a *agent) configLoop(ctx context.Context) {
	refused := false
	for {
		c, tag, err := fetchConfig(ctx, a.st, a.etag)
		switch {
		case err != nil && ctx.Err() != nil:
			return
		case isRefused(err):
			if !refused {
				log.Printf("korai-probe: Korai refused this probe's credential (removed, or reinstalled elsewhere): "+
					"create a new install token in Korai and restart with it. (%v)", err)
			}
			refused = true
		case err != nil:
			log.Printf("korai-probe: cannot download the checks (%v); running the last ones", err)
		case c != nil:
			refused = false
			a.mu.Lock()
			changed := len(c.Checks) != len(a.conf) || a.etag == ""
			a.conf, a.etag = c.Checks, tag
			a.mu.Unlock()
			c.ETag = tag
			if b, err := json.Marshal(c); err == nil {
				_ = writeAtomic(filepath.Join(a.dir, configFile), b)
			}
			if changed {
				log.Printf("korai-probe: %d checks", len(c.Checks))
			}
		default:
			refused = false
		}
		if !sleep(ctx, configEvery) {
			return
		}
	}
}

// minuteLoop runs every check once a minute, just after the minute starts.
func (a *agent) minuteLoop(ctx context.Context) {
	for {
		now := a.now()
		next := now.Truncate(time.Minute).Add(time.Minute + minuteSkew)
		if !sleep(ctx, next.Sub(now)) {
			return
		}
		minute := next.Truncate(time.Minute).Unix()
		cs := a.checks()
		if len(cs) == 0 {
			a.nudge()
			continue
		}
		// Each run has its own budget; a stop signal does not cut it short with a false "timeout". If the probe
		// stops mid-minute that minute is simply not reported (no data, never down).
		done := make(chan []Result, 1)
		go func() { done <- runAll(context.Background(), cs, minute) }()
		select {
		case <-ctx.Done():
			return
		case rs := <-done:
			if err := a.buf.Add(rs); err != nil {
				log.Printf("korai-probe: cannot keep results on disk: %v", err)
			}
			down := 0
			for _, r := range rs {
				if !r.OK {
					down++
					log.Printf("korai-probe: %s %s", label(cs, r.Check), describe(r))
				}
			}
			if down == 0 {
				log.Printf("korai-probe: %d checks up", len(rs))
			}
			a.nudge()
		}
	}
}

func label(cs []Check, id string) string {
	for _, c := range cs {
		if c.ID == id {
			return c.Name
		}
	}
	return id
}

func runAll(ctx context.Context, cs []Check, minute int64) []Result {
	out := make([]Result, len(cs))
	sem := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i, c := range cs {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, c Check) {
			defer func() { <-sem; wg.Done() }()
			r := Run(ctx, c)
			r.Check, r.Minute = c.ID, minute
			out[i] = r
		}(i, c)
	}
	wg.Wait()
	return out
}

func (a *agent) nudge() {
	select {
	case a.kick <- struct{}{}:
	default:
	}
}

// sendLoop delivers waiting results after every minute, and keeps Korai told how many are waiting.
func (a *agent) sendLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-a.kick:
		}
		a.flush(ctx)
	}
}

// flush sends the buffer, oldest first, until it is empty or Korai cannot take more now.
func (a *agent) flush(ctx context.Context) {
	delay := 2 * time.Second
	for tries := 0; ; tries++ {
		rs, total := a.buf.Peek(a.batch)
		err := postResults(ctx, a.st, rs, total-len(rs))
		var ae *apiError
		switch {
		case err == nil:
			if len(rs) > 0 {
				if err := a.buf.Drop(len(rs)); err != nil {
					log.Printf("korai-probe: cannot update the results on disk: %v", err)
				}
			}
			if a.batch < batchMax {
				a.batch *= 2
				if a.batch > batchMax {
					a.batch = batchMax
				}
			}
			if a.buf.Len() == 0 {
				return
			}
			continue
		case ctx.Err() != nil:
			return
		case errors.As(err, &ae) && ae.Status == 413 && a.batch > 1:
			a.batch /= 2
			continue
		case errors.As(err, &ae) && ae.Status == 429:
			d := ae.RetryAfter
			if d == 0 {
				d = 30 * time.Second
			}
			if !sleep(ctx, d) {
				return
			}
			continue
		case errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 422):
			// Korai will never take this batch (it is malformed): keeping it would block every later result.
			log.Printf("korai-probe: Korai refused %d results, dropping them: %v", len(rs), err)
			_ = a.buf.Drop(len(rs))
			continue
		case isRefused(err):
			return // configLoop says why; the results wait on disk for a new credential
		}
		if tries == 0 {
			log.Printf("korai-probe: cannot send results (%v); %d waiting on disk", err, total)
		}
		if tries >= 2 || !sleep(ctx, delay) {
			return // the next minute tries again
		}
		delay *= 2
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
