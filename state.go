package main

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// State is what the probe keeps between starts. The file is readable by its owner only.
type State struct {
	KoraiURL   string `json:"korai_url"`
	Credential string `json:"credential"`
	ProbeID    string `json:"probe_id"`
	Tenant     string `json:"tenant"`
	TokenSHA   string `json:"token_sha256"` // which install token this credential came from: a new token re-enrolls
}

const stateFile = "credential.json"

func sha(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }

func loadState(dir string) (*State, error) {
	b, err := os.ReadFile(filepath.Join(dir, stateFile))
	if err != nil {
		return nil, err
	}
	var st State
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	if st.Credential == "" || st.KoraiURL == "" {
		return nil, errors.New("incomplete state")
	}
	return &st, nil
}

func saveState(dir string, st *State) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(dir, stateFile), b)
}

// writeAtomic writes a file readable by its owner only, replacing the old one in a single step.
func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Token is an install token: kpe1.<base64url Korai URL>.<base64url tenant>.<probe id>.<secret>.
type Token struct {
	URL, Tenant, Probe, Raw string
}

var (
	probeID  = regexp.MustCompile(`^pr-[a-z0-9]{10}$`)
	tenantRe = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,80}$`)
	errToken = errors.New("KORAI_PROBE_TOKEN is not a Korai install token: copy it again from Uptime checks > Private probes")
)

func parseToken(s string) (*Token, error) {
	s = strings.TrimSpace(s)
	p := strings.Split(s, ".")
	if len(p) != 5 || p[0] != "kpe1" || len(p[4]) < 20 || !probeID.MatchString(p[3]) {
		return nil, errToken
	}
	u, err1 := base64.RawURLEncoding.DecodeString(p[1])
	t, err2 := base64.RawURLEncoding.DecodeString(p[2])
	if err1 != nil || err2 != nil || !tenantRe.Match(t) ||
		!(strings.HasPrefix(string(u), "https://") || strings.HasPrefix(string(u), "http://")) {
		return nil, errToken
	}
	return &Token{URL: strings.TrimRight(string(u), "/"), Tenant: string(t), Probe: p[3], Raw: s}, nil
}
