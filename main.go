// Command korai-probe runs Korai uptime checks inside a private network and sends the results to Korai.
//
// The only setting is the install token Korai shows ("Add private probe"): KORAI_PROBE_TOKEN. It carries Korai's
// address, the tenant and the probe, so there is no configuration file. On its first start the probe trades the
// token for its own credential, kept in KORAI_PROBE_STATE_DIR (default /var/lib/korai-probe). It then downloads its
// checks every minute, runs them on the minute, and posts the results over outbound HTTPS. While Korai cannot be
// reached the results wait on disk (up to seven days) and are sent later, in order.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"
)

const version = "1.0.0"

func main() {
	log.SetFlags(log.LstdFlags | log.LUTC)
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("korai-probe", version)
		return
	}
	dir := os.Getenv("KORAI_PROBE_STATE_DIR")
	if dir == "" {
		dir = "/var/lib/korai-probe"
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	st, err := setup(ctx, dir, os.Getenv("KORAI_PROBE_TOKEN"), os.Getenv("KORAI_URL"))
	if err != nil {
		log.Printf("korai-probe %s: %v", version, err)
		os.Exit(1)
	}
	log.Printf("korai-probe %s (%s/%s): probe %s of %s, reporting to %s", version, runtime.GOOS, runtime.GOARCH,
		st.ProbeID, st.Tenant, st.KoraiURL)
	a, err := newAgent(dir, st)
	if err != nil {
		log.Printf("korai-probe: %v", err)
		os.Exit(1)
	}
	a.run(ctx)
	log.Printf("korai-probe: stopped")
}

// setup returns the probe's credential: the one on disk, or a new one traded for the token when the token is new.
func setup(ctx context.Context, dir, token, override string) (*State, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("cannot create the state folder %s: %w", dir, err)
	}
	st, _ := loadState(dir)
	if token == "" {
		if st == nil {
			return nil, fmt.Errorf("no install token: set KORAI_PROBE_TOKEN to the token Korai shows in Uptime checks > Private probes")
		}
		return withOverride(st, override), nil
	}
	if st != nil && st.TokenSHA == sha(token) {
		return withOverride(st, override), nil
	}
	tok, err := parseToken(token)
	if err != nil {
		return nil, err
	}
	if override != "" {
		tok.URL = override
	}
	host, _ := os.Hostname()
	delay := 5 * time.Second
	for {
		cred, err := enroll(ctx, tok, host)
		if err == nil {
			ns := &State{KoraiURL: tok.URL, Credential: cred, ProbeID: tok.Probe, Tenant: tok.Tenant, TokenSHA: sha(token)}
			if err := saveState(dir, ns); err != nil {
				return nil, fmt.Errorf("cannot save the credential in %s: %w", dir, err)
			}
			log.Printf("korai-probe: enrolled")
			return ns, nil
		}
		if isRefused(err) {
			if st != nil && st.ProbeID == tok.Probe {
				log.Printf("korai-probe: the install token was refused (%v); keeping the credential this probe already has", err)
				return withOverride(st, override), nil
			}
			return nil, err
		}
		log.Printf("korai-probe: cannot reach Korai to enroll (%v), retrying in %s", err, delay)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay < 5*time.Minute {
			delay *= 2
		}
	}
}

func withOverride(st *State, override string) *State {
	if override != "" {
		st.KoraiURL = override
	}
	return st
}
