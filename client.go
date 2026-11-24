package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"runtime"
	"strconv"
	"time"
)

// api is the probe's only outbound connection: Korai, over the system proxy if there is one.
var api = &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment,
	TLSHandshakeTimeout: 10 * time.Second, IdleConnTimeout: 90 * time.Second}}

// apiError is an answer from Korai that is not a success.
type apiError struct {
	Status     int
	Detail     string
	RetryAfter time.Duration
}

func (e *apiError) Error() string {
	if e.Detail != "" {
		return fmt.Sprintf("Korai answered %d: %s", e.Status, e.Detail)
	}
	return fmt.Sprintf("Korai answered %d", e.Status)
}

// isRefused: Korai answered and said no (the token or credential is not valid). Anything else is worth retrying.
func isRefused(err error) bool {
	var ae *apiError
	return errors.As(err, &ae) && (ae.Status == 401 || ae.Status == 403)
}

func call(ctx context.Context, method, url, cred string, body any, hdr map[string]string) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("X-Korai-Probe-Version", version)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cred != "" {
		req.Header.Set("Authorization", "Bearer "+cred)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := api.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		ae := &apiError{Status: resp.StatusCode}
		var d struct {
			Detail any `json:"detail"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &d) == nil {
			if s, ok := d.Detail.(string); ok {
				ae.Detail = s
			}
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			ae.RetryAfter = time.Duration(s) * time.Second
		}
		return nil, ae
	}
	return resp, nil
}

func enroll(ctx context.Context, tok *Token, host string) (string, error) {
	resp, err := call(ctx, "POST", tok.URL+"/api/probe/v1/enroll", "", map[string]string{
		"token": tok.Raw, "hostname": host, "version": version, "platform": runtime.GOOS + "/" + runtime.GOARCH,
	}, nil)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Credential string `json:"credential"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil || out.Credential == "" {
		return "", fmt.Errorf("unexpected answer from Korai to enroll")
	}
	return out.Credential, nil
}

// Config is what Korai sends: the checks this probe runs.
type Config struct {
	Checks []Check `json:"checks"`
	ETag   string  `json:"etag"`
}

// fetchConfig returns nil, nil when the checks did not change (304).
func fetchConfig(ctx context.Context, st *State, etag string) (*Config, string, error) {
	hdr := map[string]string{}
	if etag != "" {
		hdr["If-None-Match"] = etag
	}
	resp, err := call(ctx, "GET", st.KoraiURL+"/api/probe/v1/config", st.Credential, nil, hdr)
	if err != nil {
		return nil, etag, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotModified {
		return nil, etag, nil
	}
	var c Config
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&c); err != nil {
		return nil, etag, fmt.Errorf("unexpected configuration from Korai: %w", err)
	}
	return &c, resp.Header.Get("ETag"), nil
}

func postResults(ctx context.Context, st *State, rs []Result, buffered int) error {
	if rs == nil {
		rs = []Result{}
	}
	resp, err := call(ctx, "POST", st.KoraiURL+"/api/probe/v1/results", st.Credential,
		map[string]any{"results": rs, "buffered": buffered, "version": version}, nil)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.Body.Close()
}
