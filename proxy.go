package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"strings"
	"time"

	"github.com/andrew/swh-git/internal/swh"
)

type proxy struct {
	store      *swh.Store
	cache, git string
	timeout    time.Duration
}

func newProxy(cfg swh.Config) (*proxy, error) {
	store, err := swh.New(cfg)
	if err != nil {
		return nil, err
	}
	return &proxy{store: store, cache: store.CacheDir(), git: store.GitPath(), timeout: cfg.Timeout}, nil
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
	if !swh.IsID(id) {
		p.serveOrigin(w, r.WithContext(ctx), name, suffix)
		return
	}
	if _, err := p.store.Ensure(ctx, id); err != nil {
		p.serveError(w, err)
		return
	}
	r = r.Clone(r.Context())
	r.URL.Path = "/" + id + ".git" + suffix
	r.URL.RawPath = ""
	backend := &cgi.Handler{
		Path: p.git, Args: []string{"-c", "http.receivepack=false", "http-backend"},
		Env: []string{"GIT_PROJECT_ROOT=" + p.cache, "GIT_HTTP_EXPORT_ALL=1", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull, "GIT_PROTOCOL=" + r.Header.Get("Git-Protocol")},
	}
	backend.ServeHTTP(w, r)
}

func (p *proxy) serveOrigin(w http.ResponseWriter, r *http.Request, name, suffix string) {
	id, err := p.store.Resolve(r.Context(), name)
	if err != nil {
		p.serveError(w, err)
		return
	}
	location := "/" + id + ".git" + suffix
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	w.Header().Set("Cache-Control", "no-store")
	http.Redirect(w, r, location, http.StatusTemporaryRedirect)
}

func (p *proxy) serveError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	var apiErr *swh.APIError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		status = http.StatusGatewayTimeout
	case errors.Is(err, swh.ErrInvalidAddress):
		status = http.StatusBadRequest
	case errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound:
		status = http.StatusNotFound
	}
	log.Print(err)
	http.Error(w, err.Error(), status)
}
