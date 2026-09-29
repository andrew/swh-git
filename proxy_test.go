package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrew/swh-git/internal/swh"
)

const testID = "swh:1:snp:0123456789012345678901234567890123456789"

func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func fixtureBundle(t *testing.T, compressed bool) ([]byte, string) {
	t.Helper()
	dir := t.TempDir()
	gitRun(t, dir, "init", "--initial-branch=main")
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from the archive\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "hello.txt")
	gitRun(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.org", "-c", "commit.gpgsign=false", "commit", "-m", "Initial")
	gitRun(t, dir, "-c", "user.name=Test", "-c", "user.email=test@example.org", "-c", "commit.gpgsign=false", "commit", "--allow-empty", "-m", "Second")
	gitRun(t, dir, "branch", "other")
	gitRun(t, dir, "tag", "example")
	head := gitRun(t, dir, "rev-parse", "HEAD")
	bare := filepath.Join(t.TempDir(), "repo.git")
	gitRun(t, dir, "clone", "--bare", dir, bare)
	var buf bytes.Buffer
	var output io.Writer = &buf
	if compressed {
		gz := gzip.NewWriter(&buf)
		output = gz
	}
	tw := tar.NewWriter(output)
	err := filepath.WalkDir(bare, func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		header, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(bare, name)
		if err != nil {
			return err
		}
		header.Name = testID + ".git/" + filepath.ToSlash(rel)
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(name)
		if err != nil {
			return err
		}
		_, err = tw.Write(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if gz, ok := output.(*gzip.Writer); ok {
		if err := gz.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return buf.Bytes(), head
}

func testProxy(t *testing.T, api, cache string, configure ...func(*swh.Config)) *proxy {
	t.Helper()
	cfg := swh.Config{API: api + "/api/1/", Cache: cache, Timeout: 5 * time.Second, Poll: time.Millisecond, MaxBytes: 32 << 20}
	for _, configure := range configure {
		configure(&cfg)
	}
	p, err := newProxy(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.store.Close)
	return p
}

func TestCloneAndPersistentCache(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		t.Run(fmt.Sprintf("gzip=%t", compressed), func(t *testing.T) {
			bundle, head := fixtureBundle(t, compressed)
			var posts, polls, downloads atomic.Int32
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/api/1/vault/git-bare/"+testID+"/raw/":
					downloads.Add(1)
					_, _ = w.Write(bundle)
				case r.Method == http.MethodPost:
					posts.Add(1)
					_, _ = io.WriteString(w, `{"status":"new"}`)
				default:
					status := "pending"
					if polls.Add(1) > 1 {
						status = "done"
					}
					_ = json.NewEncoder(w).Encode(map[string]string{"status": status})
				}
			}))
			defer api.Close()
			cache := t.TempDir()
			server := httptest.NewServer(testProxy(t, api.URL, cache))
			defer server.Close()
			dest := filepath.Join(t.TempDir(), "clone")
			gitRun(t, "", "clone", server.URL+"/"+testID+".git", dest)
			assertClone(t, dest, head)
			gitRun(t, dest, "fetch", "origin")
			api.Close()
			restarted := httptest.NewServer(testProxy(t, api.URL, cache))
			defer restarted.Close()
			second := filepath.Join(t.TempDir(), "clone")
			gitRun(t, "", "-c", "protocol.version=0", "clone", restarted.URL+"/"+testID+".git", second)
			assertClone(t, second, head)
			if posts.Load() != 1 || polls.Load() != 2 || downloads.Load() != 1 {
				t.Fatalf("posts=%d polls=%d downloads=%d", posts.Load(), polls.Load(), downloads.Load())
			}
		})
	}
}

func assertClone(t *testing.T, dir, head string) {
	t.Helper()
	if got := gitRun(t, dir, "rev-parse", "HEAD"); got != head {
		t.Fatalf("HEAD = %s, want %s", got, head)
	}
	content, err := os.ReadFile(filepath.Join(dir, "hello.txt"))
	if err != nil || string(content) != "hello from the archive\n" {
		t.Fatalf("checkout: %q, %v", content, err)
	}
	for _, ref := range []string{"refs/remotes/origin/other", "refs/tags/example"} {
		if got := gitRun(t, dir, "rev-parse", ref); got != head {
			t.Fatalf("%s = %s, want %s", ref, got, head)
		}
	}
}

func TestOriginClone(t *testing.T) {
	bundle, head := fixtureBundle(t, true)
	var origins atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/1/origin/https://example.org/team/repo.git/visit/latest/":
			origins.Add(1)
			http.NotFound(w, r)
		case "/api/1/origin/https://example.org/team/repo/visit/latest/":
			origins.Add(1)
			if r.URL.Query().Get("require_snapshot") != "true" {
				t.Error("missing require_snapshot")
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"snapshot": strings.TrimPrefix(testID, "swh:1:snp:")})
		case "/api/1/vault/git-bare/" + testID + "/":
			_, _ = io.WriteString(w, `{"status":"done"}`)
		case "/api/1/vault/git-bare/" + testID + "/raw/":
			_, _ = w.Write(bundle)
		default:
			t.Errorf("unexpected API request %s", r.URL)
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	server := httptest.NewServer(testProxy(t, api.URL, t.TempDir()))
	defer server.Close()
	for _, origin := range []string{"example.org/team/repo.git", "https://example.org/team/repo.git"} {
		dest := filepath.Join(t.TempDir(), "clone")
		gitRun(t, "", "clone", server.URL+"/"+origin, dest)
		assertClone(t, dest, head)
	}
	if origins.Load() != 4 {
		t.Fatalf("origin requests = %d, want 4", origins.Load())
	}
}

func TestRejectedRequests(t *testing.T) {
	p := testProxy(t, "http://127.0.0.1:1", t.TempDir())
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/" + testID + ".git/info/refs?service=git-receive-pack"},
		{http.MethodPost, "/" + testID + ".git/git-receive-pack"},
		{http.MethodGet, "/" + testID + ".git/config"},
		{http.MethodGet, "/swh:1:cnt:0123456789012345678901234567890123456789.git/info/refs?service=git-upload-pack"},
		{http.MethodGet, "/swh:1:snp:bad.git/info/refs?service=git-upload-pack"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest(tc.method, tc.path, nil))
			if w.Code != http.StatusForbidden && w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body)
			}
		})
	}
}

func TestVaultFailures(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		upstream, expected int
	}{
		{"failed", `{"status":"failed","progress_message":"missing objects"}`, 200, 502},
		{"unknown", `{"status":"surprise"}`, 200, 502},
		{"html", `<html>challenge</html>`, 200, 502},
		{"missing", `{}`, 404, 404},
		{"rate limit", `{}`, 429, 502},
		{"timeout", `{"status":"pending"}`, 200, 504},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.upstream)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer api.Close()
			p := testProxy(t, api.URL, t.TempDir(), func(cfg *swh.Config) { cfg.Timeout = 30 * time.Millisecond })
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
			if w.Code != tc.expected {
				t.Fatalf("status=%d, body=%s", w.Code, w.Body)
			}
			entries, err := os.ReadDir(p.cache)
			if err != nil || len(entries) != 1 || entries[0].Name() != ".locks" {
				t.Fatalf("failed request left cache entries: %v, %v", entries, err)
			}
		})
	}
}

func TestConcurrentRequests(t *testing.T) {
	bundle, _ := fixtureBundle(t, true)
	var posts atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/raw/") {
			_, _ = w.Write(bundle)
			return
		}
		posts.Add(1)
		_, _ = io.WriteString(w, `{"status":"done"}`)
	}))
	defer api.Close()
	p := testProxy(t, api.URL, t.TempDir())
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
			if w.Code != http.StatusOK {
				t.Errorf("status=%d: %s", w.Code, w.Body)
			}
		})
	}
	wg.Wait()
	if posts.Load() != 1 {
		t.Fatalf("cooking requests=%d, want 1", posts.Load())
	}
}

func TestUnsafeBundles(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header tar.Header
		limit  int64
	}{
		{"traversal", tar.Header{Name: "../outside", Typeflag: tar.TypeReg}, 1 << 20},
		{"absolute", tar.Header{Name: "/tmp/outside", Typeflag: tar.TypeReg}, 1 << 20},
		{"symlink", tar.Header{Name: testID + ".git/objects", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}, 1 << 20},
		{"hardlink", tar.Header{Name: testID + ".git/HEAD", Typeflag: tar.TypeLink, Linkname: "/etc/passwd"}, 1 << 20},
		{"alternates", tar.Header{Name: testID + ".git/objects/info/alternates", Typeflag: tar.TypeReg}, 1 << 20},
		{"alternates-mixed", tar.Header{Name: testID + ".git/objects/info/Alternates", Typeflag: tar.TypeReg}, 1 << 20},
		{"alternates-upper", tar.Header{Name: testID + ".git/OBJECTS/INFO/ALTERNATES", Typeflag: tar.TypeReg}, 1 << 20},
		{"alternates-http", tar.Header{Name: testID + ".git/objects/info/HTTP-Alternates", Typeflag: tar.TypeReg}, 1 << 20},
		{"commondir", tar.Header{Name: testID + ".git/CommonDir", Typeflag: tar.TypeReg}, 1 << 20},
		{"size", tar.Header{Name: testID + ".git/HEAD", Typeflag: tar.TypeReg, Size: 1024}, 600},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			if err := tw.WriteHeader(&tc.header); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write(make([]byte, tc.header.Size)); err != nil {
				t.Fatal(err)
			}
			if err := tw.Close(); err != nil {
				t.Fatal(err)
			}
			api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/raw/") {
					_, _ = w.Write(buf.Bytes())
					return
				}
				_, _ = io.WriteString(w, `{"status":"done"}`)
			}))
			defer api.Close()
			p := testProxy(t, api.URL, t.TempDir(), func(cfg *swh.Config) { cfg.MaxBytes = tc.limit })
			w := httptest.NewRecorder()
			p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
			if w.Code != http.StatusBadGateway {
				t.Fatalf("status=%d: %s", w.Code, w.Body)
			}
			if strings.HasPrefix(tc.name, "alternates") && !strings.Contains(w.Body.String(), "external object alternates") {
				t.Fatalf("alternates entry reached repository validation: %s", w.Body)
			}
			if tc.name == "commondir" && !strings.Contains(w.Body.String(), "external common directory") {
				t.Fatalf("commondir entry reached repository validation: %s", w.Body)
			}
			entries, err := os.ReadDir(p.cache)
			if err != nil || len(entries) != 1 || entries[0].Name() != ".locks" {
				t.Fatalf("unsafe bundle left cache entries: %v, %v", entries, err)
			}
		})
	}
}

func TestCorruptGzip(t *testing.T) {
	bundle, _ := fixtureBundle(t, true)
	bundle[len(bundle)-1] ^= 0xff
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/raw/") {
			_, _ = w.Write(bundle)
			return
		}
		_, _ = io.WriteString(w, `{"status":"done"}`)
	}))
	defer api.Close()
	p := testProxy(t, api.URL, t.TempDir())
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
	if w.Code != http.StatusBadGateway || !strings.Contains(w.Body.String(), "invalid checksum") {
		t.Fatalf("status=%d: %s", w.Code, w.Body)
	}
}

func TestDownloadRedirectDoesNotForwardToken(t *testing.T) {
	bundle, _ := fixtureBundle(t, true)
	storage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("API token leaked to storage server")
		}
		_, _ = w.Write(bundle)
	}))
	defer storage.Close()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing API token")
		}
		if strings.HasSuffix(r.URL.Path, "/raw/") {
			http.Redirect(w, r, storage.URL, http.StatusFound)
			return
		}
		_, _ = io.WriteString(w, `{"status":"done"}`)
	}))
	defer api.Close()
	p := testProxy(t, api.URL, t.TempDir(), func(cfg *swh.Config) { cfg.Token = "test-token" })
	w := httptest.NewRecorder()
	p.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d: %s", w.Code, w.Body)
	}
}
