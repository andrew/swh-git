package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxJSONBytes = 1 << 20

type apiError struct {
	status int
	path   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("Software Heritage %s: HTTP %d", e.path, e.status)
}

func (p *proxy) request(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.api+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "swh-git")
	if p.token != "" {
		req.Header.Set("Authorization", "Bearer "+p.token)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, &apiError{status: resp.StatusCode, path: path}
	}
	return resp, nil
}

func (p *proxy) getJSON(ctx context.Context, method, path string, result any) error {
	resp, err := p.request(ctx, method, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(result); err != nil {
		return fmt.Errorf("invalid JSON from Software Heritage (check API access): %w", err)
	}
	return nil
}

func (p *proxy) resolveOrigin(ctx context.Context, origin string) (string, error) {
	var visit struct{ Snapshot string }
	path := "origin/" + url.PathEscape(origin) + "/visit/latest/?require_snapshot=true"
	err := p.getJSON(ctx, http.MethodGet, path, &visit)
	var apiErr *apiError
	if errors.As(err, &apiErr) && apiErr.status == http.StatusNotFound && strings.HasSuffix(origin, ".git") {
		return p.resolveOrigin(ctx, strings.TrimSuffix(origin, ".git"))
	}
	if err != nil {
		return "", err
	}
	id := "swh:1:snp:" + visit.Snapshot
	if !swhID.MatchString(id) {
		return "", errors.New("origin has no valid archived snapshot")
	}
	return id, nil
}

func (p *proxy) cook(ctx context.Context, id string) error {
	path := "vault/git-bare/" + id + "/"
	method := http.MethodPost
	for {
		var task struct {
			Status   string `json:"status"`
			Progress string `json:"progress_message"`
		}
		if err := p.getJSON(ctx, method, path, &task); err != nil {
			return err
		}
		log.Printf("%s: %s %s", id, task.Status, task.Progress)
		switch task.Status {
		case "done":
			return nil
		case "new", "pending":
		case "failed":
			return fmt.Errorf("vault cooking failed: %s", task.Progress)
		default:
			return fmt.Errorf("unknown Vault status %q", task.Status)
		}
		timer := time.NewTimer(p.poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		method = http.MethodGet
	}
}

func (p *proxy) prepare(ctx context.Context, id string) error {
	if err := p.cook(ctx, id); err != nil {
		return err
	}
	resp, err := p.request(ctx, http.MethodGet, "vault/git-bare/"+id+"/raw/")
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	tmp, err := os.MkdirTemp(p.cache, ".unpack-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			log.Printf("remove staging directory: %v", err)
		}
	}()
	if err := extract(resp.Body, tmp, id+".git", p.maxBytes); err != nil {
		return fmt.Errorf("extract bundle: %w", err)
	}
	repo := filepath.Join(tmp, id+".git")
	if err := os.WriteFile(filepath.Join(repo, "config"), []byte("[core]\n\trepositoryformatversion = 0\n\tbare = true\n[http]\n\treceivepack = false\n"), fileMode); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, p.git, "--git-dir="+repo, "rev-parse", "--is-bare-repository")
	output, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "true" {
		return fmt.Errorf("bundle is not a bare Git repository: %s (%v)", output, err)
	}
	if err := os.Rename(repo, filepath.Join(p.cache, id+".git")); err != nil {
		return err
	}
	log.Printf("cached %s", id)
	return nil
}
