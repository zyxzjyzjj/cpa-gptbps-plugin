package auth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBrowserLoginPKCEExchangeAndPersistence(t *testing.T) {
	exchanges := 0
	var challenge, previousChallenge, previousState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		exchanges++
		_ = r.ParseForm()
		verifier := r.Form.Get("code_verifier")
		digest := sha256.Sum256([]byte(verifier))
		if r.Method != "POST" || r.URL.Path != "/oauth/token" || r.URL.Query().Get("unified") != "true" || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("code") != "fixture-code" || r.Form.Get("client_id") != ExcelClientID || r.Form.Get("redirect_uri") != BrowserRedirect || len(verifier) < 43 || base64.RawURLEncoding.EncodeToString(digest[:]) != challenge {
			t.Error("browser code exchange does not match the original PKCE request")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "fixture-at", "refresh_token": "fixture-rt", "token_type": "Bearer", "expires_in": 3600, "scope": "openid offline_access", "earliest_refresh_at": 12345})
	}))
	defer srv.Close()
	client := &DeviceClient{HTTP: srv.Client(), base: srv.URL}
	for range 2 {
		creds, err := client.LoginBrowser(context.Background(), func(ctx context.Context, address string) (string, error) {
			u, e := url.Parse(address)
			if e != nil {
				t.Fatal(e)
			}
			q := u.Query()
			challenge = q.Get("code_challenge")
			state := q.Get("state")
			deadline, ok := ctx.Deadline()
			if u.Scheme != "https" || u.Host != "auth.openai.com" || u.Path != "/oauth/authorize" || q.Get("client_id") != ExcelClientID || q.Get("redirect_uri") != BrowserRedirect || q.Get("response_type") != "code" || q.Get("code_challenge_method") != "S256" || q.Get("scope") != "openid offline_access email profile organization.read" || len(state) < 43 || challenge == "" || challenge == previousChallenge || state == previousState || !ok || time.Until(deadline) > 15*time.Minute {
				t.Fatal("invalid or reused authorization parameters")
			}
			previousChallenge, previousState = challenge, state
			return BrowserRedirect + "?" + url.Values{"code": {"fixture-code"}, "state": {state}}.Encode(), nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if creds.ClientID != ExcelClientID || creds.ObtainedAt == "" || string(creds.EarliestRefreshAt) != "12345" {
			t.Fatal("credential metadata lost")
		}
		path := filepath.Join(t.TempDir(), "oauth.json")
		if err := SaveDeviceCredentials(path, creds); err != nil {
			t.Fatal(err)
		}
		store, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		at, _, err := store.Credentials()
		if err != nil || at != "fixture-at" {
			t.Fatal("browser credentials cannot be used by the bridge", err)
		}
		if err := SaveDeviceCredentials(path, creds); !errors.Is(err, os.ErrExist) {
			t.Fatal("existing credentials were overwritten")
		}
	}
	if exchanges != 2 {
		t.Fatal("unexpected token exchange count", exchanges)
	}
}

func TestBrowserLoginRejectsCallbacksBeforeExchange(t *testing.T) {
	for _, mode := range []string{"host", "http", "path", "encoded_path", "userinfo", "fragment", "empty_fragment", "duplicate_code", "duplicate_state", "state", "missing_state", "missing_code", "blank_code", "error", "malformed_query", "oversized", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			exchanges := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				exchanges++
				w.WriteHeader(500)
			}))
			defer srv.Close()
			client := &DeviceClient{HTTP: srv.Client(), base: srv.URL}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err := client.LoginBrowser(ctx, func(_ context.Context, address string) (string, error) {
				u, _ := url.Parse(address)
				q := url.Values{"state": {u.Query().Get("state")}, "code": {"fixture-secret"}}
				target := BrowserRedirect
				suffix := ""
				switch mode {
				case "host":
					target = "https://untrusted.example/auth/callback"
				case "http":
					target = strings.Replace(target, "https:", "http:", 1)
				case "path":
					target += "/"
				case "encoded_path":
					target = strings.Replace(target, "callback", "%63allback", 1)
				case "userinfo":
					target = strings.Replace(target, "https://", "https://fixture-secret@", 1)
				case "fragment":
					suffix = "#fixture-secret"
				case "empty_fragment":
					suffix = "#"
				case "duplicate_code":
					q.Add("code", "another")
				case "duplicate_state":
					q.Add("state", q.Get("state"))
				case "state":
					q.Set("state", "other")
				case "missing_state":
					q.Del("state")
				case "missing_code":
					q.Del("code")
				case "blank_code":
					q.Set("code", " ")
				case "error":
					q.Set("error", "fixture-secret")
				case "malformed_query":
					suffix = "&bad=%zz"
				case "oversized":
					q.Set("code", strings.Repeat("a", 17<<10))
				case "cancel":
					cancel()
				}
				return target + "?" + q.Encode() + suffix, nil
			})
			if err == nil || exchanges != 0 {
				t.Fatal("invalid callback exchanged a token")
			}
			if strings.Contains(err.Error(), "fixture-secret") {
				t.Fatal("credential appeared in error")
			}
		})
	}
}

func TestBrowserLoginRejectsMissingRefreshTokenAndSanitizesErrors(t *testing.T) {
	for _, mode := range []string{"missing_rt", "upstream_error", "redirect"} {
		t.Run(mode, func(t *testing.T) {
			forwarded := 0
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { forwarded++ }))
			defer other.Close()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "missing_rt":
					ioJSON(w, map[string]any{"access_token": "fixture-secret", "token_type": "Bearer", "expires_in": 3600})
				case "upstream_error":
					w.WriteHeader(400)
					ioJSON(w, map[string]any{"error": "fixture-secret", "error_description": "fixture-secret"})
				case "redirect":
					http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
				}
			}))
			defer srv.Close()
			client, _ := NewBrowserClient("")
			client.base = srv.URL
			_, err := client.LoginBrowser(context.Background(), func(_ context.Context, address string) (string, error) {
				u, _ := url.Parse(address)
				return BrowserRedirect + "?" + url.Values{"state": {u.Query().Get("state")}, "code": {"fixture-secret"}}.Encode(), nil
			})
			if err == nil || strings.Contains(err.Error(), "fixture-secret") || forwarded != 0 {
				t.Fatal("failed token response was accepted, exposed, or redirected")
			}
		})
	}
}

func TestBrowserClientProxyPolicy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://environment.invalid:8888")
	for _, address := range []string{"", "http://127.0.0.1:7890", "socks5h://127.0.0.1:7890"} {
		client, err := NewBrowserClient(address)
		if err != nil {
			t.Fatal(err)
		}
		transport := client.HTTP.Transport.(*http.Transport)
		if address == "" {
			if transport.Proxy != nil {
				t.Fatal("browser token exchange implicitly uses environment proxy")
			}
			continue
		}
		req, _ := http.NewRequest("POST", "https://auth.openai.com/oauth/token", nil)
		u, err := transport.Proxy(req)
		if err != nil || u.String() != address {
			t.Fatal("explicit proxy not preserved")
		}
	}
}
