// Package forge is how starci talks to the place the code lives: reading a
// push, fetching the script for a commit, and saying how the run went.
package forge

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

// Push is a branch being moved to a new commit.
type Push struct {
	Repo     string // owner/name
	Ref      string // the branch, without refs/heads/
	SHA      string
	CloneURL string
	// Changed is every file the pushed commits added, modified or removed.
	Changed []string
}

// Status is what a forge shows beside a commit.
type Status struct {
	State       string // pending, success, failure or error
	Description string
	URL         string
}

// StatusContext is the name the status appears under.
const StatusContext = "starci"

// ErrNotFound is returned by File when the commit has no such file.
var ErrNotFound = errors.New("not found")

// ErrIgnored is returned by ParsePush for a delivery that is genuine but is
// not a push to a branch: a tag, a deleted branch, some other event.
var ErrIgnored = errors.New("not a push to a branch")

// ErrSignature is returned by ParsePush for a delivery that does not prove it
// came from the forge.
var ErrSignature = errors.New("the delivery is not signed with the shared secret")

// Forge is one code host.
type Forge interface {
	Name() string
	// ParsePush checks that a webhook delivery really came from the forge,
	// using the secret shared with it, and reads the push out of it.
	ParsePush(header http.Header, body []byte, secret []byte) (Push, error)
	// File returns a file as it is at a commit.
	File(ctx context.Context, repo, sha, path string) ([]byte, error)
	SetStatus(ctx context.Context, repo, sha string, s Status) error
}

// Forgejo is a Forgejo or Gitea instance.
type Forgejo struct {
	BaseURL string // https://git.example.com
	Token   string
}

// GitHub is github.com.
type GitHub struct {
	Token string
	// API is the API's address; empty means https://api.github.com.
	API string
}

func (f Forgejo) Name() string { return "forgejo" }
func (g GitHub) Name() string  { return "github" }

// pushPayload is the part of a push delivery both forges agree on.
type pushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Repository struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
	Commits []struct {
		Added    []string `json:"added"`
		Modified []string `json:"modified"`
		Removed  []string `json:"removed"`
	} `json:"commits"`
}

func parsePush(event string, body []byte) (Push, error) {
	if event != "push" {
		return Push{}, ErrIgnored
	}
	var p pushPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return Push{}, fmt.Errorf("unreadable push: %w", err)
	}
	branch, isBranch := strings.CutPrefix(p.Ref, "refs/heads/")
	if !isBranch || p.Deleted || p.After == "" || strings.Trim(p.After, "0") == "" {
		return Push{}, ErrIgnored
	}
	if p.Repository.FullName == "" || p.Repository.CloneURL == "" {
		return Push{}, errors.New("push names no repository")
	}
	seen := map[string]bool{}
	var changed []string
	for _, c := range p.Commits {
		for _, list := range [][]string{c.Added, c.Modified, c.Removed} {
			for _, f := range list {
				if !seen[f] {
					seen[f] = true
					changed = append(changed, f)
				}
			}
		}
	}
	sort.Strings(changed)
	return Push{Repo: p.Repository.FullName, Ref: branch, SHA: p.After, CloneURL: p.Repository.CloneURL, Changed: changed}, nil
}

// signed reports whether signature is the hex HMAC-SHA256 of body under
// secret. A delivery is not believed without it: anyone can POST to a
// webhook address.
func signed(body, secret []byte, signature string) bool {
	if len(secret) == 0 || signature == "" {
		return false
	}
	want, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), want)
}

func (f Forgejo) ParsePush(h http.Header, body, secret []byte) (Push, error) {
	// Forgejo signs with its own header and Gitea's, and with GitHub's form
	// for compatibility. Any one of them matching is the same proof.
	hub := strings.TrimPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
	if !signed(body, secret, h.Get("X-Forgejo-Signature")) && !signed(body, secret, h.Get("X-Gitea-Signature")) && !signed(body, secret, hub) {
		return Push{}, ErrSignature
	}
	event := h.Get("X-Forgejo-Event")
	if event == "" {
		event = h.Get("X-Gitea-Event")
	}
	return parsePush(event, body)
}

func (g GitHub) ParsePush(h http.Header, body, secret []byte) (Push, error) {
	sig, ok := strings.CutPrefix(h.Get("X-Hub-Signature-256"), "sha256=")
	if !ok || !signed(body, secret, sig) {
		return Push{}, ErrSignature
	}
	return parsePush(h.Get("X-GitHub-Event"), body)
}

var client = &http.Client{Timeout: 30 * time.Second}

func call(ctx context.Context, method, address string, header map[string]string, body any) ([]byte, int, error) {
	var payload io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, err
		}
		payload = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, address, payload)
	if err != nil {
		return nil, 0, err
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return data, resp.StatusCode, err
}

func segments(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

func (f Forgejo) api(path string) string { return strings.TrimRight(f.BaseURL, "/") + "/api/v1" + path }
func (f Forgejo) auth() map[string]string {
	return map[string]string{"Authorization": "token " + f.Token}
}

func (f Forgejo) File(ctx context.Context, repo, sha, path string) ([]byte, error) {
	data, code, err := call(ctx, "GET", f.api("/repos/"+segments(repo)+"/raw/"+segments(path)+"?ref="+url.QueryEscape(sha)), f.auth(), nil)
	return fileResult(f, repo, path, data, code, err)
}

func (f Forgejo) SetStatus(ctx context.Context, repo, sha string, s Status) error {
	data, code, err := call(ctx, "POST", f.api("/repos/"+segments(repo)+"/statuses/"+url.PathEscape(sha)), f.auth(), statusBody(s))
	return statusResult(f, repo, data, code, err)
}

func (g GitHub) api(path string) string {
	base := g.API
	if base == "" {
		base = "https://api.github.com"
	}
	return strings.TrimRight(base, "/") + path
}

func (g GitHub) auth(accept string) map[string]string {
	return map[string]string{"Authorization": "Bearer " + g.Token, "Accept": accept, "X-GitHub-Api-Version": "2022-11-28"}
}

func (g GitHub) File(ctx context.Context, repo, sha, path string) ([]byte, error) {
	data, code, err := call(ctx, "GET", g.api("/repos/"+segments(repo)+"/contents/"+segments(path)+"?ref="+url.QueryEscape(sha)), g.auth("application/vnd.github.raw+json"), nil)
	return fileResult(g, repo, path, data, code, err)
}

func (g GitHub) SetStatus(ctx context.Context, repo, sha string, s Status) error {
	data, code, err := call(ctx, "POST", g.api("/repos/"+segments(repo)+"/statuses/"+url.PathEscape(sha)), g.auth("application/vnd.github+json"), statusBody(s))
	return statusResult(g, repo, data, code, err)
}

func statusBody(s Status) map[string]string {
	desc := s.Description
	// Both forges cut descriptions off; doing it here keeps the ending tidy.
	if len(desc) > 140 {
		desc = desc[:137] + "..."
	}
	body := map[string]string{"state": s.State, "description": desc, "context": StatusContext}
	if s.URL != "" {
		body["target_url"] = s.URL
	}
	return body
}

func fileResult(f Forge, repo, path string, data []byte, code int, err error) ([]byte, error) {
	switch {
	case err != nil:
		return nil, fmt.Errorf("%s: fetching %s from %s: %w", f.Name(), path, repo, err)
	case code == http.StatusNotFound:
		return nil, ErrNotFound
	case code != http.StatusOK:
		return nil, fmt.Errorf("%s: fetching %s from %s: HTTP %d: %s", f.Name(), path, repo, code, brief(data))
	}
	return data, nil
}

func statusResult(f Forge, repo string, data []byte, code int, err error) error {
	if err != nil {
		return fmt.Errorf("%s: setting a status on %s: %w", f.Name(), repo, err)
	}
	if code < 200 || code > 299 {
		return fmt.Errorf("%s: setting a status on %s: HTTP %d: %s", f.Name(), repo, code, brief(data))
	}
	return nil
}

func brief(data []byte) string {
	s := strings.TrimSpace(string(data))
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}

// GitEnv returns environment variables that make git authenticate to the
// given forges when it fetches from them, for repositories that are not
// public. The credentials travel as git configuration in the environment:
// they are in no command line and no URL.
func GitEnv(forges ...Forge) []string {
	var keys, values []string
	for _, f := range forges {
		switch f := f.(type) {
		case Forgejo:
			if f.Token != "" {
				keys = append(keys, "http."+strings.TrimRight(f.BaseURL, "/")+"/.extraheader")
				values = append(values, "Authorization: token "+f.Token)
			}
		case GitHub:
			if f.Token != "" {
				basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + f.Token))
				keys = append(keys, "http.https://github.com/.extraheader")
				values = append(values, "Authorization: Basic "+basic)
			}
		}
	}
	env := []string{fmt.Sprintf("GIT_CONFIG_COUNT=%d", len(keys))}
	for i := range keys {
		env = append(env, fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, keys[i]), fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, values[i]))
	}
	if len(keys) == 0 {
		return nil
	}
	return env
}
