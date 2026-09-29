package swh

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func (s *Store) cook(ctx context.Context, id string) error {
	method := http.MethodPost
	var lastStatus string
	for {
		task, err := s.api.task(ctx, method, id)
		if err != nil {
			return err
		}
		if task.Status != lastStatus {
			log.Printf("%s: %s %s", id, task.Status, task.Progress)
			lastStatus = task.Status
		}
		switch task.Status {
		case "done":
			return nil
		case "new", "pending":
		case "failed":
			return fmt.Errorf("vault cooking failed: %s", task.Progress)
		default:
			return fmt.Errorf("unknown Vault status %q", task.Status)
		}
		timer := time.NewTimer(s.config.Poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		method = http.MethodGet
	}
}

func (s *Store) prepare(ctx context.Context, id string) error {
	if err := s.cook(ctx, id); err != nil {
		return err
	}
	resp, err := s.api.download(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	tmp, err := os.MkdirTemp(s.config.Cache, ".unpack-"+id+"-")
	if err != nil {
		return err
	}
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			log.Printf("remove staging directory: %v", err)
		}
	}()
	if err := extract(resp.Body, tmp, id+".git", s.config.MaxBytes); err != nil {
		return fmt.Errorf("extract bundle: %w", err)
	}
	repo := filepath.Join(tmp, id+".git")
	if err := s.validateBundleConfig(ctx, repo); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(repo, "config"), []byte("[core]\n\trepositoryformatversion = 0\n\tbare = true\n[http]\n\treceivepack = false\n"), fileMode); err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, s.git, "--git-dir="+repo, "rev-parse", "--is-bare-repository")
	cmd.Env = GitEnv()
	output, err := cmd.CombinedOutput()
	if err != nil || strings.TrimSpace(string(output)) != "true" {
		return fmt.Errorf("bundle does not contain a usable Git directory: %s (%v)", output, err)
	}
	if err := os.Rename(repo, filepath.Join(s.config.Cache, id+".git")); err != nil {
		return err
	}
	log.Printf("cached %s", id)
	return nil
}

func (s *Store) validateBundleConfig(ctx context.Context, repo string) error {
	check := exec.CommandContext(ctx, s.git, "config", "--file", filepath.Join(repo, "config"), "--no-includes", "--type=bool", "--get", "core.bare")
	check.Env = GitEnv()
	bare, err := check.CombinedOutput()
	if err != nil || strings.TrimSpace(string(bare)) != "true" {
		return fmt.Errorf("bundle config must declare core.bare=true: %s (%v)", bare, err)
	}
	check = exec.CommandContext(ctx, s.git, "config", "--file", filepath.Join(repo, "config"), "--no-includes", "--type=int", "--get", "core.repositoryformatversion")
	check.Env = GitEnv()
	version, err := check.CombinedOutput()
	if err != nil || strings.TrimSpace(string(version)) != "0" {
		return fmt.Errorf("unsupported bundle repository format: expected core.repositoryformatversion=0, got %q (%v)", strings.TrimSpace(string(version)), err)
	}
	return nil
}

func GitEnv() []string {
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "GIT_") || key == "GIT_PROTOCOL" || strings.HasPrefix(key, "GIT_TRACE") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull)
}
