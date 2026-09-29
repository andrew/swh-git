package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/andrew/swh-git/internal/swh"
)

var helperBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "swh-helper-test-")
	if err != nil {
		panic(err)
	}
	helperBin = filepath.Join(dir, "git-remote-swh")
	output, err := exec.Command("go", "build", "-race", "-o", helperBin, "./cmd/git-remote-swh").CombinedOutput()
	if err != nil {
		panic(fmt.Sprintf("build helper: %v: %s", err, output))
	}
	code := m.Run()
	if err := os.RemoveAll(dir); err != nil {
		panic(err)
	}
	os.Exit(code)
}

func remoteCmd(ctx context.Context, api, cache string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = append(os.Environ(),
		"PATH="+filepath.Dir(helperBin)+string(os.PathListSeparator)+os.Getenv("PATH"),
		"SWH_API="+api+"/api/1/", "SWH_CACHE="+cache, "SWH_TOKEN=test-token",
		"SWH_TIMEOUT=5s", "SWH_POLL=1ms", "SWH_MAX_BYTES=33554432",
		"GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	return cmd
}

func remoteRun(t *testing.T, api, cache string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	output, err := remoteCmd(ctx, api, cache, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return string(output)
}

func bundleAPI(t *testing.T, bundle []byte) *httptest.Server {
	t.Helper()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/raw/") {
			_, _ = w.Write(bundle)
			return
		}
		_, _ = io.WriteString(w, `{"status":"done"}`)
	}))
	t.Cleanup(api.Close)
	return api
}

func TestRemoteHelperProtocol(t *testing.T) {
	for _, tc := range []struct {
		name, input, env, wantError, wantOutput string
		args                                    []string
	}{
		{name: "capabilities", args: []string{testID}, input: "capabilities\n\n", env: "SWH_TIMEOUT=invalid", wantOutput: "connect\n\n"},
		{name: "missing address", input: "capabilities\n", wantError: "usage:"},
		{name: "unsupported", args: []string{"origin", testID}, input: "list\n", wantError: "unsupported helper command"},
		{name: "long command", args: []string{testID}, input: strings.Repeat("x", 8192), wantError: "helper command too long"},
		{name: "read only", args: []string{testID}, input: "connect git-receive-pack\n", wantError: "read-only"},
		{name: "invalid ID", args: []string{"swh:1:snp:invalid"}, input: "connect git-upload-pack\n", wantError: "invalid Software Heritage address"},
		{name: "timeout", args: []string{testID}, input: "connect git-upload-pack\n", env: "SWH_TIMEOUT=invalid", wantError: "SWH_TIMEOUT"},
		{name: "poll", args: []string{testID}, input: "connect git-upload-pack\n", env: "SWH_POLL=invalid", wantError: "SWH_POLL"},
		{name: "size", args: []string{testID}, input: "connect git-upload-pack\n", env: "SWH_MAX_BYTES=invalid", wantError: "SWH_MAX_BYTES"},
		{name: "positive limit", args: []string{testID}, input: "connect git-upload-pack\n", env: "SWH_MAX_BYTES=0", wantError: "must be positive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, helperBin, tc.args...)
			cmd.Env = remoteCmd(ctx, "http://127.0.0.1:1", t.TempDir()).Env
			if tc.env != "" {
				cmd.Env = append(cmd.Env, tc.env)
			}
			cmd.Stdin = strings.NewReader(tc.input)
			var stdout, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &stdout, &stderr
			err := cmd.Run()
			if (err != nil) != (tc.wantError != "") || !strings.Contains(stderr.String(), tc.wantError) {
				t.Fatalf("error=%v, stderr=%q, want %q", err, &stderr, tc.wantError)
			}
			if stdout.String() != tc.wantOutput {
				t.Fatalf("protocol output=%q, want %q", &stdout, tc.wantOutput)
			}
		})
	}
}

func TestRemoteHelper(t *testing.T) {
	bundle, head := fixtureBundle(t, true)
	var cooks, downloads, origins atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Error("missing API token")
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/1/origin/"):
			origins.Add(1)
			_, _ = fmt.Fprintf(w, `{"snapshot":%q}`, strings.TrimPrefix(testID, "swh:1:snp:"))
		case strings.HasSuffix(r.URL.Path, "/raw/"):
			downloads.Add(1)
			_, _ = w.Write(bundle)
		default:
			cooks.Add(1)
			_, _ = io.WriteString(w, `{"status":"done"}`)
		}
	}))
	defer api.Close()
	cache := t.TempDir()
	for _, address := range []string{"swh::" + testID, "swh::" + testID + ".git", "swh::example.org/team/repo", "swh::https://example.org/team/repo", "swh://" + testID} {
		t.Run(address, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "clone")
			remoteRun(t, api.URL, cache, "clone", address, dest)
			assertClone(t, dest, head)
			if got := gitRun(t, dest, "remote", "get-url", "origin"); got != address {
				t.Fatalf("remote = %q", got)
			}
			remoteRun(t, api.URL, cache, "-C", dest, "fetch", "origin")
		})
	}
	if cooks.Load() != 1 || downloads.Load() != 1 || origins.Load() != 4 {
		t.Fatalf("cooks=%d downloads=%d origins=%d", cooks.Load(), downloads.Load(), origins.Load())
	}
	refs := remoteRun(t, api.URL, cache, "ls-remote", "--symref", "swh::"+testID, "HEAD", "refs/heads/*", "refs/tags/*")
	for _, ref := range []string{"ref: refs/heads/main\tHEAD", head + "\trefs/heads/other", head + "\trefs/tags/example"} {
		if !strings.Contains(refs, ref) {
			t.Fatalf("missing %q in %s", ref, refs)
		}
	}
	dest := filepath.Join(t.TempDir(), "shallow")
	remoteRun(t, api.URL, cache, "clone", "--depth=1", "swh::"+testID, dest)
	if got := gitRun(t, dest, "rev-list", "--count", "HEAD"); got != "1" {
		t.Fatalf("shallow count=%s", got)
	}
	remoteRun(t, api.URL, cache, "-C", dest, "fetch", "--unshallow")
	if got := gitRun(t, dest, "rev-list", "--count", "HEAD"); got != "2" {
		t.Fatalf("full count=%s", got)
	}
	output, err := remoteCmd(t.Context(), "http://127.0.0.1:1", cache, "-C", dest, "push", "swh::"+testID, "HEAD:refs/heads/rejected").CombinedOutput()
	if err == nil || !strings.Contains(string(output), "read-only") {
		t.Fatalf("push: %v: %s", err, output)
	}
	refs = gitRun(t, "", "--git-dir="+filepath.Join(cache, testID+".git"), "show-ref")
	if strings.Contains(refs, "rejected") {
		t.Fatal("push changed refs")
	}
	api.Close()
	offline := filepath.Join(t.TempDir(), "offline")
	remoteRun(t, api.URL, cache, "clone", "swh::"+testID, offline)
	assertClone(t, offline, head)
}

func TestHelperAndHTTPShareColdCache(t *testing.T) {
	bundle, _ := fixtureBundle(t, true)
	started, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	var cooks, downloads atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/raw/") {
			if downloads.Add(1) == 1 {
				close(started)
			}
			select {
			case <-release:
				_, _ = w.Write(bundle)
			case <-r.Context().Done():
			}
			return
		}
		cooks.Add(1)
		_, _ = io.WriteString(w, `{"status":"done"}`)
	}))
	defer api.Close()
	cache := t.TempDir()
	first := remoteCmd(t.Context(), api.URL, cache, "ls-remote", "swh::"+testID, "HEAD")
	var firstOutput bytes.Buffer
	first.Stdout, first.Stderr = &firstOutput, &firstOutput
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	await(t, started)
	second := remoteCmd(t.Context(), api.URL, cache, "ls-remote", "swh::"+testID, "HEAD")
	var secondOutput bytes.Buffer
	second.Stdout, second.Stderr = &secondOutput, &secondOutput
	if err := second.Start(); err != nil {
		t.Fatal(err)
	}
	p := testProxy(t, api.URL, cache)
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
		close(done)
	}()
	time.Sleep(150 * time.Millisecond)
	unblock()
	if err := first.Wait(); err != nil {
		t.Fatalf("first helper: %v: %s", err, &firstOutput)
	}
	if err := second.Wait(); err != nil {
		t.Fatalf("second helper: %v: %s", err, &secondOutput)
	}
	await(t, done)
	if response.Code != http.StatusOK {
		t.Fatalf("HTTP status %d: %s", response.Code, response.Body)
	}
	if cooks.Load() != 1 || downloads.Load() != 1 {
		t.Fatalf("cooks=%d downloads=%d", cooks.Load(), downloads.Load())
	}
}

func await(t *testing.T, ready <-chan struct{}) {
	t.Helper()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for operation")
	}
}

func TestCancelledHTTPRequestKeepsSharedDownload(t *testing.T) {
	bundle, _ := fixtureBundle(t, true)
	started, release := make(chan struct{}), make(chan struct{})
	var downloads atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/raw/") {
			_, _ = io.WriteString(w, `{"status":"done"}`)
			return
		}
		if downloads.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			_, _ = w.Write(bundle)
		case <-r.Context().Done():
		}
	}))
	defer api.Close()
	p := testProxy(t, api.URL, t.TempDir())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	firstDone := make(chan struct{})
	go func() {
		p.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil).WithContext(ctx))
		close(firstDone)
	}()
	await(t, started)
	cancel()
	await(t, firstDone)
	response := httptest.NewRecorder()
	secondDone := make(chan struct{})
	go func() {
		p.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
		close(secondDone)
	}()
	close(release)
	await(t, secondDone)
	if response.Code != http.StatusOK || downloads.Load() != 1 {
		t.Fatalf("status=%d downloads=%d: %s", response.Code, downloads.Load(), response.Body)
	}
}

func TestFailedPreparationSharedWithHTTPWaiters(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	var posts atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if posts.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
			_, _ = io.WriteString(w, `{"status":"failed","progress_message":"missing object"}`)
		case <-r.Context().Done():
		}
	}))
	defer api.Close()
	p := testProxy(t, api.URL, t.TempDir())
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			response := httptest.NewRecorder()
			p.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
			if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "missing object") {
				t.Errorf("status=%d: %s", response.Code, response.Body)
			}
		})
	}
	await(t, started)
	time.Sleep(150 * time.Millisecond)
	close(release)
	wg.Wait()
	if posts.Load() != 1 {
		t.Fatalf("cooking attempts=%d, want 1", posts.Load())
	}
}

func TestBundleSizeBoundary(t *testing.T) {
	bundle, _ := fixtureBundle(t, false)
	for _, compressed := range []bool{false, true} {
		body := bundle
		if compressed {
			var buf bytes.Buffer
			gz := gzip.NewWriter(&buf)
			if _, err := gz.Write(bundle); err != nil {
				t.Fatal(err)
			}
			if err := gz.Close(); err != nil {
				t.Fatal(err)
			}
			body = buf.Bytes()
		}
		api := bundleAPI(t, body)
		for _, extra := range []int64{-1, 0, 1} {
			t.Run(fmt.Sprintf("gzip=%t/extra=%d", compressed, extra), func(t *testing.T) {
				p := testProxy(t, api.URL, t.TempDir(), func(cfg *swh.Config) { cfg.MaxBytes = int64(len(bundle)) + extra })
				response := httptest.NewRecorder()
				p.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
				expected := http.StatusOK
				if extra < 0 {
					expected = http.StatusBadGateway
				}
				if response.Code != expected {
					t.Fatalf("status=%d, want %d: %s", response.Code, expected, response.Body)
				}
			})
		}
	}
}

func TestBundleRequiresOriginalBareConfig(t *testing.T) {
	bundle, _ := fixtureBundle(t, false)
	var buf bytes.Buffer
	reader := tar.NewReader(bytes.NewReader(bundle))
	writer := tar.NewWriter(&buf)
	for {
		header, err := reader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		body, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == testID+".git/config" {
			body = bytes.ReplaceAll(body, []byte("bare = true"), []byte("bare = false"))
			header.Size = int64(len(body))
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write(body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	api := bundleAPI(t, buf.Bytes())
	p := testProxy(t, api.URL, t.TempDir())
	response := httptest.NewRecorder()
	p.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
	if response.Code != http.StatusBadGateway || !strings.Contains(response.Body.String(), "core.bare=true") {
		t.Fatalf("status=%d: %s", response.Code, response.Body)
	}
}

func TestHelperInterruptedPreparation(t *testing.T) {
	for _, signal := range []os.Signal{syscall.SIGTERM, syscall.SIGKILL} {
		t.Run(signal.String(), func(t *testing.T) {
			testInterruptedPreparation(t, signal)
		})
	}
}

func testInterruptedPreparation(t *testing.T, signal os.Signal) {
	bundle, _ := fixtureBundle(t, false)
	started := make(chan struct{})
	var downloads atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/raw/") {
			_, _ = io.WriteString(w, `{"status":"done"}`)
			return
		}
		if downloads.Add(1) == 1 {
			_, _ = w.Write(bundle[:1024])
			w.(http.Flusher).Flush()
			close(started)
			<-r.Context().Done()
			return
		}
		_, _ = w.Write(bundle)
	}))
	defer api.Close()
	cache := t.TempDir()
	cmd := exec.CommandContext(t.Context(), helperBin, "origin", testID)
	cmd.Env = remoteCmd(t.Context(), api.URL, cache).Env
	cmd.Stdin = strings.NewReader("capabilities\nconnect git-upload-pack\n")
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	await(t, started)
	staging := awaitStaging(t, cache)
	if err := cmd.Process.Signal(signal); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatalf("interrupted helper succeeded: %s", &output)
	}
	if signal == syscall.SIGTERM {
		if _, err := os.Stat(staging); !os.IsNotExist(err) {
			t.Fatalf("staging survived cancellation: %v", err)
		}
	}
	other := filepath.Join(cache, ".unpack-swh:1:snp:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-active")
	if err := os.Mkdir(other, 0700); err != nil {
		t.Fatal(err)
	}
	remoteRun(t, api.URL, cache, "ls-remote", "swh::"+testID, "HEAD")
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatalf("abandoned staging survived retry: %v", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("another SWHID's staging was removed: %v", err)
	}
}

func awaitStaging(t *testing.T, cache string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		staging, err := filepath.Glob(filepath.Join(cache, ".unpack-"+testID+"-*"))
		if err != nil {
			t.Fatal(err)
		}
		if len(staging) == 1 {
			return staging[0]
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("expected an active staging directory")
	return ""
}
