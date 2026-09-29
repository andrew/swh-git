package main

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/andrew/swh-git/internal/swh"
)

func TestLateHTTPWaiterRetriesPreparationTimeout(t *testing.T) {
	bundle, _ := fixtureBundle(t, true)
	started := make(chan struct{})
	var posts atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/raw/") {
			_, _ = w.Write(bundle)
			return
		}
		if r.Method == http.MethodPost && posts.Add(1) == 1 {
			close(started)
			<-r.Context().Done()
			return
		}
		_, _ = io.WriteString(w, `{"status":"done"}`)
	}))
	defer api.Close()
	p := testProxy(t, api.URL, t.TempDir(), func(cfg *swh.Config) { cfg.Timeout = 2 * time.Second })
	first := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
		close(done)
	}()
	await(t, started)
	time.Sleep(time.Second)
	second := httptest.NewRecorder()
	p.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
	await(t, done)
	if first.Code != http.StatusGatewayTimeout {
		t.Fatalf("first status=%d: %s", first.Code, first.Body)
	}
	if second.Code != http.StatusOK || posts.Load() != 2 {
		t.Fatalf("late waiter status=%d, attempts=%d: %s", second.Code, posts.Load(), second.Body)
	}
	if !strings.Contains(second.Body.String(), "# service=git-upload-pack") {
		t.Fatalf("missing Git advertisement: %s", second.Body)
	}
}

func TestStoreCloseStopsHTTPPreparation(t *testing.T) {
	started := make(chan struct{})
	var posts atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		if posts.Add(1) == 1 {
			close(started)
		}
		<-r.Context().Done()
	}))
	defer api.Close()
	p := testProxy(t, api.URL, t.TempDir())
	response := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		p.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
		close(done)
	}()
	await(t, started)
	p.store.Close()
	await(t, done)
	afterClose := httptest.NewRecorder()
	p.ServeHTTP(afterClose, httptest.NewRequest(http.MethodGet, "/"+testID+".git/info/refs?service=git-upload-pack", nil))
	for _, result := range []*httptest.ResponseRecorder{response, afterClose} {
		if result.Code != http.StatusBadGateway || !strings.Contains(result.Body.String(), "context canceled") {
			t.Fatalf("closed store status=%d: %s", result.Code, result.Body)
		}
	}
	if posts.Load() != 1 {
		t.Fatalf("closed store restarted preparation: %d attempts", posts.Load())
	}
}

func TestHelperRejectsUnsupportedRepositoryFormat(t *testing.T) {
	bundle, _ := fixtureBundle(t, true, "sha256")
	api := bundleAPI(t, bundle)
	cache := t.TempDir()
	output, err := remoteCmd(t.Context(), api.URL, cache, "ls-remote", "swh::"+testID).CombinedOutput()
	if err == nil || !bytes.Contains(output, []byte("unsupported bundle repository format")) {
		t.Fatalf("unsupported format: %v: %s", err, output)
	}
	if _, err := os.Stat(filepath.Join(cache, testID+".git")); !os.IsNotExist(err) {
		t.Fatalf("unsupported repository was cached: %v", err)
	}
}

func TestHelperLogsVaultStatusChanges(t *testing.T) {
	bundle, _ := fixtureBundle(t, true)
	var polls atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/raw/") {
			_, _ = w.Write(bundle)
			return
		}
		status := "pending"
		switch polls.Add(1) {
		case 1:
			status = "new"
		case 5:
			status = "done"
		}
		_, _ = fmt.Fprintf(w, `{"status":%q,"progress_message":"poll %d"}`, status, polls.Load())
	}))
	defer api.Close()
	output := remoteRun(t, api.URL, t.TempDir(), "ls-remote", "swh::"+testID, "HEAD")
	for _, status := range []string{"new", "pending", "done"} {
		if count := strings.Count(output, testID+": "+status+" "); count != 1 {
			t.Errorf("logged %s %d times: %s", status, count, output)
		}
	}
	if polls.Load() != 5 {
		t.Fatalf("polls=%d, want 5", polls.Load())
	}
}
