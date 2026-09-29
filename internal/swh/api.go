package swh

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

const maxJSONBytes = 1 << 20

type apiClient struct {
	base, token string
	http        *http.Client
}

type APIError struct {
	Status int
	path   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("Software Heritage %s: HTTP %d", e.path, e.Status)
}

type vaultTask struct {
	Status   string `json:"status"`
	Progress string `json:"progress_message"`
}

func (a *apiClient) request(ctx context.Context, method, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "swh-git")
	if a.token != "" {
		req.Header.Set("Authorization", "Bearer "+a.token)
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, &APIError{Status: resp.StatusCode, path: path}
	}
	return resp, nil
}

func (a *apiClient) getJSON(ctx context.Context, method, path string, result any) error {
	resp, err := a.request(ctx, method, path)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxJSONBytes)).Decode(result); err != nil {
		return fmt.Errorf("invalid JSON from Software Heritage (check API access): %w", err)
	}
	return nil
}

func (a *apiClient) latestSnapshot(ctx context.Context, origin string) (string, error) {
	var visit struct{ Snapshot string }
	err := a.getJSON(ctx, http.MethodGet, "origin/"+url.PathEscape(origin)+"/visit/latest/?require_snapshot=true", &visit)
	return visit.Snapshot, err
}

func (a *apiClient) task(ctx context.Context, method, id string) (vaultTask, error) {
	var task vaultTask
	err := a.getJSON(ctx, method, "vault/git-bare/"+id+"/", &task)
	return task, err
}

func (a *apiClient) download(ctx context.Context, id string) (*http.Response, error) {
	return a.request(ctx, http.MethodGet, "vault/git-bare/"+id+"/raw/")
}
