package main

// Redis / Valkey SLOWLOG reader. Optional: it runs only when KORAI_REDIS_SLOWLOG lists servers.
//
// Every 30 seconds the probe reads SLOWLOG GET from each server it can reach inside the network (self-hosted Redis,
// Valkey, Memorystore, ElastiCache), keeps the entries it has not sent yet and posts them to Korai, which shows them
// in Observability > Infrastructure > Redis > Slow commands. Argument values never leave the probe: an entry carries
// the command name and, for commands that take a key, the key as a pattern ("GET cart:91822" is sent as "GET cart:?").

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	slowlogEvery    = 30 * time.Second
	slowlogCount    = 128  // entries read per server per pass
	slowlogBatch    = 500  // Korai's MAX_ENTRIES per request
	slowlogQueueMax = 5000 // entries kept in memory while Korai cannot take them
	slowlogTargets  = 50   // servers per probe
	redisTimeout    = 5 * time.Second
	redisMaxReply   = 8 << 20
)

// redisTarget is one server from KORAI_REDIS_SLOWLOG.
type redisTarget struct {
	Name     string // shown in Korai: ?name=, or host:port
	Host     string
	Port     int
	User     string
	Password string
	TLS      bool
	Insecure bool
}

// parseTargets reads KORAI_REDIS_SLOWLOG: redis:// or rediss:// URLs separated by commas, spaces or new lines.
// redis://[user:password@]host[:port][?name=label], rediss:// for TLS (?insecure=1 skips the certificate check).
// A password with special characters is URL-encoded (Memorystore AUTH strings are safe as they are).
func parseTargets(s string) ([]redisTarget, []error) {
	var out []redisTarget
	var errs []error
	seen := map[string]bool{}
	for _, raw := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' }) {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "redis" && u.Scheme != "rediss") || u.Hostname() == "" {
			errs = append(errs, fmt.Errorf("%q is not a redis:// or rediss:// URL", redact(raw)))
			continue
		}
		t := redisTarget{Host: u.Hostname(), Port: 6379, TLS: u.Scheme == "rediss"}
		if p := u.Port(); p != "" {
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				errs = append(errs, fmt.Errorf("%q: bad port", redact(raw)))
				continue
			}
			t.Port = n
		}
		if u.User != nil {
			t.User = u.User.Username()
			if pw, ok := u.User.Password(); ok {
				t.Password = pw
			} else if t.User != "" {
				t.Password, t.User = t.User, "" // redis://secret@host: the secret is the password
			}
		}
		q := u.Query()
		t.Insecure = q.Get("insecure") == "1" || q.Get("insecure") == "true"
		t.Name = strings.TrimSpace(q.Get("name"))
		if t.Name == "" {
			t.Name = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
		}
		if len(t.Name) > 120 {
			t.Name = t.Name[:120]
		}
		key := net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
		if seen[key] {
			continue
		}
		seen[key] = true
		if len(out) == slowlogTargets {
			errs = append(errs, fmt.Errorf("more than %d servers: the rest are not read", slowlogTargets))
			break
		}
		out = append(out, t)
	}
	return out, errs
}

var userinfo = regexp.MustCompile(`//[^@/]*@`)

// redact hides the credentials of a URL in a log line.
func redact(s string) string { return userinfo.ReplaceAllString(s, "//***@") }

// ------------------------------------------------------------------ a minimal RESP client

type redisConn struct {
	c net.Conn
	r *bufio.Reader
}

var errRedisReply = errors.New("unexpected reply from the server")

// redisError is an error reply ("-ERR ..."), never a network failure.
type redisError string

func (e redisError) Error() string { return string(e) }

func dialRedis(ctx context.Context, t redisTarget, roots *x509.CertPool) (*redisConn, error) {
	c, err := dialer(redisTimeout)(ctx, "tcp", net.JoinHostPort(t.Host, strconv.Itoa(t.Port)))
	if err != nil {
		return nil, err
	}
	if t.TLS {
		cfg := &tls.Config{ServerName: t.Host, RootCAs: roots, MinVersion: tls.VersionTLS12, InsecureSkipVerify: t.Insecure} //nolint:gosec // only with ?insecure=1
		tc := tls.Client(c, cfg)
		_ = tc.SetDeadline(time.Now().Add(redisTimeout))
		if err := tc.HandshakeContext(ctx); err != nil {
			c.Close()
			return nil, err
		}
		c = tc
	}
	rc := &redisConn{c: c, r: bufio.NewReader(c)}
	if t.Password != "" {
		args := []string{"AUTH", t.Password}
		if t.User != "" {
			args = []string{"AUTH", t.User, t.Password}
		}
		if _, err := rc.do(args...); err != nil {
			rc.Close()
			var re redisError
			if errors.As(err, &re) {
				return nil, redisError("AUTH refused: " + firstWord(string(re)))
			}
			return nil, err
		}
	}
	return rc, nil
}

func firstWord(s string) string {
	if i := strings.IndexByte(s, ' '); i > 0 {
		return s[:i]
	}
	return s
}

func (rc *redisConn) Close() error { return rc.c.Close() }

func (rc *redisConn) do(args ...string) (any, error) {
	_ = rc.c.SetDeadline(time.Now().Add(redisTimeout))
	var b strings.Builder
	fmt.Fprintf(&b, "*%d\r\n", len(args))
	for _, a := range args {
		fmt.Fprintf(&b, "$%d\r\n%s\r\n", len(a), a)
	}
	if _, err := io.WriteString(rc.c, b.String()); err != nil {
		return nil, err
	}
	budget := redisMaxReply
	return readReply(rc.r, &budget, 0)
}

func readReply(r *bufio.Reader, budget *int, depth int) (any, error) {
	if depth > 8 {
		return nil, errRedisReply
	}
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if *budget -= len(line); *budget < 0 || len(line) < 3 || !strings.HasSuffix(line, "\r\n") {
		return nil, errRedisReply
	}
	kind, rest := line[0], line[1:len(line)-2]
	switch kind {
	case '+':
		return rest, nil
	case '-':
		return nil, redisError(rest)
	case ':':
		return strconv.ParseInt(rest, 10, 64)
	case '$':
		n, err := strconv.Atoi(rest)
		if err != nil || n < -1 {
			return nil, errRedisReply
		}
		if n == -1 {
			return nil, nil
		}
		if *budget -= n + 2; *budget < 0 {
			return nil, errRedisReply
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		return string(buf[:n]), nil
	case '*':
		n, err := strconv.Atoi(rest)
		if err != nil || n < -1 || n > 1<<20 {
			return nil, errRedisReply
		}
		if n == -1 {
			return nil, nil
		}
		out := make([]any, 0, min(n, 1024))
		for i := 0; i < n; i++ {
			v, err := readReply(r, budget, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	}
	return nil, errRedisReply
}

// ------------------------------------------------------------------ entries

// SlowEntry is one SLOWLOG entry as sent to Korai: no argument values, no client address or name.
type SlowEntry struct {
	Instance    string `json:"instance"`
	Address     string `json:"address"`
	Port        int    `json:"port"`
	ID          int64  `json:"id"`
	TS          int64  `json:"ts"`
	US          int64  `json:"us"`
	Command     string `json:"command"`
	CommandName string `json:"command_name"`
	Args        int    `json:"args"`
}

// noKey: commands whose first argument is not a key (or is a script, a channel or an option): only the name is sent.
var noKey = map[string]bool{}

func init() {
	for _, c := range strings.Fields(`AUTH HELLO PING ECHO SELECT INFO CONFIG CLIENT CLUSTER COMMAND DBSIZE DEBUG FLUSHALL
		FLUSHDB SLOWLOG LATENCY MEMORY MODULE MONITOR MULTI EXEC DISCARD WATCH UNWATCH SCRIPT EVAL EVALSHA EVAL_RO
		EVALSHA_RO FCALL FCALL_RO FUNCTION KEYS SCAN PUBLISH SUBSCRIBE PSUBSCRIBE UNSUBSCRIBE PUNSUBSCRIBE SPUBLISH
		SSUBSCRIBE SUNSUBSCRIBE XREAD XREADGROUP BLPOP BRPOP BLMPOP BZPOPMIN BZPOPMAX BZMPOP LMPOP ZMPOP WAIT WAITAOF SAVE
		BGSAVE BGREWRITEAOF LASTSAVE SHUTDOWN TIME ROLE REPLICAOF SLAVEOF SWAPDB RANDOMKEY OBJECT ACL RESET QUIT MIGRATE
		PUBSUB FAILOVER LOLWUT SYNC PSYNC READONLY READWRITE ASKING`) {
		noKey[c] = true
	}
}

var (
	idPattern   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}|\b[0-9a-fA-F]{16,}\b|\d+`)
	namePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_|.-]{0,39}$`)
)

// keyPattern removes the ids from a key: cart:91822 -> cart:?, session:3f2a...e1 -> session:?
func keyPattern(k string) string {
	k = strings.ToValidUTF8(k, "?")
	k = idPattern.ReplaceAllString(k, "?")
	k = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, k)
	if r := []rune(k); len(r) > 80 {
		k = string(r[:80])
	}
	return k
}

// describeCommand returns (name, command as sent) for one entry's arguments.
func describeCommand(args []string, keys bool) (string, string) {
	if len(args) == 0 || !namePattern.MatchString(args[0]) {
		return "?", "?"
	}
	name := strings.ToUpper(args[0])
	// SLOWLOG keeps at most 32 arguments and replaces the rest with "... (N more arguments)"
	if !keys || noKey[name] || len(args) < 2 || (strings.HasPrefix(args[1], "...") && strings.Contains(args[1], "more argument")) {
		return name, name
	}
	return name, name + " " + keyPattern(args[1])
}

// toEntry reads one SLOWLOG entry: [id, unix time, microseconds, [args...], client addr, client name].
func toEntry(t redisTarget, v any, keys bool) (SlowEntry, bool) {
	a, ok := v.([]any)
	if !ok || len(a) < 4 {
		return SlowEntry{}, false
	}
	id, ok1 := a[0].(int64)
	ts, ok2 := a[1].(int64)
	us, ok3 := a[2].(int64)
	raw, ok4 := a[3].([]any)
	if !ok1 || !ok2 || !ok3 || !ok4 {
		return SlowEntry{}, false
	}
	args := make([]string, 0, 2)
	for i, x := range raw {
		if i == 2 {
			break
		}
		s, _ := x.(string)
		args = append(args, s)
	}
	name, cmd := describeCommand(args, keys)
	n := 0
	if len(raw) > 1 {
		n = len(raw) - 1
		// the last argument of a truncated list says how many more there were
		if s, _ := raw[len(raw)-1].(string); strings.HasSuffix(s, "more arguments)") {
			if i := strings.LastIndexByte(s, '('); i >= 0 {
				if more, err := strconv.Atoi(strings.Fields(s[i+1:])[0]); err == nil {
					n += more - 1
				}
			}
		}
	}
	return SlowEntry{Instance: t.Name, Address: t.Host, Port: t.Port, ID: id, TS: ts, US: us, Command: cmd, CommandName: name, Args: n}, true
}

// cursor remembers the last entry sent for one server.
type cursor struct {
	ID    int64
	TS    int64
	valid bool
}

// fresh returns the entries not sent yet, oldest first. On the first read it takes only entries newer than since;
// when the ids go backwards (the server restarted; SLOWLOG RESET empties the log but keeps the ids) it starts
// over from the last time it saw.
func (c *cursor) fresh(es []SlowEntry, since int64) []SlowEntry {
	if len(es) == 0 {
		return nil
	}
	top := es[0]
	for _, e := range es {
		if e.ID > top.ID {
			top = e
		}
	}
	reset := c.valid && top.ID < c.ID
	var out []SlowEntry
	for _, e := range es {
		switch {
		case !c.valid && e.TS >= since,
			c.valid && !reset && e.ID > c.ID,
			reset && e.TS >= c.TS:
			out = append(out, e)
		}
	}
	for i := 1; i < len(out); i++ { // insertion sort: at most slowlogCount entries
		for j := i; j > 0 && out[j].ID < out[j-1].ID; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	c.ID, c.valid = top.ID, true
	if top.TS > c.TS {
		c.TS = top.TS
	}
	return out
}

// ------------------------------------------------------------------ the loop

// TargetStatus is what Korai shows on the probe for each server.
type TargetStatus struct {
	Instance string `json:"instance"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Entries  int    `json:"entries"`
}

type slowlog struct {
	st      *State
	targets []redisTarget
	roots   *x509.CertPool
	keys    bool
	mu      sync.Mutex
	cursors map[string]*cursor
	queue   []SlowEntry
	dropped int
	read    func(ctx context.Context, t redisTarget) ([]SlowEntry, error)
	post    func(ctx context.Context, es []SlowEntry, ts []TargetStatus) error
	now     func() time.Time
}

// newSlowlog returns nil when KORAI_REDIS_SLOWLOG is empty.
func newSlowlog(st *State, env func(string) string) *slowlog {
	ts, errs := parseTargets(env("KORAI_REDIS_SLOWLOG"))
	for _, err := range errs {
		log.Printf("korai-probe: KORAI_REDIS_SLOWLOG: %v", err)
	}
	if len(ts) == 0 {
		return nil
	}
	s := &slowlog{st: st, targets: ts, keys: env("KORAI_REDIS_SLOWLOG_KEYS") == "1", cursors: map[string]*cursor{}, now: time.Now}
	if f := env("KORAI_REDIS_CA_FILE"); f != "" {
		pem, err := os.ReadFile(f)
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if err != nil || !pool.AppendCertsFromPEM(pem) {
			log.Printf("korai-probe: KORAI_REDIS_CA_FILE %s: no certificate could be read (%v); using the system ones", f, err)
		} else {
			s.roots = pool
		}
	}
	s.read = s.readServer
	s.post = func(ctx context.Context, es []SlowEntry, ts []TargetStatus) error {
		return postSlowlog(ctx, st, es, ts)
	}
	names := make([]string, len(ts))
	for i, t := range ts {
		names[i] = t.Name
	}
	log.Printf("korai-probe: reading the Redis slowlog of %s", strings.Join(names, ", "))
	return s
}

func (s *slowlog) readServer(ctx context.Context, t redisTarget) ([]SlowEntry, error) {
	rc, err := dialRedis(ctx, t, s.roots)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	v, err := rc.do("SLOWLOG", "GET", strconv.Itoa(slowlogCount))
	if err != nil {
		return nil, err
	}
	list, ok := v.([]any)
	if !ok && v != nil {
		return nil, errRedisReply
	}
	out := make([]SlowEntry, 0, len(list))
	for _, x := range list {
		if e, ok := toEntry(t, x, s.keys); ok {
			out = append(out, e)
		}
	}
	return out, nil
}

func (s *slowlog) run(ctx context.Context) {
	since := s.now().Add(-slowlogEvery).Unix()
	for {
		s.pass(ctx, since)
		if !sleep(ctx, slowlogEvery) {
			return
		}
	}
}

// pass reads every server once, then sends what is waiting.
func (s *slowlog) pass(ctx context.Context, since int64) {
	status := make([]TargetStatus, len(s.targets))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, t := range s.targets {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, t redisTarget) {
			defer func() { <-sem; wg.Done() }()
			es, err := s.read(ctx, t)
			status[i] = TargetStatus{Instance: t.Name, OK: err == nil}
			if err != nil {
				status[i].Error = describeRedisError(err)
				return
			}
			s.mu.Lock()
			c := s.cursors[t.Name]
			if c == nil {
				c = &cursor{}
				s.cursors[t.Name] = c
			}
			f := c.fresh(es, since)
			s.queue = append(s.queue, f...)
			s.mu.Unlock()
			status[i].Entries = len(f)
		}(i, t)
	}
	wg.Wait()
	for i, st := range status {
		if !st.OK {
			log.Printf("korai-probe: Redis %s: %s", s.targets[i].Name, st.Error)
		}
	}
	s.mu.Lock()
	if over := len(s.queue) - slowlogQueueMax; over > 0 {
		s.queue = s.queue[over:] // the oldest go first
		s.dropped += over
	}
	if s.dropped > 0 {
		log.Printf("korai-probe: %d slow commands were not sent (Korai could not take them in time)", s.dropped)
		s.dropped = 0
	}
	s.mu.Unlock()
	s.send(ctx, status)
}

// send posts the queue in batches. The first batch always goes, even empty, so Korai shows each server's status.
func (s *slowlog) send(ctx context.Context, status []TargetStatus) {
	for first := true; ; first = false {
		s.mu.Lock()
		n := min(len(s.queue), slowlogBatch)
		batch := append([]SlowEntry(nil), s.queue[:n]...)
		s.mu.Unlock()
		if n == 0 && !first {
			return
		}
		err := s.post(ctx, batch, status)
		var ae *apiError
		switch {
		case err == nil, errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 413 || ae.Status == 422):
			if err != nil {
				log.Printf("korai-probe: Korai refused %d slow commands, dropping them: %v", n, err)
			}
			s.mu.Lock()
			s.queue = s.queue[n:]
			s.mu.Unlock()
			if n < slowlogBatch {
				return
			}
		default:
			if ctx.Err() == nil && !isRefused(err) {
				log.Printf("korai-probe: cannot send slow commands (%v); trying again in %s", err, slowlogEvery)
			}
			return // they stay in the queue for the next pass
		}
	}
}

// describeRedisError is the short reason shown in Korai: never a password.
func describeRedisError(err error) string {
	var re redisError
	switch {
	case errors.Is(err, errBlocked):
		return "blocked: link-local or metadata address"
	case errors.As(err, &re):
		return truncate(string(re), 160)
	case isTimeout(err):
		return "timeout"
	}
	var de *net.DNSError
	if errors.As(err, &de) {
		return "DNS: no such host"
	}
	var ce x509.UnknownAuthorityError
	if errors.As(err, &ce) {
		return "TLS: certificate not trusted (set KORAI_REDIS_CA_FILE)"
	}
	var he x509.HostnameError
	if errors.As(err, &he) {
		return "TLS: certificate does not match the host"
	}
	if errors.Is(err, io.EOF) {
		return "connection closed by the server (TLS needed? use rediss://)"
	}
	return truncate(classify(err)+": "+err.Error(), 160)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
