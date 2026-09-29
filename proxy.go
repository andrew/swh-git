package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/cgi"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

var swhID = regexp.MustCompile(`^swh:1:(snp|rev|rel|dir):[0-9a-f]{40}$`)

const maxRedirects = 10

type proxy struct {
	api, cache, token, git string
	client                 *http.Client
	timeout, poll          time.Duration
	maxBytes               int64
	mu                     sync.Mutex
	preparing              map[string]chan struct{}
}

func newProxy(api, cache, token string, timeout, poll time.Duration, maxBytes int64) (*proxy, error) {
	u, err := url.Parse(api)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("api must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if timeout <= 0 || poll <= 0 || maxBytes <= 0 {
		return nil, errors.New("timeout, poll, and max-bytes must be positive")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	cache, err = filepath.Abs(cache)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cache, dirMode); err != nil {
		return nil, err
	}
	return &proxy{
		api: strings.TrimRight(api, "/") + "/", cache: cache, token: token, git: git,
		timeout: timeout, poll: poll, maxBytes: maxBytes,
		client: &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return errors.New("too many redirects")
			}
			if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
				req.Header.Del("Authorization")
			}
			return nil
		}},
		preparing: make(map[string]chan struct{}),
	}, nil
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var suffix string
	switch {
	case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/info/refs") && r.URL.Query().Get("service") == "git-upload-pack":
		suffix = "/info/refs"
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/git-upload-pack"):
		suffix = "/git-upload-pack"
	default:
		http.Error(w, "only Git clone and fetch are supported", http.StatusForbidden)
		return
	}
	name := strings.TrimPrefix(strings.TrimSuffix(r.URL.Path, suffix), "/")
	id := strings.TrimSuffix(name, ".git")
	ctx, cancel := context.WithTimeout(r.Context(), p.timeout)
	defer cancel()
	if !swhID.MatchString(id) {
		p.serveOrigin(w, r.WithContext(ctx), name, suffix)
		return
	}
	if err := p.ensureRepo(ctx, id); err != nil {
		p.serveError(w, err)
		return
	}
	r = r.Clone(r.Context())
	r.URL.Path = "/" + id + ".git" + suffix
	r.URL.RawPath = ""
	backend := &cgi.Handler{
		Path: p.git, Args: []string{"-c", "http.receivepack=false", "http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + p.cache, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_PROTOCOL=" + r.Header.Get("Git-Protocol")},
	}
	backend.ServeHTTP(w, r)
}

func (p *proxy) serveOrigin(w http.ResponseWriter, r *http.Request, name, suffix string) {
	if strings.HasPrefix(name, "swh:") || name == "" {
		http.Error(w, "expected a core snp, rev, rel, or dir SWHID", http.StatusBadRequest)
		return
	}
	origin := name
	if !strings.Contains(origin, "://") {
		origin = "https://" + origin
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Path == "" || u.User != nil || (u.Scheme != "https" && u.Scheme != "http") {
		http.Error(w, "expected an HTTP(S) origin URL or host/path", http.StatusBadRequest)
		return
	}
	id, err := p.resolveOrigin(r.Context(), origin)
	if err != nil {
		p.serveError(w, err)
		return
	}
	log.Printf("origin %s resolved to %s", origin, id)
	location := "/" + id + ".git" + suffix
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, location, http.StatusTemporaryRedirect)
}

func (p *proxy) serveError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var apiErr *apiError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound:
		status = http.StatusNotFound
	}
	log.Print(err)
	http.Error(w, err.Error(), status)
}

func (p *proxy) ensureRepo(ctx context.Context, id string) error {
	for {
		p.mu.Lock()
		if _, err := os.Stat(filepath.Join(p.cache, id+".git")); err == nil {
			p.mu.Unlock()
			return nil
		}
		wait, busy := p.preparing[id]
		if !busy {
			wait = make(chan struct{})
			p.preparing[id] = wait
		}
		p.mu.Unlock()
		if !busy {
			err := p.prepare(ctx, id)
			p.mu.Lock()
			delete(p.preparing, id)
			close(wait)
			p.mu.Unlock()
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for %s: %w", id, ctx.Err())
		case <-wait:
		}
	}
}
