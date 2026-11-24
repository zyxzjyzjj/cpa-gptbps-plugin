package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A same-origin iframe shares the main page's CDP target and is insufficient to
// test interception. This fixture loads localhost inside 127.0.0.1, then makes
// that separate renderer navigate back to the callback after its script runs.
func TestBrowserCaptureCrossSiteFrame(t *testing.T) {
	if os.Getenv("BPS_BROWSER_INTEGRATION") != "1" {
		t.Skip("set BPS_BROWSER_INTEGRATION=1 to exercise a local Chromium browser")
	}
	executable, err := browserExecutable(os.Getenv("BPS_TEST_BROWSER_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	var callbackHits, frameHits atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		switch r.URL.Path {
		case "/login":
			_, _ = fmt.Fprintf(w, `<html><body><iframe src="%s/frame"></iframe></body></html>`, strings.Replace(server.URL, "127.0.0.1", "localhost", 1))
		case "/frame":
			frameHits.Add(1)
			_, _ = fmt.Fprintf(w, `<html><script>setTimeout(() => { location.href = "%s/callback?state=fixture-state&code=fixture-code"; }, 100)</script></html>`, server.URL)
		case "/callback":
			callbackHits.Add(1)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	profile := filepath.Join(t.TempDir(), "profile")
	if err := os.Mkdir(profile, 0700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(executable, "--headless=new", "--user-data-dir="+profile, "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--no-proxy-server", "--disable-extensions", "--new-window", "about:blank")
	prepareBrowserProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal("cannot start local browser fixture")
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { stopBrowserProcess(cmd, done) }) }
	t.Cleanup(func() { stop(); removeBrowserProfile(profile) })
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	endpoint, err := waitBrowserPage(ctx, profile, done)
	if err != nil {
		t.Fatal(err)
	}
	callback, err := captureBrowserPageLifecycle(ctx, endpoint, server.URL+"/login", server.URL+"/callback", func(raw string, _ bool) bool {
		u, e := url.Parse(raw)
		return e == nil && (u.Scheme+"://"+u.Host == server.URL || u.Scheme+"://"+u.Host == strings.Replace(server.URL, "127.0.0.1", "localhost", 1))
	}, stop)
	if err == nil || !strings.Contains(err.Error(), "outside the main browser page") || callback != "" {
		t.Fatal("cross-site iframe callback was not rejected by interception", err)
	}
	if callbackHits.Load() != 0 || frameHits.Load() != 1 {
		t.Fatal("cross-site callback reached the network or fixture frame did not load", callbackHits.Load(), frameHits.Load())
	}
	select {
	case <-done:
	default:
		t.Fatal("temporary browser was not stopped")
	}
}
