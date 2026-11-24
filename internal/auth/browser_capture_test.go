package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestBrowserCaptureProtocol(t *testing.T) {
	for _, mode := range []string{"success", "iframe_callback", "oopif_callback", "nested_oopif_callback", "wrong_host", "wrong_path", "fragment", "enable_failed", "attach_failed", "cancel", "cancel_paused"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			var methods []string
			var closed bool
			callback := BrowserRedirect + "?code=fixture-code&state=fixture-state"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer conn.CloseNow()
				ctx := context.Background()
				write := func(value any) bool {
					raw, _ := json.Marshal(value)
					return conn.Write(ctx, websocket.MessageText, raw) == nil
				}
				paused := func(session, id, link, frame string) bool {
					return write(map[string]any{"sessionId": session, "method": "Fetch.requestPaused", "params": map[string]any{"requestId": id, "frameId": frame, "request": map[string]string{"url": link}}})
				}
				attached := func(parent, session string) bool {
					return write(map[string]any{"sessionId": parent, "method": "Target.attachedToTarget", "params": map[string]any{"sessionId": session, "waitingForDebugger": true, "targetInfo": map[string]string{"type": "iframe"}}})
				}
				var navigateID int64
				for {
					_, raw, err := conn.Read(ctx)
					if err != nil {
						return
					}
					var request struct {
						ID        int64          `json:"id"`
						SessionID string         `json:"sessionId"`
						Method    string         `json:"method"`
						Params    map[string]any `json:"params"`
					}
					if json.Unmarshal(raw, &request) != nil {
						t.Error("bad request")
						return
					}
					label := request.Method
					if request.SessionID != "" {
						label = request.SessionID + "/" + label
					}
					mu.Lock()
					methods = append(methods, label)
					mu.Unlock()
					result := map[string]any{}
					reply := func() bool {
						return write(map[string]any{"id": request.ID, "sessionId": request.SessionID, "result": result})
					}
					switch request.Method {
					case "Page.getFrameTree":
						result["frameTree"] = map[string]any{"frame": map[string]string{"id": "main"}}
					case "Fetch.enable":
						if mode == "enable_failed" {
							write(map[string]any{"id": request.ID, "error": map[string]any{"code": -1, "message": "private fixture information"}})
							continue
						}
						patterns, _ := request.Params["patterns"].([]any)
						if len(patterns) != 1 {
							t.Error("interceptor was not installed")
						}
					case "Target.setAutoAttach":
						if request.Params["autoAttach"] != true || request.Params["waitForDebuggerOnStart"] != true || request.Params["flatten"] != true {
							t.Error("child frame was not paused before interception")
						}
						if mode == "attach_failed" {
							write(map[string]any{"id": request.ID, "error": map[string]any{"code": -1}})
							continue
						}
					case "Page.navigate":
						navigateID = request.ID
						if !paused("", "entry", "https://auth.openai.com/oauth/authorize", "main") {
							return
						}
						continue // Navigation replies only after Fetch resumes its request.
					case "Fetch.continueRequest":
						if request.Params["requestId"] != "entry" || request.SessionID != "" {
							t.Error("callback escaped interception")
						}
						if !reply() || !write(map[string]any{"id": navigateID, "result": map[string]string{"frameId": "main"}}) {
							return
						}
						if mode == "cancel" {
							continue
						}
						if mode == "oopif_callback" || mode == "nested_oopif_callback" {
							if !attached("", "child-session") {
								return
							}
							continue
						}
						link, frame := callback, "main"
						switch mode {
						case "iframe_callback":
							frame = "child"
						case "wrong_host":
							link = "https://elsewhere.invalid/callback?code=fixture-code"
						case "wrong_path":
							link = strings.Replace(callback, "/auth/callback", "/wrong/callback", 1)
						case "fragment":
							link += "#extra"
						}
						if !paused("", "callback", link, frame) {
							return
						}
						continue
					case "Runtime.runIfWaitingForDebugger":
						if request.SessionID == "" {
							t.Error("resumed an untracked target")
						}
						if !reply() {
							return
						}
						if mode == "nested_oopif_callback" && request.SessionID == "child-session" {
							if !attached(request.SessionID, "grandchild-session") {
								return
							}
						} else if !paused(request.SessionID, "callback", callback, "child-frame") {
							return
						}
						continue
					case "Fetch.failRequest":
						if request.Params["requestId"] != "callback" || request.Params["errorReason"] != "Aborted" {
							t.Error("callback was not explicitly aborted")
						}
						expectedSession := ""
						if mode == "oopif_callback" {
							expectedSession = "child-session"
						}
						if mode == "nested_oopif_callback" {
							expectedSession = "grandchild-session"
						}
						if request.SessionID != expectedSession {
							t.Error("callback abort was sent to the wrong target")
						}
						if mode == "cancel_paused" {
							continue
						} // Await caller cancellation without acknowledging abort.
					case "Browser.close":
						if request.SessionID != "" {
							t.Error("browser close must use the root session")
						}
						mu.Lock()
						closed = true
						mu.Unlock()
						reply()
						return
					}
					if !reply() {
						return
					}
				}
			}))
			defer server.Close()
			budget := 2 * time.Second
			if mode == "cancel" || mode == "cancel_paused" {
				budget = 150 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), budget)
			defer cancel()
			returned, err := captureBrowserPage(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), "https://auth.openai.com/oauth/authorize", BrowserRedirect)
			if mode == "success" {
				if err != nil || returned != callback {
					t.Fatalf("capture failed: %v", err)
				}
			} else if err == nil || returned != "" {
				t.Fatal("invalid callback was accepted")
			}
			if err != nil && (strings.Contains(err.Error(), "fixture-code") || strings.Contains(err.Error(), "private fixture")) {
				t.Fatal("error leaked browser details")
			}
			mu.Lock()
			observed := append([]string(nil), methods...)
			wasClosed := closed
			mu.Unlock()
			if !wasClosed {
				t.Fatal("control socket detached before Browser.close")
			}
			if len(observed) < 3 || !reflect.DeepEqual(observed[:3], []string{"Page.enable", "Page.getFrameTree", "Fetch.enable"}) {
				t.Fatalf("unsafe setup order: %v", observed)
			}
			if mode == "enable_failed" || mode == "attach_failed" {
				for _, method := range observed {
					if method == "Page.navigate" {
						t.Fatal("opened login without complete interception")
					}
				}
			} else if len(observed) < 6 || observed[3] != "Target.setAutoAttach" || observed[4] != "Page.navigate" || observed[5] != "Fetch.continueRequest" {
				t.Fatalf("unexpected navigation order: %v", observed)
			}
			if mode == "oopif_callback" || mode == "nested_oopif_callback" {
				expected := []string{"child-session/Fetch.enable", "child-session/Target.setAutoAttach", "child-session/Runtime.runIfWaitingForDebugger"}
				if len(observed) < 9 || !reflect.DeepEqual(observed[6:9], expected) {
					t.Fatalf("child frame resumed before interception: %v", observed)
				}
				if mode == "nested_oopif_callback" {
					expected = []string{"grandchild-session/Fetch.enable", "grandchild-session/Target.setAutoAttach", "grandchild-session/Runtime.runIfWaitingForDebugger"}
					if len(observed) < 12 || !reflect.DeepEqual(observed[9:12], expected) {
						t.Fatalf("nested frame resumed before interception: %v", observed)
					}
				}
			}
		})
	}
}

func TestBrowserProxyArguments(t *testing.T) {
	for _, test := range []struct {
		address, argument string
		failure           bool
	}{
		{"", "--no-proxy-server", false},
		{"http://127.0.0.1:7890", "--proxy-server=http://127.0.0.1:7890", false},
		{"https://proxy.example:8443", "--proxy-server=https://proxy.example:8443", false},
		{"socks5h://127.0.0.1:1080", "--proxy-server=socks5://127.0.0.1:1080", false},
		{"http://user:private-password@127.0.0.1:7890", "", true},
		{"http://proxy.example:8080;direct://", "", true},
		{"socks4://127.0.0.1:1080", "", true},
		{"http://proxy.example:8080/path", "", true},
		{"http://:8080", "", true},
		{"http://proxy.example:8080?", "", true},
		{"http://proxy.example:8080#", "", true},
	} {
		args, err := browserProxyArgs(test.address)
		if test.failure {
			if err == nil || strings.Contains(err.Error(), "private-password") {
				t.Fatal("invalid proxy accepted or secret leaked")
			}
		} else if err != nil || len(args) == 0 || args[0] != test.argument {
			t.Fatalf("unexpected proxy arguments: %v, %v", args, err)
		}
	}
}

func TestBrowserCaptureCanceledBeforeLaunch(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := CaptureBrowserAuthorization(ctx, "https://auth.openai.com/oauth/authorize", "", "/nonexistent-browser")
	if err != context.Canceled {
		t.Fatalf("canceled login attempted to initialize browser: %v", err)
	}
}

func TestBrowserDocumentBoundaries(t *testing.T) {
	for _, test := range []struct {
		link              string
		embedded, allowed bool
	}{
		{"https://auth.openai.com/log-in", false, true},
		{"https://chatgpt.com/", false, true},
		{"https://sentinel.openai.com/frame", true, true},
		{"https://sentinel.openai.com/frame", false, false},
		{"http://auth.openai.com/", false, false},
		{"https://auth.openai.com.evil.invalid/", false, false},
		{"https://auth.openai.com@evil.invalid/", false, false},
		{"https://user@auth.openai.com/", false, false},
		{"https://auth.openai.com:443/", false, false},
	} {
		if browserDocumentAllowed(test.link, test.embedded) != test.allowed {
			t.Errorf("document policy mismatch for %q", test.link)
		}
	}
}

func TestBrowserPageDiscoveryIgnoresEnvironmentProxy(t *testing.T) {
	var socket string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/json/list" {
			t.Error("unexpected discovery request")
		}
		json.NewEncoder(w).Encode([]map[string]string{{"type": "page", "url": "about:blank", "webSocketDebuggerUrl": socket}})
	}))
	defer server.Close()
	socket = "ws" + strings.TrimPrefix(server.URL, "http") + "/devtools/page/owned"
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	profile := t.TempDir()
	port := strings.Split(strings.TrimPrefix(server.URL, "http://"), ":")[1]
	if err := os.WriteFile(filepath.Join(profile, "DevToolsActivePort"), []byte(port+"\n/devtools/browser/owned\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	endpoint, err := waitBrowserPage(ctx, profile, make(chan struct{}))
	if err != nil || endpoint != socket {
		t.Fatalf("local discovery failed: %v", err)
	}
}

func TestBrowserOwnedProcessCleanup(t *testing.T) {
	if os.Getenv("BPS_BROWSER_PROCESS_HELPER") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestBrowserOwnedProcessCleanup$")
	cmd.Env = append(os.Environ(), "BPS_BROWSER_PROCESS_HELPER=1")
	prepareBrowserProcess(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	stopBrowserProcess(cmd, done)
	select {
	case <-done:
	default:
		t.Fatal("owned browser process was not reaped")
	}
	profile, err := os.MkdirTemp("", "bps-test-profile-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profile, "fixture"), []byte("private fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	removeBrowserProfile(profile)
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("temporary profile was retained")
	}
}
