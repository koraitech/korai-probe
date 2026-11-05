package main

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Check is one uptime check as Korai sends it (GET /api/probe/v1/config).
type Check struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Kind            string  `json:"kind"` // http, tcp, adms
	URL             string  `json:"url,omitempty"`
	Method          string  `json:"method,omitempty"`
	Expect          [][]int `json:"expect,omitempty"`
	Keyword         string  `json:"keyword,omitempty"`
	TimeoutS        int     `json:"timeout_s"`
	FollowRedirects *bool   `json:"follow_redirects,omitempty"`
	VerifyTLS       *bool   `json:"verify_tls,omitempty"`
	HeaderName      string  `json:"header_name,omitempty"`
	HeaderValue     string  `json:"header_value,omitempty"`
	Host            string  `json:"host,omitempty"`
	Port            int     `json:"port,omitempty"`
}

// Result is one run of one check for one minute (POST /api/probe/v1/results). Never a response body.
type Result struct {
	Check    string `json:"check"`
	Minute   int64  `json:"minute"`
	OK       bool   `json:"ok"`
	Status   *int   `json:"status,omitempty"`
	MS       *int   `json:"ms,omitempty"`
	Error    string `json:"error,omitempty"`
	Attempts int    `json:"attempts"`
	TLSDays  *int   `json:"tls_days,omitempty"`
}

const (
	checkBudget  = 25 * time.Second // both attempts of one run, DNS included (the same budget as Korai's own runner)
	retryDelay   = 2 * time.Second
	maxBody      = 256 * 1024
	maxRedirects = 3
	userAgent    = "Korai-Probe/" + version + " (+https://korai.tech)"
)

// isLinkLocal: the cloud metadata services live there; a check never calls them, even inside the network.
func isLinkLocal(ip net.IP) bool {
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}
	return ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() ||
		ip.Equal(net.ParseIP("fd00:ec2::254"))
}

// dialer refuses link-local and metadata addresses at connect time (after DNS), whatever the name was.
func dialer(timeout time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	d := &net.Dialer{Timeout: timeout, KeepAlive: -1}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil {
			return nil, &net.DNSError{Err: "no such host", Name: host}
		}
		for _, a := range ips {
			if isLinkLocal(a.IP) {
				return nil, errBlocked
			}
		}
		if len(ips) == 0 {
			return nil, &net.DNSError{Err: "no such host", Name: host}
		}
		return d.DialContext(ctx, network, net.JoinHostPort(ips[0].IP.String(), port))
	}
}

var errBlocked = errors.New("blocked")

func expectOK(c Check, status int) bool {
	ranges := c.Expect
	if len(ranges) == 0 {
		ranges = [][]int{{200, 399}}
	}
	for _, r := range ranges {
		if len(r) == 2 && status >= r[0] && status <= r[1] {
			return true
		}
	}
	return false
}

func origin(u *url.URL) string {
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(u.Hostname()) + ":" + port
}

func classify(err error) string {
	var dnsErr *net.DNSError
	var certErr *tls.CertificateVerificationError
	var recErr tls.RecordHeaderError
	switch {
	case errors.Is(err, errBlocked):
		return "blocked"
	case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
		return "timeout"
	case errors.As(err, &dnsErr):
		return "dns"
	case errors.As(err, &certErr) || errors.As(err, &recErr) || strings.Contains(err.Error(), "tls:") ||
		strings.Contains(err.Error(), "x509:"):
		return "tls"
	case strings.Contains(err.Error(), "malformed HTTP"):
		return "protocol"
	}
	return "connect"
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func ms(d time.Duration) *int { v := int(d / time.Millisecond); return &v }

// probeHTTP: one attempt of an http or adms check, within the deadline of ctx.
func probeHTTP(ctx context.Context, c Check) Result {
	start := time.Now()
	r := Result{Check: c.ID, Attempts: 1}
	home, err := url.Parse(c.URL)
	if err != nil || (home.Scheme != "http" && home.Scheme != "https") || home.User != nil {
		r.Error = "invalid"
		return r
	}
	verify := c.VerifyTLS == nil || *c.VerifyTLS
	follow := c.FollowRedirects == nil || *c.FollowRedirects
	timeout := time.Duration(c.TimeoutS) * time.Second
	tr := &http.Transport{
		DialContext:           dialer(timeout),
		TLSClientConfig:       &tls.Config{InsecureSkipVerify: !verify, MinVersion: tls.VersionTLS12}, //nolint:gosec // the tenant's choice per check
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
		// No proxy: the probe runs inside the network it checks (a proxy setting is for reaching Korai).
	}
	defer tr.CloseIdleConnections()
	withSecret := c.HeaderName != "" && c.HeaderValue != ""
	client := &http.Client{Transport: tr, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if !follow {
			return http.ErrUseLastResponse
		}
		if len(via) > maxRedirects {
			return errTooMany
		}
		if withSecret {
			if home.Scheme == "https" && req.URL.Scheme == "http" {
				return errInsecure
			}
			if origin(req.URL) != origin(home) {
				// the token never goes to another origin: that answer is the result
				return http.ErrUseLastResponse
			}
		}
		if c.Method == "POST" && origin(req.URL) != origin(home) {
			return http.ErrUseLastResponse
		}
		return nil
	}}
	method := c.Method
	if method == "" {
		method = "GET"
	}
	req, err := http.NewRequestWithContext(ctx, method, c.URL, nil)
	if err != nil {
		r.Error = "invalid"
		return r
	}
	req.Header.Set("User-Agent", userAgent)
	if withSecret {
		req.Header.Set(c.HeaderName, c.HeaderValue)
	}
	resp, err := client.Do(req)
	r.MS = ms(time.Since(start))
	if err != nil {
		switch {
		case errors.Is(err, errTooMany):
			r.Error = "redirects"
		case errors.Is(err, errInsecure):
			r.Error = "insecure_redirect"
		default:
			r.Error = classify(err)
		}
		return r
	}
	defer resp.Body.Close()
	st := resp.StatusCode
	r.Status = &st
	if resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		d := int(time.Until(resp.TLS.PeerCertificates[0].NotAfter).Hours() / 24)
		r.TLSDays = &d
	}
	if !expectOK(c, st) {
		r.Error = "status"
		return r
	}
	if c.Keyword != "" {
		body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
		r.MS = ms(time.Since(start))
		if err != nil {
			r.Error = classify(err)
			return r
		}
		if !strings.Contains(string(body), c.Keyword) {
			r.Error = "keyword"
			return r
		}
	}
	if time.Since(start) > timeout {
		r.Error = "timeout"
		return r
	}
	r.OK = true
	return r
}

var (
	errTooMany  = errors.New("too many redirects")
	errInsecure = errors.New("https to http")
)

func probeTCP(ctx context.Context, c Check) Result {
	start := time.Now()
	r := Result{Check: c.ID, Attempts: 1}
	if c.Host == "" || c.Port < 1 || c.Port > 65535 {
		r.Error = "invalid"
		return r
	}
	conn, err := dialer(time.Duration(c.TimeoutS)*time.Second)(ctx, "tcp", net.JoinHostPort(c.Host, strconv.Itoa(c.Port)))
	r.MS = ms(time.Since(start))
	if err != nil {
		if errors.Is(err, errBlocked) {
			r.Error = "blocked"
		} else {
			r.Error = "connect" // like Korai: no refused-or-silent, no time
		}
		return r
	}
	_ = conn.Close()
	r.OK = true
	return r
}

// Run is one run of a check: up to two attempts a couple of seconds apart, all within checkBudget.
func Run(parent context.Context, c Check) Result {
	if c.TimeoutS < 1 || c.TimeoutS > 20 {
		c.TimeoutS = 10
	}
	deadline := time.Now().Add(checkBudget)
	one := func() Result {
		left := time.Until(deadline)
		t := time.Duration(c.TimeoutS) * time.Second
		if left < t {
			t = left
		}
		ctx, cancel := context.WithTimeout(parent, t)
		defer cancel()
		if c.Kind == "tcp" {
			return probeTCP(ctx, c)
		}
		return probeHTTP(ctx, c)
	}
	r := one()
	if r.OK || r.Error == "blocked" || r.Error == "invalid" || r.Error == "insecure_redirect" {
		return r
	}
	if time.Until(deadline)-retryDelay < time.Second {
		return r
	}
	select {
	case <-parent.Done():
		return r
	case <-time.After(retryDelay):
	}
	r2 := one()
	r2.Attempts = 2
	return r2
}

func describe(r Result) string {
	if r.OK {
		return fmt.Sprintf("up %dms", deref(r.MS))
	}
	return "down: " + r.Error
}

func deref(p *int) int {
	if p == nil {
		return 0
	}
	return *p
}
