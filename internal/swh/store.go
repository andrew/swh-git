package swh

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

const maxRedirects = 10

var (
	swhID             = regexp.MustCompile(`^swh:1:(snp|rev|rel|dir):[0-9a-f]{40}$`)
	ErrInvalidAddress = errors.New("invalid Software Heritage address")
)

type Store struct {
	api      *apiClient
	config   Config
	git      string
	mu       sync.Mutex
	flights  map[string]*preparation
	lifetime context.Context
	stop     context.CancelFunc
	workers  sync.WaitGroup
}

type preparation struct {
	done chan struct{}
	repo string
	err  error
}

func New(cfg Config) (*Store, error) {
	u, err := url.Parse(cfg.API)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return nil, errors.New("api must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if cfg.Timeout <= 0 || cfg.Poll <= 0 || cfg.MaxBytes <= 0 {
		return nil, errors.New("timeout, poll, and max-bytes must be positive")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, err
	}
	cfg.Cache, err = filepath.Abs(cfg.Cache)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.Cache, dirMode); err != nil {
		return nil, err
	}
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= maxRedirects {
			return errors.New("too many redirects")
		}
		if req.URL.Host != via[0].URL.Host || req.URL.Scheme != via[0].URL.Scheme {
			req.Header.Del("Authorization")
		}
		return nil
	}}
	lifetime, stop := context.WithCancel(context.Background())
	return &Store{config: cfg, git: git, flights: make(map[string]*preparation), lifetime: lifetime, stop: stop, api: &apiClient{
		base: strings.TrimRight(cfg.API, "/") + "/", token: cfg.Token, http: client,
	}}, nil
}

func (s *Store) CacheDir() string { return s.config.Cache }
func (s *Store) GitPath() string  { return s.git }

func IsID(id string) bool { return swhID.MatchString(id) }

func (s *Store) Resolve(ctx context.Context, address string) (string, error) {
	address = strings.TrimPrefix(address, "swh://")
	id := strings.TrimSuffix(address, ".git")
	if IsID(id) {
		return id, nil
	}
	if address == "" || strings.HasPrefix(address, "swh:") {
		return "", fmt.Errorf("%w: expected a core snp, rev, rel, or dir SWHID", ErrInvalidAddress)
	}
	origin := address
	if !strings.Contains(origin, "://") {
		origin = "https://" + origin
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.Path == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "https" && u.Scheme != "http") {
		return "", fmt.Errorf("%w: expected an HTTP(S) origin URL or host/path", ErrInvalidAddress)
	}
	return s.resolveOrigin(ctx, origin)
}

func (s *Store) resolveOrigin(ctx context.Context, origin string) (string, error) {
	snapshot, err := s.api.latestSnapshot(ctx, origin)
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound && strings.HasSuffix(origin, ".git") {
		return s.resolveOrigin(ctx, strings.TrimSuffix(origin, ".git"))
	}
	if err != nil {
		return "", err
	}
	id := "swh:1:snp:" + snapshot
	if !IsID(id) {
		return "", errors.New("origin has no valid archived snapshot")
	}
	log.Printf("origin %s resolved to %s", origin, id)
	return id, nil
}

func (s *Store) Close() {
	s.mu.Lock()
	s.stop()
	s.mu.Unlock()
	s.workers.Wait()
}

func (s *Store) Ensure(ctx context.Context, id string) (string, error) {
	if !IsID(id) {
		return "", fmt.Errorf("%w: invalid SWHID", ErrInvalidAddress)
	}
	repo := filepath.Join(s.config.Cache, id+".git")
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		if ready, err := cached(repo); ready || err != nil {
			return repo, err
		}
		s.mu.Lock()
		if err := s.lifetime.Err(); err != nil {
			s.mu.Unlock()
			return "", err
		}
		flight, exists := s.flights[id]
		if !exists {
			flight = &preparation{done: make(chan struct{})}
			s.flights[id] = flight
			s.workers.Add(1)
			go s.prepareShared(id, flight)
		}
		s.mu.Unlock()
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-flight.done:
			if exists && errors.Is(flight.err, context.DeadlineExceeded) {
				continue
			}
			return flight.repo, flight.err
		}
	}
}

func (s *Store) prepareShared(id string, flight *preparation) {
	defer s.workers.Done()
	ctx, cancel := context.WithTimeout(s.lifetime, s.config.Timeout)
	defer cancel()
	flight.repo, flight.err = s.ensureLocked(ctx, id)
	if flight.err != nil && ctx.Err() != nil {
		flight.err = ctx.Err()
	}
	s.mu.Lock()
	delete(s.flights, id)
	close(flight.done)
	s.mu.Unlock()
}

func (s *Store) ensureLocked(ctx context.Context, id string) (string, error) {
	lock, err := acquireLock(ctx, s.config.Cache, id)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := lock.Close(); err != nil {
			log.Printf("close cache lock: %v", err)
		}
	}()
	if err := s.cleanStaging(id); err != nil {
		return "", err
	}
	repo := filepath.Join(s.config.Cache, id+".git")
	if ready, err := cached(repo); ready || err != nil {
		return repo, err
	}
	if err := s.prepare(ctx, id); err != nil {
		return "", err
	}
	return repo, nil
}

func (s *Store) cleanStaging(id string) error {
	matches, err := filepath.Glob(filepath.Join(s.config.Cache, ".unpack-"+id+"-*"))
	if err != nil {
		return err
	}
	for _, path := range matches {
		if err := os.RemoveAll(path); err != nil {
			return err
		}
	}
	return nil
}

func cached(repo string) (bool, error) {
	info, err := os.Stat(repo)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.IsDir() {
		return false, fmt.Errorf("cache path is not a directory: %s", repo)
	}
	return true, nil
}
