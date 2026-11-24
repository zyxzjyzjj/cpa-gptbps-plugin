package auth

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Opt in on a machine with Chrome/Chromium/Edge. These tests use local fixture
// pages only: no real account, OAuth token, or upstream authorization request.
func TestBrowserCaptureLocalChromium(t *testing.T) {
	if os.Getenv("BPS_BROWSER_INTEGRATION") != "1" {
		t.Skip("set BPS_BROWSER_INTEGRATION=1 to exercise a local Chromium browser")
	}
	executable, err := browserExecutable(os.Getenv("BPS_TEST_BROWSER_PATH"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	for _, mode := range []string{"capture", "cancel", "wrong_frame", "proxy", "proxy_down"} {
		t.Run(mode, func(t *testing.T) {
			var callbackHits, loginHits atomic.Int64
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/login":
					loginHits.Add(1)
					w.Header().Set("Content-Type", "text/html")
					if mode == "cancel" {
						_, _ = w.Write([]byte("<html><body>Local authorization fixture</body></html>"))
					} else if mode == "wrong_frame" {
						_, _ = w.Write([]byte(`<html><body><iframe src="/callback?state=fixture-state&amp;code=fixture-code"></iframe></body></html>`))
					} else {
						http.Redirect(w, r, "/callback?state=fixture-state&code=fixture-code", http.StatusFound)
					}
				case "/callback":
					callbackHits.Add(1)
					w.WriteHeader(http.StatusInternalServerError)
				default:
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()
			var proxyHits atomic.Int64
			proxyAddress := ""
			if mode == "proxy" || mode == "proxy_down" {
				forward := &http.Transport{Proxy: nil}
				defer forward.CloseIdleConnections()
				proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					// This fixture never forwards traffic outside the local test server.
					if r.URL.Scheme+"://"+r.URL.Host != srv.URL {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					proxyHits.Add(1)
					req := r.Clone(r.Context())
					req.RequestURI = ""
					resp, err := forward.RoundTrip(req)
					if err != nil {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					defer resp.Body.Close()
					for k, values := range resp.Header {
						w.Header()[k] = values
					}
					w.WriteHeader(resp.StatusCode)
					_, _ = io.Copy(w, resp.Body)
				}))
				defer proxy.Close()
				proxyAddress = proxy.URL
				if mode == "proxy_down" {
					proxy.Close()
				}
			}
			profile := filepath.Join(t.TempDir(), "profile")
			if err := os.Mkdir(profile, 0700); err != nil {
				t.Fatal(err)
			}
			args := []string{"--headless=new", "--user-data-dir=" + profile, "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--no-first-run", "--no-default-browser-check", "--disable-background-networking", "--disable-component-update", "--disable-sync", "--disable-extensions", "--disable-quic", "--new-window", "about:blank"}
			proxyArgs, err := browserProxyArgs(proxyAddress)
			if err != nil {
				t.Fatal(err)
			}
			args = append(args, proxyArgs...)
			cmd := exec.Command(executable, args...)
			prepareBrowserProcess(cmd)
			if err := cmd.Start(); err != nil {
				t.Fatal("cannot start local browser fixture")
			}
			done := make(chan struct{})
			go func() { _ = cmd.Wait(); close(done) }()
			var cleanupOnce sync.Once
			cleanup := func() { cleanupOnce.Do(func() { stopBrowserProcess(cmd, done); removeBrowserProfile(profile) }) }
			t.Cleanup(cleanup)
			ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
			defer cancel()
			endpoint, err := waitBrowserPage(ctx, profile, done)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "cancel" {
				go func() {
					ticker := time.NewTicker(20 * time.Millisecond)
					defer ticker.Stop()
					for {
						select {
						case <-ctx.Done():
							return
						case <-ticker.C:
							if loginHits.Load() > 0 {
								cancel()
								return
							}
						}
					}
				}()
			}
			callback, err := captureBrowserPagePolicy(ctx, endpoint, srv.URL+"/login", srv.URL+"/callback", func(raw string, _ bool) bool {
				u, e := url.Parse(raw)
				return e == nil && u.Scheme+"://"+u.Host == srv.URL
			})
			if mode == "capture" || mode == "proxy" {
				if err != nil || callback != srv.URL+"/callback?state=fixture-state&code=fixture-code" {
					t.Fatal("local browser did not capture the expected callback", err)
				}
			} else if mode == "cancel" {
				if !errors.Is(err, context.Canceled) || callback != "" {
					t.Fatal("browser cancellation was not propagated", err)
				}
			} else if err == nil || callback != "" {
				t.Fatal("unexpected callback from failed authorization")
			}
			wantLoginHits := int64(1)
			if mode == "proxy_down" {
				wantLoginHits = 0
			}
			if callbackHits.Load() != 0 || loginHits.Load() != wantLoginHits {
				t.Fatal("callback reached the network or login was not opened", callbackHits.Load(), loginHits.Load())
			}
			if mode == "proxy" && proxyHits.Load() == 0 {
				t.Fatal("explicit browser proxy was bypassed")
			}
			cleanup()
			select {
			case <-done:
			default:
				t.Fatal("temporary browser is still running")
			}
			if _, err := os.Stat(profile); !os.IsNotExist(err) {
				t.Fatal("temporary browser profile remains")
			}
		})
	}
}
