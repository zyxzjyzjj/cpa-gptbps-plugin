package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	bpsauth "github.com/zyxzjyzjj/cpa-gptbps-plugin/internal/auth"
)

type loginSession struct {
	mu        sync.Mutex
	state     string
	url       string
	code      string
	expires   time.Time
	callback  chan string
	done      chan struct{}
	cancel    context.CancelFunc
	auth      *authData
	err       error
	submitted bool
}

func (p *plugin) startLogin(req loginRequest) (any, error) {
	p.rememberHost(req.Host)
	method := p.getConfig().LoginMethod
	if s, ok := req.Metadata["method"].(string); ok && s != "" {
		method = s
	}
	if method != "browser" && method != "device" {
		return nil, badRequest("login method must be browser or device")
	}
	proxyURL := ""
	if s, ok := req.Metadata["proxy_url"].(string); ok {
		proxyURL = s
	}
	if err := validateProxy(proxyURL); err != nil {
		return nil, err
	}
	client, err := p.oauthClient(p.effectiveProxy(proxyURL), "")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(p.ctx, 15*time.Minute)
	session := &loginSession{expires: time.Now().Add(15 * time.Minute), callback: make(chan string, 1), done: make(chan struct{}), cancel: cancel}
	if err := p.beginRun(); err != nil {
		cancel()
		return nil, err
	}
	ready := make(chan error, 1)
	notify := func(authURL, code string) error {
		state := urlState(authURL)
		if state == "" {
			state = randomID()
		}
		session.mu.Lock()
		session.state, session.url, session.code = state, authURL, code
		session.mu.Unlock()
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return context.Canceled
		}
		for key, old := range p.logins {
			if time.Now().After(old.expires) {
				old.cancel()
				delete(p.logins, key)
			}
		}
		if len(p.logins) >= 32 {
			p.mu.Unlock()
			return errors.New("too many pending login sessions")
		}
		p.logins[state] = session
		p.mu.Unlock()
		ready <- nil
		return nil
	}
	go func() {
		defer p.runs.Done()
		defer close(session.done)
		defer cancel()
		var creds *bpsauth.DeviceCredentials
		var loginErr error
		if method == "device" {
			creds, loginErr = client.Login(ctx, func(prompt bpsauth.DevicePrompt) error { return notify(prompt.URL, prompt.UserCode) })
		} else {
			// Original BPS PKCE generation/state/redirect validation are reused.
			creds, loginErr = client.LoginBrowser(ctx, func(ctx context.Context, authURL string) (string, error) {
				if err := notify(authURL, ""); err != nil {
					return "", err
				}
				select {
				case callback := <-session.callback:
					return callback, nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			})
		}
		session.mu.Lock()
		defer session.mu.Unlock()
		if loginErr != nil {
			session.err = loginErr
			select {
			case ready <- loginErr:
			default:
			}
			return
		}
		cred := credential{DeviceCredentials: *creds, Type: provider, ProxyURL: proxyURL}
		raw, _ := json.Marshal(cred)
		cred, session.err = readCredential(raw)
		if session.err == nil {
			data := credentialData(cred, "")
			session.auth = &data
		}
	}()
	select {
	case err := <-ready:
		if err != nil {
			cancel()
			return nil, &rpcError{Code: "login_start_failed", Message: "BPS login could not start; check proxy or try browser mode if device auth is disabled", HTTPStatus: 502}
		}
		session.mu.Lock()
		defer session.mu.Unlock()
		metadata := map[string]any{"method": method, "callback_submission": "/v0/management/plugins/codex-bps/callback"}
		if session.code != "" {
			metadata["user_code"] = session.code
		}
		return loginStart{Provider: provider, URL: session.url, State: session.state, ExpiresAt: session.expires, Metadata: metadata}, nil
	case <-ctx.Done():
		cancel()
		return nil, &rpcError{Code: "login_start_failed", Message: "BPS login start timed out", HTTPStatus: 504}
	}
}
func urlState(value string) string {
	u, err := url.Parse(value)
	if err != nil {
		return ""
	}
	return u.Query().Get("state")
}
func (p *plugin) findLogin(state string) (*loginSession, error) {
	p.mu.Lock()
	s := p.logins[state]
	p.mu.Unlock()
	if s == nil || time.Now().After(s.expires) {
		return nil, badRequest("BPS authorization state missing or expired; start again")
	}
	return s, nil
}
func (p *plugin) submitCallback(state, callback string) error {
	s, err := p.findLogin(state)
	if err != nil {
		return err
	}
	u, parseErr := url.Parse(callback)
	expected, _ := url.Parse(bpsauth.BrowserRedirect)
	if parseErr != nil || u.Scheme != expected.Scheme || u.Host != expected.Host || u.Path != expected.Path || urlState(callback) != state || len(callback) > 16<<10 {
		return badRequest("BPS callback URL or state mismatch")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.submitted {
		return badRequest("BPS callback already submitted")
	}
	select {
	case <-s.done:
		return badRequest("BPS authorization already finished")
	default:
	}
	s.submitted = true
	select {
	case s.callback <- callback:
		return nil
	default:
		return badRequest("BPS callback already submitted")
	}
}
func (p *plugin) pollLogin(req loginRequest) (any, error) {
	s, err := p.findLogin(req.State)
	if err != nil {
		return loginPoll{Status: "error", Message: err.Error()}, nil
	}
	// The normal CPA OAuth callback box writes a pending-session file rather
	// than placing the code in PollLogin.Metadata. Read only this in-memory
	// session's exact native filename; never accept a caller-supplied path.
	if req.Host.AuthDir != "" {
		s.mu.Lock()
		submitted := s.submitted
		s.mu.Unlock()
		if !submitted {
			path := filepath.Join(req.Host.AuthDir, ".oauth-"+provider+"-"+s.state+".oauth")
			if info, statErr := os.Lstat(path); statErr == nil && info.Mode().IsRegular() && info.Size() <= 16<<10 {
				if file, openErr := os.Open(path); openErr == nil {
					body, readErr := io.ReadAll(io.LimitReader(file, (16<<10)+1))
					_ = file.Close()
					var callback struct {
						Code  string `json:"code"`
						State string `json:"state"`
						Error string `json:"error"`
					}
					if readErr == nil && len(body) <= 16<<10 && json.Unmarshal(body, &callback) == nil && callback.State == s.state {
						values := url.Values{"state": {s.state}}
						if callback.Error != "" {
							values.Set("error", "access_denied")
						} else {
							values.Set("code", callback.Code)
						}
						if p.submitCallback(s.state, bpsauth.BrowserRedirect+"?"+values.Encode()) == nil {
							_ = os.Remove(path)
						}
					}
				}
			}
		}
	}
	// CPA's generic callback polling uses code/state metadata. Reconstruct
	// only our registered redirect; LoginBrowser still validates it and PKCE.
	if callback, ok := req.Metadata["redirect_url"].(string); ok && callback != "" {
		_ = p.submitCallback(req.State, callback)
	} else if code, ok := req.Metadata["code"].(string); ok && strings.TrimSpace(code) != "" {
		_ = p.submitCallback(req.State, bpsauth.BrowserRedirect+"?"+url.Values{"code": {code}, "state": {req.State}}.Encode())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return loginPoll{Status: "error", Message: "BPS authorization failed or expired; start again"}, nil
	}
	if s.auth != nil {
		return loginPoll{Status: "success", Auth: s.auth}, nil
	}
	message := "Complete BPS login, then submit the registered callback URL in the Codex BPS panel"
	if s.code != "" {
		message = "Enter device code " + s.code + " in the BPS authorization page"
	}
	return loginPoll{Status: "pending", Message: message}, nil
}
