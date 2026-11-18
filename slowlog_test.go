package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestParseTargets(t *testing.T) {
	ts, errs := parseTargets("redis://10.0.0.3, rediss://default:p%40ss@10.0.0.4:6378?name=sessions&insecure=1\n" +
		"redis://:secret@cache.internal:6380 redis://onlysecret@10.0.0.5 http://x redis://10.0.0.3:6379")
	if len(errs) != 1 || !strings.Contains(errs[0].Error(), "http://x") {
		t.Fatalf("errors: %v", errs)
	}
	if len(ts) != 4 {
		t.Fatalf("targets: %+v", ts)
	}
	if ts[0].Name != "10.0.0.3:6379" || ts[0].TLS || ts[0].Password != "" {
		t.Errorf("plain: %+v", ts[0])
	}
	if ts[1].Name != "sessions" || !ts[1].TLS || !ts[1].Insecure || ts[1].User != "default" || ts[1].Password != "p@ss" || ts[1].Port != 6378 {
		t.Errorf("tls: %+v", ts[1])
	}
	if ts[2].User != "" || ts[2].Password != "secret" || ts[2].Port != 6380 {
		t.Errorf("password only: %+v", ts[2])
	}
	if ts[3].User != "" || ts[3].Password != "onlysecret" {
		t.Errorf("secret as user: %+v", ts[3])
	}
	if s := redact("redis://default:hunter2@10.0.0.4"); strings.Contains(s, "hunter2") {
		t.Errorf("redact: %s", s)
	}
}

func TestDescribeCommandSendsNoValues(t *testing.T) {
	for _, c := range []struct {
		args       []string
		name, sent string
	}{
		{[]string{"get", "cart:91822"}, "GET", "GET cart:?"},
		{[]string{"SET", "session:3f2a9c1e-77aa-4b1e-9a0b-1234567890ab", "{\"card\":\"4111\"}"}, "SET", "SET session:?"},
		{[]string{"HGETALL", "user:00ff00ff00ff00ff:profile"}, "HGETALL", "HGETALL user:?:profile"},
		{[]string{"AUTH", "hunter2"}, "AUTH", "AUTH"},
		{[]string{"EVAL", "return redis.call('get', KEYS[1])", "1", "k"}, "EVAL", "EVAL"},
		{[]string{"MSET", "... (40 more arguments)"}, "MSET", "MSET"},
		{[]string{"weird name!", "x"}, "?", "?"},
		{nil, "?", "?"},
	} {
		name, sent := describeCommand(c.args, true)
		if name != c.name || sent != c.sent {
			t.Errorf("%q: got %q %q", c.args, name, sent)
		}
	}
	if _, sent := describeCommand([]string{"GET", "cart:1"}, false); sent != "GET" {
		t.Errorf("keys off: %q", sent)
	}
}

func TestCursorSendsEachEntryOnceAndSurvivesARestart(t *testing.T) {
	e := func(id, ts int64) SlowEntry { return SlowEntry{ID: id, TS: ts} }
	var c cursor
	got := c.fresh([]SlowEntry{e(12, 1012), e(11, 1011), e(10, 990)}, 1000)
	if len(got) != 2 || got[0].ID != 11 || got[1].ID != 12 {
		t.Fatalf("first read: %+v", got)
	}
	if got := c.fresh([]SlowEntry{e(13, 1013), e(12, 1012)}, 1000); len(got) != 1 || got[0].ID != 13 {
		t.Fatalf("second read: %+v", got)
	}
	if got := c.fresh([]SlowEntry{e(13, 1013)}, 1000); len(got) != 0 {
		t.Fatalf("nothing new: %+v", got)
	}
	// the server restarted: ids start again from 0
	if got := c.fresh([]SlowEntry{e(1, 1020), e(0, 1019)}, 1000); len(got) != 2 || got[0].ID != 0 {
		t.Fatalf("after restart: %+v", got)
	}
	if got := c.fresh([]SlowEntry{e(2, 1021), e(1, 1020)}, 1000); len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("after restart, next read: %+v", got)
	}
}

// fakeRedis answers AUTH and SLOWLOG GET like Redis does.
func fakeRedis(t *testing.T, password string, entries func() string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				r := bufio.NewReader(c)
				authed := password == ""
				for {
					args, err := readCommand(r)
					if err != nil {
						return
					}
					switch strings.ToUpper(args[0]) {
					case "AUTH":
						if args[len(args)-1] == password {
							authed = true
							io.WriteString(c, "+OK\r\n")
						} else {
							io.WriteString(c, "-WRONGPASS invalid username-password pair or user is disabled.\r\n")
						}
					case "SLOWLOG":
						if !authed {
							io.WriteString(c, "-NOAUTH Authentication required.\r\n")
						} else {
							io.WriteString(c, entries())
						}
					}
				}
			}(c)
		}
	}()
	return ln.Addr().String()
}

func readCommand(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	n, _ := strconv.Atoi(strings.TrimSpace(line[1:]))
	out := make([]string, n)
	for i := range out {
		if _, err := r.ReadString('\n'); err != nil {
			return nil, err
		}
		s, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		out[i] = strings.TrimSuffix(s, "\r\n")
	}
	return out, nil
}

func bulk(s string) string { return fmt.Sprintf("$%d\r\n%s\r\n", len(s), s) }

func slowEntry(id, ts, us int64, args ...string) string {
	b := fmt.Sprintf("*6\r\n:%d\r\n:%d\r\n:%d\r\n*%d\r\n", id, ts, us, len(args))
	for _, a := range args {
		b += bulk(a)
	}
	return b + bulk("10.1.2.3:51234") + bulk("worker")
}

func TestSlowlogReadsTheServerAndPostsMaskedEntries(t *testing.T) {
	now := time.Now().Unix()
	var mu sync.Mutex
	list := []string{slowEntry(8, now, 25000, "HGETALL", "cart:91822"), slowEntry(7, now-1, 12000, "SET", "token:42", "secret-value")}
	addr := fakeRedis(t, "s3cret", func() string {
		mu.Lock()
		defer mu.Unlock()
		return fmt.Sprintf("*%d\r\n%s", len(list), strings.Join(list, ""))
	})

	var got []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/probe/v1/slowlog" || r.Header.Get("Authorization") != "Bearer kprb_x" {
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		got = append(got, body)
		mu.Unlock()
		w.Write([]byte(`{"accepted":0,"errors":[]}`))
	}))
	defer srv.Close()

	st := &State{KoraiURL: srv.URL, Credential: "kprb_x"}
	env := map[string]string{"KORAI_REDIS_SLOWLOG_KEYS": "1", "KORAI_REDIS_SLOWLOG": "redis://:s3cret@" + addr + "?name=cache, redis://:wrong@" + addr + "?name=other"}
	sl := newSlowlog(st, func(k string) string { return env[k] })
	if sl == nil || len(sl.targets) != 1 {
		// both URLs point at the same host:port, so the second is a duplicate
		t.Fatalf("targets: %+v", sl)
	}
	sl.targets = append(sl.targets, redisTarget{Name: "other", Host: "127.0.0.1", Port: sl.targets[0].Port, Password: "wrong"})
	ctx := context.Background()
	sl.pass(ctx, now-30)

	if len(got) != 1 {
		t.Fatalf("posts: %d", len(got))
	}
	raw, _ := json.Marshal(got[0])
	for _, leak := range []string{"91822", "secret-value", "s3cret", "10.1.2.3", "worker", "token:42"} {
		if strings.Contains(string(raw), leak) {
			t.Errorf("%q was sent: %s", leak, raw)
		}
	}
	es := got[0]["entries"].([]any)
	if len(es) != 2 {
		t.Fatalf("entries: %s", raw)
	}
	first := es[0].(map[string]any)
	if first["command"] != "SET token:?" || first["instance"] != "cache" || first["us"].(float64) != 12000 || first["args"].(float64) != 2 {
		t.Errorf("first entry: %v", first)
	}
	tg := got[0]["targets"].([]any)
	if tg[0].(map[string]any)["ok"] != true || tg[0].(map[string]any)["entries"].(float64) != 2 {
		t.Errorf("cache status: %v", tg[0])
	}
	if o := tg[1].(map[string]any); o["ok"] != false || !strings.HasPrefix(o["error"].(string), "AUTH refused: WRONGPASS") {
		t.Errorf("other status: %v", o)
	}

	// next pass: only the new entry, and the status still goes out when there is nothing new
	mu.Lock()
	list = append([]string{slowEntry(9, now, 40000, "GET", "cart:7")}, list...)
	mu.Unlock()
	sl.pass(ctx, now-30)
	sl.pass(ctx, now-30)
	if len(got) != 3 || len(got[1]["entries"].([]any)) != 1 || len(got[2]["entries"].([]any)) != 0 {
		t.Fatalf("later passes: %v", got[1:])
	}
}

func TestSlowlogKeepsEntriesWhileKoraiCannotTakeThem(t *testing.T) {
	sl := &slowlog{targets: []redisTarget{{Name: "a"}}, cursors: map[string]*cursor{}, now: time.Now}
	n := 0
	sl.read = func(context.Context, redisTarget) ([]SlowEntry, error) {
		n++
		return []SlowEntry{{ID: int64(n), TS: 2000}}, nil
	}
	fail := true
	var sent []SlowEntry
	sl.post = func(_ context.Context, es []SlowEntry, _ []TargetStatus) error {
		if fail {
			return &apiError{Status: 503}
		}
		sent = append(sent, es...)
		return nil
	}
	sl.pass(context.Background(), 1000)
	sl.pass(context.Background(), 1000)
	if len(sl.queue) != 2 || len(sent) != 0 {
		t.Fatalf("queue %d sent %d", len(sl.queue), len(sent))
	}
	fail = false
	sl.pass(context.Background(), 1000)
	if len(sl.queue) != 0 || len(sent) != 3 || sent[0].ID != 1 {
		t.Fatalf("queue %d sent %+v", len(sl.queue), sent)
	}
}

func TestSlowlogRefusesMetadataAddresses(t *testing.T) {
	sl := &slowlog{}
	_, err := sl.readServer(context.Background(), redisTarget{Name: "md", Host: "169.254.169.254", Port: 6379})
	if err == nil || describeRedisError(err) != "blocked: link-local or metadata address" {
		t.Fatalf("got %v", err)
	}
}

func TestKeysAreOffByDefault(t *testing.T) {
	sl := newSlowlog(&State{}, func(k string) string {
		if k == "KORAI_REDIS_SLOWLOG" {
			return "redis://10.0.0.3"
		}
		return ""
	})
	if sl == nil || sl.keys {
		t.Fatalf("keys must be off unless KORAI_REDIS_SLOWLOG_KEYS=1: %+v", sl)
	}
	for _, args := range [][]string{
		{"GET", "session:jose@example.com"},
		{"HGETALL", "customer:john_smith"},
		{"SET", "password-reset:Zx81kQ2vLmN0pR7s", "1"},
		{"GET", "token:01HZX3V7K9M2Q4R6T8W0Y1A3C5"},
		{"GET", "auth:aGVsbG8gd29ybGQ="},
		{"JSON.GET", "account:premium-customer-name", "$"},
	} {
		if _, sent := describeCommand(args, sl.keys); sent != strings.ToUpper(args[0]) {
			t.Errorf("%q sent %q", args, sent)
		}
	}
}
