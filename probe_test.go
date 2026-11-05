package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func b64(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }

func token(url string) string {
	return strings.Join([]string{"kpe1", b64(url), b64("acme"), "pr-abcdefghij", strings.Repeat("s", 43)}, ".")
}

func TestParseToken(t *testing.T) {
	tok, err := parseToken("  " + token("https://app.korai.tech/") + "\n")
	if err != nil || tok.URL != "https://app.korai.tech" || tok.Tenant != "acme" || tok.Probe != "pr-abcdefghij" {
		t.Fatalf("got %+v %v", tok, err)
	}
	for _, bad := range []string{"", "kpe1.a.b.c.d", strings.Replace(token("https://x"), "pr-abcdefghij", "pr-X", 1),
		"kpe2" + token("https://x")[4:], token("ftp://x")} {
		if _, err := parseToken(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestBufferKeepsOrderAcrossRestarts(t *testing.T) {
	dir := t.TempDir()
	b, _ := openBuffer(dir)
	now := time.Now().Unix() / 60 * 60
	old := Result{Check: "old", Minute: now - 8*86400, Attempts: 1}
	if err := b.Add([]Result{old, {Check: "a", Minute: now - 60, OK: true}, {Check: "b", Minute: now}}); err != nil {
		t.Fatal(err)
	}
	b2, err := openBuffer(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, total := b2.Peek(10)
	if total != 2 || got[0].Check != "a" || got[1].Check != "b" {
		t.Fatalf("after restart: %+v (%d)", got, total) // the 8-day-old result is gone, the rest in order
	}
	_ = b2.Drop(1)
	b3, _ := openBuffer(dir)
	if got, _ := b3.Peek(10); len(got) != 1 || got[0].Check != "b" {
		t.Fatalf("after drop: %+v", got)
	}
}

func check(url string) Check { return Check{ID: "c1", Kind: "http", URL: url, TimeoutS: 5} }

func TestHTTPCheck(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = io.WriteString(w, "all systems fine")
		case "/down":
			w.WriteHeader(503)
		case "/secret":
			if r.Header.Get("X-Token") != "s3cret" {
				w.WriteHeader(401)
			}
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	if r := probeHTTP(ctx, check(srv.URL+"/ok")); !r.OK || *r.Status != 200 {
		t.Fatalf("ok: %+v", r)
	}
	c := check(srv.URL + "/ok")
	c.Keyword = "fine"
	if r := probeHTTP(ctx, c); !r.OK {
		t.Fatalf("keyword: %+v", r)
	}
	c.Keyword = "broken"
	if r := probeHTTP(ctx, c); r.OK || r.Error != "keyword" {
		t.Fatalf("missing keyword: %+v", r)
	}
	if r := probeHTTP(ctx, check(srv.URL+"/down")); r.OK || r.Error != "status" || *r.Status != 503 {
		t.Fatalf("down: %+v", r)
	}
	c = check(srv.URL + "/secret")
	c.HeaderName, c.HeaderValue = "X-Token", "s3cret"
	if r := probeHTTP(ctx, c); !r.OK {
		t.Fatalf("header: %+v", r)
	}
	if r := probeHTTP(ctx, check("ftp://x")); r.Error != "invalid" {
		t.Fatalf("invalid: %+v", r)
	}
}

func TestTokenNeverFollowsARedirectToAnotherOrigin(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("X-Token") != ""
	}))
	defer other.Close()
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusFound)
	}))
	defer home.Close()
	c := check(home.URL)
	c.HeaderName, c.HeaderValue = "X-Token", "s3cret"
	r := probeHTTP(context.Background(), c)
	if leaked || r.Status == nil || *r.Status != 302 {
		t.Fatalf("leaked=%v %+v", leaked, r)
	}
}

func TestMetadataAddressesAreBlocked(t *testing.T) {
	for _, h := range []string{"169.254.169.254", "fe80::1", "0.0.0.0", "224.0.0.1", "fd00:ec2::254"} {
		if !isLinkLocal(net.ParseIP(h)) {
			t.Errorf("%s not blocked", h)
		}
	}
	for _, h := range []string{"10.0.0.5", "192.168.1.1", "127.0.0.1", "8.8.8.8"} {
		if isLinkLocal(net.ParseIP(h)) {
			t.Errorf("%s blocked", h)
		}
	}
	r := Run(context.Background(), check("http://169.254.169.254/latest/meta-data/"))
	if r.OK || r.Error != "blocked" || r.Attempts != 1 {
		t.Fatalf("metadata: %+v", r)
	}
}

func TestTCPCheckAndRetry(t *testing.T) {
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	if r := Run(context.Background(), Check{ID: "t", Kind: "tcp", Host: "127.0.0.1", Port: port, TimeoutS: 2}); !r.OK {
		t.Fatalf("tcp: %+v", r)
	}
	ln.Close()
	r := Run(context.Background(), Check{ID: "t", Kind: "tcp", Host: "127.0.0.1", Port: port, TimeoutS: 2})
	if r.OK || r.Error != "connect" || r.Attempts != 2 {
		t.Fatalf("closed port: %+v", r) // failures are tried a second time before they count
	}
}

// fakeKorai is the probe API: enroll once, config with an ETag, results recorded (or refused while down).
type fakeKorai struct {
	mu       sync.Mutex
	down     bool
	enrolled int
	results  []Result
	buffered []int
	etagHits int
}

func (f *fakeKorai) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down {
		w.WriteHeader(502)
		return
	}
	switch r.URL.Path {
	case "/api/probe/v1/enroll":
		f.enrolled++
		_ = json.NewEncoder(w).Encode(map[string]any{"credential": "kprb_x.pr-abcdefghij." + strings.Repeat("c", 30)})
	case "/api/probe/v1/config":
		if r.Header.Get("Authorization") == "" {
			w.WriteHeader(401)
			return
		}
		if r.Header.Get("If-None-Match") == `"v1"` {
			f.etagHits++
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_ = json.NewEncoder(w).Encode(map[string]any{"checks": []Check{{ID: "c1", Kind: "tcp", Host: "127.0.0.1", Port: 1}}})
	case "/api/probe/v1/results":
		var d struct {
			Results  []Result `json:"results"`
			Buffered int      `json:"buffered"`
		}
		_ = json.NewDecoder(r.Body).Decode(&d)
		f.results = append(f.results, d.Results...)
		f.buffered = append(f.buffered, d.Buffered)
		_, _ = io.WriteString(w, `{"accepted":1}`)
	}
}

func TestEnrollOnceThenKeepTheCredential(t *testing.T) {
	f := &fakeKorai{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dir := t.TempDir()
	tok := token(srv.URL)
	st, err := setup(context.Background(), dir, tok, "")
	if err != nil || st.Tenant != "acme" || !strings.HasPrefix(st.Credential, "kprb_") {
		t.Fatalf("%+v %v", st, err)
	}
	if _, err := setup(context.Background(), dir, tok, ""); err != nil || f.enrolled != 1 {
		t.Fatalf("restart enrolled again: %d %v", f.enrolled, err)
	}
	if _, err := setup(context.Background(), dir, "", ""); err != nil {
		t.Fatalf("no token, credential on disk: %v", err)
	}
	if _, err := setup(context.Background(), t.TempDir(), "", ""); err == nil {
		t.Fatal("started with neither a token nor a credential")
	}
	tok2 := strings.Replace(tok, strings.Repeat("s", 43), strings.Repeat("t", 43), 1)
	if _, err := setup(context.Background(), dir, tok2, ""); err != nil || f.enrolled != 2 {
		t.Fatalf("a new token re-enrolls: %d %v", f.enrolled, err)
	}
}

func TestResultsWaitWhileKoraiIsDownAndArriveInOrder(t *testing.T) {
	f := &fakeKorai{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	dir := t.TempDir()
	st := &State{KoraiURL: srv.URL, Credential: "kprb_x"}
	a, err := newAgent(dir, st)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	m := time.Now().Unix() / 60 * 60
	f.down = true
	for i := int64(0); i < 3; i++ {
		_ = a.buf.Add([]Result{{Check: "c1", Minute: m + 60*i, OK: i != 1}})
	}
	a.flush(ctx)
	if a.buf.Len() != 3 {
		t.Fatalf("lost results while Korai was down: %d", a.buf.Len())
	}
	// The probe restarts while Korai is still away: the results are still there.
	a, _ = newAgent(dir, st)
	f.mu.Lock()
	f.down = false
	f.mu.Unlock()
	a.batch = 2
	a.flush(ctx)
	if a.buf.Len() != 0 || len(f.results) != 3 || f.results[0].Minute != m || f.results[2].Minute != m+120 {
		t.Fatalf("delivered %+v, %d left", f.results, a.buf.Len())
	}
	if f.buffered[0] != 1 || f.buffered[len(f.buffered)-1] != 0 {
		t.Fatalf("buffered counts %v", f.buffered)
	}
}

func TestConfigIsCachedAndPulledWithETag(t *testing.T) {
	f := &fakeKorai{}
	srv := httptest.NewServer(f)
	defer srv.Close()
	st := &State{KoraiURL: srv.URL, Credential: "kprb_x"}
	c, tag, err := fetchConfig(context.Background(), st, "")
	if err != nil || len(c.Checks) != 1 || tag != `"v1"` {
		t.Fatalf("%+v %q %v", c, tag, err)
	}
	if c, _, err := fetchConfig(context.Background(), st, tag); c != nil || err != nil || f.etagHits != 1 {
		t.Fatalf("304: %+v %v", c, err)
	}
	if _, _, err := fetchConfig(context.Background(), &State{KoraiURL: srv.URL}, ""); !isRefused(err) {
		t.Fatalf("refused: %v", err)
	}
}

func TestRunAllStampsTheMinute(t *testing.T) {
	rs := runAll(context.Background(), []Check{{ID: "x", Kind: "http", URL: "ftp://x"}, {ID: "y", Kind: "tcp"}}, 1200)
	if len(rs) != 2 || rs[0].Check != "x" || rs[1].Check != "y" || rs[0].Minute != 1200 || rs[1].Error != "invalid" {
		t.Fatalf("%+v", rs)
	}
}
