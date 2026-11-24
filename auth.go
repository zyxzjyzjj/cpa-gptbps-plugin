package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	bpsauth "github.com/zyxzjyzjj/cpa-gptbps-plugin/internal/auth"
)

type credential struct {
	bpsauth.DeviceCredentials
	Type      string `json:"type"`
	AccountID string `json:"account_id"`
	Email     string `json:"email,omitempty"`
	ProxyURL  string `json:"proxy_url,omitempty"`
	Label     string `json:"label,omitempty"`
	Prefix    string `json:"prefix,omitempty"`
	Disabled  bool   `json:"disabled,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"`
	expiry    time.Time
}

func (c credential) Credentials() (string, string, error) {
	if c.AccessToken == "" {
		return "", "", errors.New("BPS access token required")
	}
	if !c.expiry.IsZero() && !c.expiry.After(time.Now()) {
		return "", "", &rpcError{Code: "credential_expired", Message: "BPS access token expired; refresh or authorize again", HTTPStatus: 401}
	}
	return c.AccessToken, c.AccountID, nil
}
func (c credential) Expiry() time.Time { return c.expiry }
func readCredential(raw []byte) (credential, error) {
	var c credential
	var doc map[string]json.RawMessage
	if len(raw) > 4<<20 || json.Unmarshal(raw, &doc) != nil || doc == nil {
		return c, badRequest("invalid BPS credential JSON")
	}
	if nested := doc["tokens"]; len(nested) > 0 && string(nested) != "null" {
		if len(doc["access_token"]) > 0 {
			return c, badRequest("ambiguous BPS token envelope")
		}
		var tokens map[string]json.RawMessage
		if json.Unmarshal(nested, &tokens) != nil {
			return c, badRequest("invalid BPS token envelope")
		}
		for k, v := range tokens {
			doc[k] = v
		}
		delete(doc, "tokens")
	}
	// Existing standalone exports accept Unix seconds as well as RFC3339.
	if value := doc["expires_at"]; len(value) > 0 {
		var timestamp int64
		if json.Unmarshal(value, &timestamp) == nil && timestamp > 0 {
			doc["expires_at"], _ = json.Marshal(time.Unix(timestamp, 0).UTC().Format(time.RFC3339))
		}
	}
	normalized, _ := json.Marshal(doc)
	if json.Unmarshal(normalized, &c) != nil {
		return c, badRequest("invalid BPS credential fields")
	}
	if c.Type != "" && c.Type != provider {
		return c, badRequest("credential type must be codex-bps")
	}
	if strings.TrimSpace(c.AccessToken) == "" {
		return c, badRequest("BPS access_token is required; a Codex RT or API key is not a BPS login")
	}
	if c.TokenType != "" && !strings.EqualFold(c.TokenType, "Bearer") {
		return c, badRequest("BPS token_type must be Bearer")
	}
	claims := jwtClaims(c.AccessToken)
	diagnostics := bpsauth.InspectToken(c.AccessToken)
	if (c.ClientID != "" && c.ClientID != bpsauth.ExcelClientID) || (diagnostics.ClientID != "" && diagnostics.ClientID != bpsauth.ExcelClientID) {
		return c, badRequest("credential belongs to another OAuth client; authorize with the BPS client")
	}
	if c.ClientID == "" {
		c.ClientID = diagnostics.ClientID
	}
	if c.ClientID != bpsauth.ExcelClientID {
		return c, badRequest("BPS client_id is required for opaque credentials")
	}
	if id := bpsauth.AccountIDFromToken(c.AccessToken); id != "" {
		if c.AccountID != "" && c.AccountID != id {
			return c, badRequest("BPS account ID does not match token")
		}
		c.AccountID = id
	}
	if c.AccountID == "" {
		c.AccountID = bpsauth.AccountIDFromToken(c.IDToken)
	}
	if c.AccountID == "" {
		return c, badRequest("BPS account_id is required")
	}
	if c.Email == "" {
		c.Email, _ = jwtClaims(c.IDToken)["email"].(string)
		if c.Email == "" {
			profile, _ := claims["https://api.openai.com/profile"].(map[string]any)
			c.Email, _ = profile["email"].(string)
		}
	}
	if exp, ok := claims["exp"].(float64); ok && exp > 0 {
		c.expiry = time.Unix(int64(exp), 0)
	}
	if c.ExpiresAt != "" {
		t, err := time.Parse(time.RFC3339, c.ExpiresAt)
		if err != nil {
			return c, badRequest("invalid BPS expires_at")
		}
		if c.expiry.IsZero() || t.Before(c.expiry) {
			c.expiry = t
		}
	} else if c.ExpiresIn > 0 && c.ObtainedAt != "" {
		t, err := time.Parse(time.RFC3339, c.ObtainedAt)
		if err != nil || c.ExpiresIn > 31536000 {
			return c, badRequest("invalid BPS obtained_at or expires_in")
		}
		t = t.Add(time.Duration(c.ExpiresIn) * time.Second)
		if c.expiry.IsZero() || t.Before(c.expiry) {
			c.expiry = t
		}
	}
	if !c.expiry.IsZero() {
		c.ExpiresAt = c.expiry.UTC().Format(time.RFC3339)
	}
	c.Type, c.TokenType = provider, "Bearer"
	if err := validateProxy(c.ProxyURL); err != nil {
		return c, err
	}
	return c, nil
}
func jwtClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return map[string]any{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return map[string]any{}
	}
	var claims map[string]any
	_ = json.Unmarshal(raw, &claims)
	if claims == nil {
		return map[string]any{}
	}
	return claims
}
func credentialData(c credential, filename string) authData {
	if filename == "" {
		key := bpsauth.AdmissionKeyFromToken(c.AccessToken)
		if key == "" {
			key = c.AccountID
		}
		hash := sha256.Sum256([]byte(key))
		filename = provider + "-" + hex.EncodeToString(hash[:8]) + ".json"
	}
	raw, _ := json.Marshal(c)
	var metadata map[string]any
	_ = json.Unmarshal(raw, &metadata)
	// These native fields drive CPA's refresh scheduler and account statistics.
	if !c.expiry.IsZero() {
		metadata["expired"] = c.expiry.UTC().Format(time.RFC3339)
	}
	label := c.Label
	if label == "" {
		label = c.Email
	}
	if label == "" {
		label = "BPS " + c.AccountID
	}
	data := authData{Provider: provider, ID: filename, FileName: filename, Label: label, Prefix: c.Prefix, ProxyURL: c.ProxyURL, Disabled: c.Disabled,
		StorageJSON: raw, Metadata: metadata, Attributes: map[string]string{"account_id": c.AccountID}}
	if c.RefreshToken == "" {
		data.NextRefreshAfter = time.Now().Add(365 * 24 * time.Hour)
	} else if !c.expiry.IsZero() {
		data.NextRefreshAfter = c.expiry.Add(-5 * time.Minute)
	}
	return data
}
func (p *plugin) parseAuth(raw []byte) (any, error) {
	var req authParseRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	var marker struct {
		Type     string `json:"type"`
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal(req.RawJSON, &marker)
	// Never claim a built-in Codex credential. Standalone BPS exports carry
	// their BPS client ID; CPA files carry the codex-bps type.
	if marker.Type != provider && !(marker.Type == "" && marker.ClientID == bpsauth.ExcelClientID) {
		return map[string]any{"Handled": false}, nil
	}
	if marker.Type != "" && marker.Type != provider {
		return map[string]any{"Handled": false}, nil
	}
	c, err := readCredential(req.RawJSON)
	if err != nil {
		return nil, err
	}
	p.rememberHost(req.Host)
	return map[string]any{"Handled": true, "Auth": credentialData(c, req.FileName)}, nil
}
func validateProxy(address string) error {
	if address == "" {
		return nil
	}
	u, err := url.Parse(address)
	if err != nil || u.Host == "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https" && u.Scheme != "socks5" && u.Scheme != "socks5h") {
		return badRequest("invalid outbound proxy URL")
	}
	return nil
}
func (p *plugin) rememberHost(h hostConfig) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if h.AuthDir != "" {
		p.hostConfig = h
	}
}
func (p *plugin) effectiveProxy(authProxy string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if authProxy != "" {
		return authProxy
	}
	if p.hostConfig.ProxyURL != "" {
		return p.hostConfig.ProxyURL
	}
	return p.cfg.ProxyURL
}

// Normal OAuth HTTP uses the CPA transport when no explicit proxy override is
// needed. BPS WebSocket upgrades have no host WS API and use the copied core.
type hostOAuthTransport struct {
	call       hostCall
	callbackID string
}

func (t hostOAuthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(io.LimitReader(req.Body, 2<<20))
		if err != nil {
			return nil, err
		}
	}
	var result hostHTTPResponse
	err := t.call("host.http.do", map[string]any{"host_callback_id": t.callbackID, "method": req.Method, "url": req.URL.String(), "headers": req.Header, "body": body}, &result)
	if err != nil {
		return nil, errors.New("OAuth host transport failed")
	}
	return &http.Response{StatusCode: result.StatusCode, Header: result.Headers, Body: io.NopCloser(strings.NewReader(string(result.Body))), Request: req}, nil
}
func (p *plugin) oauthClient(proxyURL, callbackID string) (*bpsauth.DeviceClient, error) {
	client, err := bpsauth.NewBrowserClient(proxyURL)
	if err != nil {
		return nil, err
	}
	if proxyURL == "" && p.host != nil {
		client.HTTP.Transport = hostOAuthTransport{call: p.host, callbackID: callbackID}
	}
	return client, nil
}

// Refresh receipts and pending markers prevent reuse of an ambiguously consumed
// rotating grant. A pending marker survives a crash and requires reauthorization.
func (p *plugin) refresh(raw []byte) (any, error) {
	if err := p.beginRun(); err != nil {
		return nil, err
	}
	defer p.runs.Done()
	var req authMaterial
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	p.rememberHost(req.Host)
	c, err := readCredential(materialJSON(req.StorageJSON, req.Metadata))
	if err != nil {
		return nil, err
	}
	if c.RefreshToken == "" {
		return map[string]any{"Auth": credentialData(c, req.AuthID), "NextRefreshAfter": time.Now().Add(365 * 24 * time.Hour)}, nil
	}
	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()
	hash := sha256.Sum256([]byte(c.ClientID + "\x00" + c.RefreshToken))
	dir := p.getConfig().StateDir + "-refresh"
	if err = os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	key := filepath.Join(dir, hex.EncodeToString(hash[:]))
	if receipt, readErr := os.ReadFile(key + ".json"); readErr == nil {
		next, parseErr := readCredential(receipt)
		if parseErr == nil && next.expiry.After(time.Now().Add(time.Minute)) {
			data := credentialData(next, req.AuthID)
			return map[string]any{"Auth": data, "NextRefreshAfter": data.NextRefreshAfter}, nil
		}
		return nil, &rpcError{Code: "refresh_receipt_expired", Message: "BPS refresh receipt expired; authorize again to avoid replaying a rotated grant", HTTPStatus: 401}
	}
	file, err := os.OpenFile(key+".pending", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return nil, &rpcError{Code: "refresh_outcome_unknown", Message: "BPS refresh already attempted or pending; authorize again instead of replaying the grant", HTTPStatus: 401}
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return nil, err
	}
	_ = file.Close()
	client, err := p.oauthClient(p.effectiveProxy(c.ProxyURL), req.HostCallbackID)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancel()
	next, err := client.Refresh(ctx, c.DeviceCredentials)
	if err != nil {
		return nil, &rpcError{Code: "refresh_failed", Message: "BPS refresh failed or outcome unknown; authorize again. The grant was not retried.", HTTPStatus: 401}
	}
	updated := c
	updated.DeviceCredentials = *next
	updated.ExpiresAt = ""
	updated.expiry = time.Time{}
	body, _ := json.Marshal(updated)
	updated, err = readCredential(body)
	if err != nil {
		return nil, err
	}
	body, _ = json.Marshal(updated)
	if err = writePrivate(key+".json", body); err != nil {
		return nil, err
	}
	data := credentialData(updated, req.AuthID)
	return map[string]any{"Auth": data, "NextRefreshAfter": data.NextRefreshAfter}, nil
}
func writePrivate(path string, raw []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, err = file.Write(raw)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
