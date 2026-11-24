package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"
)

// CaptureBrowserAuthorization opens a temporary, isolated Chromium profile.
// It captures the registered callback before network delivery. It never attaches
// to an existing browser or reads an existing browser profile.
func CaptureBrowserAuthorization(ctx context.Context, authorizeURL, proxyAddress, executablePath string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	u, err := url.Parse(authorizeURL)
	if err != nil || u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != "/oauth/authorize" || u.RawPath != "" || u.User != nil || strings.Contains(authorizeURL, "#") {
		return "", errors.New("invalid browser authorization URL")
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	executable, err := browserExecutable(executablePath)
	if err != nil {
		return "", err
	}
	proxyArgs, err := browserProxyArgs(proxyAddress)
	if err != nil {
		return "", err
	}
	profile, err := os.MkdirTemp("", "bps-login-")
	if err != nil {
		return "", errors.New("cannot create temporary browser profile")
	}
	defer removeBrowserProfile(profile)
	args := []string{"--user-data-dir=" + profile, "--remote-debugging-address=127.0.0.1", "--remote-debugging-port=0", "--no-first-run", "--no-default-browser-check", "--disable-sync", "--disable-background-networking", "--disable-component-update", "--disable-quic", "--force-webrtc-ip-handling-policy=disable_non_proxied_udp", "--disable-extensions", "--new-window", "about:blank"}
	args = append(args, proxyArgs...)
	cmd := exec.Command(executable, args...)
	prepareBrowserProcess(cmd)
	// Browser diagnostics can contain authorization URLs; do not copy its
	// stdout/stderr into terminal output or public error messages.
	if err = cmd.Start(); err != nil {
		return "", errors.New("cannot start browser; check --browser-path and the desktop session")
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { stopBrowserProcess(cmd, done) }) }
	defer stop()
	endpoint, err := waitBrowserPage(ctx, profile, done)
	if err != nil {
		return "", err
	}
	return captureBrowserPageLifecycle(ctx, endpoint, authorizeURL, BrowserRedirect, browserDocumentAllowed, stop)
}

func removeBrowserProfile(profile string) {
	// Windows may briefly retain profile locks after the browser exits.
	for i := 0; i < 20; i++ {
		if os.RemoveAll(profile) == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func browserExecutable(explicit string) (string, error) {
	if explicit != "" {
		if path, err := exec.LookPath(explicit); err == nil {
			return path, nil
		}
		return "", errors.New("--browser-path is not an executable browser")
	}
	var candidates []string
	switch runtime.GOOS {
	case "darwin":
		for _, root := range []string{"/Applications", filepath.Join(os.Getenv("HOME"), "Applications")} {
			for _, app := range []string{"Google Chrome", "Chromium", "Microsoft Edge"} {
				candidates = append(candidates, filepath.Join(root, app+".app", "Contents", "MacOS", app))
			}
		}
	case "windows":
		for _, root := range []string{os.Getenv("PROGRAMFILES"), os.Getenv("PROGRAMFILES(X86)"), os.Getenv("LOCALAPPDATA")} {
			if root != "" {
				candidates = append(candidates, filepath.Join(root, "Google", "Chrome", "Application", "chrome.exe"), filepath.Join(root, "Microsoft", "Edge", "Application", "msedge.exe"), filepath.Join(root, "Chromium", "Application", "chrome.exe"))
			}
		}
	}
	candidates = append(candidates, "google-chrome", "google-chrome-stable", "chromium", "chromium-browser", "microsoft-edge", "microsoft-edge-stable", "chrome", "msedge")
	for _, candidate := range candidates {
		if path, err := exec.LookPath(candidate); err == nil {
			return path, nil
		}
	}
	return "", errors.New("Chrome, Chromium or Edge was not found; install a desktop browser or set --browser-path (use device login on a headless server)")
}

func browserProxyArgs(address string) ([]string, error) {
	if address == "" {
		return []string{"--no-proxy-server"}, nil
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || u.Hostname() == "" || u.Opaque != "" || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.ForceQuery || strings.Contains(address, "#") {
		return nil, errors.New("invalid browser proxy URL")
	}
	if u.User != nil {
		return nil, errors.New("browser login requires a proxy without embedded credentials; use a local authenticated-proxy relay or device login")
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	case "socks5h":
		u.Scheme = "socks5" // Chromium SOCKS5 resolves hostnames through the proxy.
	default:
		return nil, errors.New("unsupported browser proxy scheme")
	}
	if strings.ContainsAny(u.Host, ";, \t\r\n") {
		return nil, errors.New("invalid browser proxy URL")
	}
	return []string{"--proxy-server=" + u.Scheme + "://" + u.Host, "--proxy-bypass-list=<-loopback>"}, nil
}

func browserHTTPClient() *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: nil, DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext}, Timeout: 3 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func waitBrowserPage(ctx context.Context, profile string, done <-chan struct{}) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	client := browserHTTPClient()
	defer client.CloseIdleConnections()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-done:
			return "", errors.New("browser exited before authorization; check the desktop session and browser sandbox")
		case <-ticker.C:
		}
		data, err := os.ReadFile(filepath.Join(profile, "DevToolsActivePort"))
		if err != nil {
			continue
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		port, err := strconv.Atoi(strings.TrimSpace(lines[0]))
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		address := "127.0.0.1:" + strconv.Itoa(port)
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+address+"/json/list", nil)
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var targets []struct {
			Type   string `json:"type"`
			URL    string `json:"url"`
			Socket string `json:"webSocketDebuggerUrl"`
		}
		err = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<20)).Decode(&targets)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		for _, target := range targets {
			if target.Type != "page" || target.URL != "about:blank" {
				continue
			}
			u, err := url.Parse(target.Socket)
			if err == nil && u.Scheme == "ws" && u.Host == address && u.User == nil && u.RawQuery == "" && u.Fragment == "" && strings.HasPrefix(u.Path, "/devtools/page/") {
				return target.Socket, nil
			}
		}
	}
}

type browserMessage struct {
	ID        int64           `json:"id"`
	SessionID string          `json:"sessionId,omitempty"`
	Method    string          `json:"method"`
	Params    json.RawMessage `json:"params"`
	Result    json.RawMessage `json:"result"`
	Error     json.RawMessage `json:"error"`
}

type browserCDP struct {
	conn    *websocket.Conn
	mu      sync.Mutex
	next    int64
	pending map[int64]chan browserMessage
	events  chan browserMessage
	done    chan struct{}
}

func newBrowserCDP(ctx context.Context, endpoint string) (*browserCDP, error) {
	client := browserHTTPClient()
	conn, _, err := websocket.Dial(ctx, endpoint, &websocket.DialOptions{HTTPClient: client})
	if err != nil {
		client.CloseIdleConnections()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("cannot connect to the temporary browser")
	}
	conn.SetReadLimit(2 << 20)
	c := &browserCDP{conn: conn, pending: make(map[int64]chan browserMessage), events: make(chan browserMessage, 128), done: make(chan struct{})}
	// Keep the control connection alive when the user cancels login. Otherwise
	// a detached debugger could release paused requests before Browser.close.
	readCtx, stopRead := context.WithCancel(context.Background())
	go func() {
		defer stopRead()
		defer close(c.done)
		defer client.CloseIdleConnections()
		for {
			_, raw, err := conn.Read(readCtx)
			if err != nil {
				return
			}
			var msg browserMessage
			if json.Unmarshal(raw, &msg) != nil {
				return
			}
			if msg.ID != 0 {
				c.mu.Lock()
				ch := c.pending[msg.ID]
				c.mu.Unlock()
				if ch != nil {
					select {
					case ch <- msg:
					default:
						return
					}
				}
			} else if msg.Method != "" {
				select {
				case c.events <- msg:
				case <-readCtx.Done():
					return
				default:
					return // Never silently drop a paused callback.
				}
			}
		}
	}()
	return c, nil
}

func (c *browserCDP) call(ctx context.Context, method string, params any, result any) error {
	return c.callSession(ctx, "", method, params, result)
}

func (c *browserCDP) callSession(ctx context.Context, session, method string, params any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	c.next++
	id := c.next
	ch := make(chan browserMessage, 1)
	c.pending[id] = ch
	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()
	request := map[string]any{"id": id, "method": method, "params": params}
	if session != "" {
		request["sessionId"] = session
	}
	raw, _ := json.Marshal(request)
	// Caller cancellation controls the response wait, not the socket lifetime.
	// A write uses its own short deadline so cleanup can still close the browser.
	writeCtx, cancelWrite := context.WithTimeout(context.Background(), 2*time.Second)
	err := c.conn.Write(writeCtx, websocket.MessageText, raw)
	cancelWrite()
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("browser control connection failed")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("browser control connection closed")
	case msg := <-ch:
		if msg.SessionID != session {
			return errors.New("browser control response session mismatch")
		}
		if len(msg.Error) != 0 && string(msg.Error) != "null" {
			return errors.New("browser rejected an authorization control command")
		}
		if result != nil && json.Unmarshal(msg.Result, result) != nil {
			return errors.New("invalid browser control response")
		}
		return nil
	}
}

// captureBrowserPage is also exercised with local mock authorization pages.
// Production callers only pass the validated OpenAI entry and fixed callback.
func captureBrowserPage(ctx context.Context, endpoint, entry, callback string) (string, error) {
	return captureBrowserPagePolicy(ctx, endpoint, entry, callback, browserDocumentAllowed)
}

func captureBrowserPagePolicy(ctx context.Context, endpoint, entry, callback string, allowDocument func(string, bool) bool) (string, error) {
	return captureBrowserPageLifecycle(ctx, endpoint, entry, callback, allowDocument, nil)
}

func captureBrowserPageLifecycle(ctx context.Context, endpoint, entry, callback string, allowDocument func(string, bool) bool, stopBrowser func()) (string, error) {
	return captureBrowserPageLifecycleMode(ctx, endpoint, entry, callback, allowDocument, stopBrowser, true)
}

func captureBrowserPageLifecycleMode(ctx context.Context, endpoint, entry, callback string, allowDocument func(string, bool) bool, stopBrowser func(), closeBrowser bool) (string, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	c, err := newBrowserCDP(ctx, endpoint)
	if err != nil {
		return "", err
	}
	defer func() {
		// Do not disable Fetch: doing so could release a captured authorization
		// request. An attached, task-owned tab must not close its shared browser.
		closeCtx, closeCancel := context.WithTimeout(context.Background(), time.Second)
		defer closeCancel()
		closeMethod := "Page.close"
		if closeBrowser {
			closeMethod = "Browser.close"
		}
		_ = c.call(closeCtx, closeMethod, map[string]any{}, nil)
		if stopBrowser != nil {
			stopBrowser()
		}
		_ = c.conn.CloseNow()
	}()
	setupCtx, setupCancel := context.WithTimeout(ctx, 10*time.Second)
	defer setupCancel()
	if err = c.call(setupCtx, "Page.enable", map[string]any{}, nil); err != nil {
		return "", err
	}
	var frame struct {
		FrameTree struct {
			Frame struct {
				ID string `json:"id"`
			} `json:"frame"`
		} `json:"frameTree"`
	}
	if err = c.call(setupCtx, "Page.getFrameTree", map[string]any{}, &frame); err != nil {
		return "", err
	}
	mainFrame := frame.FrameTree.Frame.ID
	if mainFrame == "" {
		return "", errors.New("temporary browser has no main frame")
	}
	if err = c.call(setupCtx, "Fetch.enable", map[string]any{"patterns": []map[string]string{{"urlPattern": "*", "resourceType": "Document", "requestStage": "Request"}}}, nil); err != nil {
		return "", err
	}
	// Cross-site iframes have their own CDP targets. Pause them before any
	// script can navigate to the callback, then install the same Fetch policy.
	if err = c.call(setupCtx, "Target.setAutoAttach", browserFrameAttachment(), nil); err != nil {
		return "", err
	}
	frameSessions := make(map[string]bool)
	navigation := make(chan error, 1)
	go func() {
		var result struct {
			ErrorText string `json:"errorText"`
		}
		err := c.call(ctx, "Page.navigate", map[string]string{"url": entry}, &result)
		if err == nil && result.ErrorText != "" {
			err = errors.New("browser could not load the authorization page; check proxy connectivity")
		}
		navigation <- err
	}()
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-c.done:
			if ctx.Err() != nil {
				return "", ctx.Err()
			}
			return "", errors.New("authorization browser was closed")
		case err := <-navigation:
			if err != nil {
				return "", err
			}
			navigation = nil
		case event := <-c.events:
			if event.Method == "Target.attachedToTarget" {
				var attached struct {
					SessionID  string `json:"sessionId"`
					TargetInfo struct {
						Type string `json:"type"`
					} `json:"targetInfo"`
				}
				if json.Unmarshal(event.Params, &attached) != nil || attached.SessionID == "" || attached.TargetInfo.Type != "iframe" || (event.SessionID != "" && !frameSessions[event.SessionID]) {
					return "", errors.New("invalid browser frame attachment")
				}
				frameSessions[attached.SessionID] = true
				attachCtx, attachCancel := context.WithTimeout(ctx, 5*time.Second)
				err = c.callSession(attachCtx, attached.SessionID, "Fetch.enable", map[string]any{"patterns": []map[string]string{{"urlPattern": "*", "resourceType": "Document", "requestStage": "Request"}}}, nil)
				if err == nil {
					err = c.callSession(attachCtx, attached.SessionID, "Target.setAutoAttach", browserFrameAttachment(), nil)
				}
				if err == nil {
					err = c.callSession(attachCtx, attached.SessionID, "Runtime.runIfWaitingForDebugger", map[string]any{}, nil)
				}
				attachCancel()
				if err != nil {
					return "", err
				}
				continue
			}
			if event.Method == "Target.detachedFromTarget" {
				var detached struct {
					SessionID string `json:"sessionId"`
				}
				if json.Unmarshal(event.Params, &detached) != nil {
					return "", errors.New("invalid browser frame detachment")
				}
				delete(frameSessions, detached.SessionID)
				continue
			}
			if event.Method != "Fetch.requestPaused" {
				continue
			}
			if event.SessionID != "" && !frameSessions[event.SessionID] {
				return "", errors.New("unknown browser frame session")
			}
			var paused struct {
				ID      string `json:"requestId"`
				Frame   string `json:"frameId"`
				Request struct {
					URL string `json:"url"`
				} `json:"request"`
			}
			if json.Unmarshal(event.Params, &paused) != nil || paused.ID == "" {
				return "", errors.New("invalid browser navigation event")
			}
			matched := browserCallbackMatches(callback, paused.Request.URL)
			allowed := allowDocument(paused.Request.URL, event.SessionID != "" || (paused.Frame != "" && paused.Frame != mainFrame))
			method := "Fetch.continueRequest"
			params := map[string]string{"requestId": paused.ID}
			if matched || !allowed {
				method = "Fetch.failRequest"
				params["errorReason"] = "Aborted"
			}
			commandCtx, commandCancel := context.WithTimeout(ctx, 5*time.Second)
			err = c.callSession(commandCtx, event.SessionID, method, params, nil)
			commandCancel()
			if err != nil {
				return "", err
			}
			if matched {
				if event.SessionID != "" || paused.Frame != mainFrame {
					return "", errors.New("authorization callback appeared outside the main browser page")
				}
				return paused.Request.URL, nil
			}
			if !allowed {
				return "", errors.New("unexpected authorization navigation was blocked; use device login for unsupported sign-in providers")
			}
		}
	}
}

func browserFrameAttachment() map[string]any {
	return map[string]any{
		"autoAttach": true, "waitForDebuggerOnStart": true, "flatten": true,
		"filter": []map[string]any{{"type": "iframe", "exclude": false}, {"exclude": true}},
	}
}

func browserCallbackMatches(expected, actual string) bool {
	e, err := url.Parse(expected)
	if err != nil {
		return false
	}
	u, err := url.Parse(actual)
	return err == nil && u.Scheme == e.Scheme && u.Host == e.Host && u.Path == e.Path && u.RawPath == "" && u.User == nil && !strings.Contains(actual, "#")
}

func browserDocumentAllowed(raw string, embedded bool) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	switch u.Host {
	case "auth.openai.com", "chatgpt.com":
		return true
	case "sentinel.openai.com", "cdn.platform.openai.com":
		return embedded
	default:
		return false
	}
}
