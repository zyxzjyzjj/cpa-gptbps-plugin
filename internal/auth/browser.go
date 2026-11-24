package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BrowserRedirect is the registered BPS browser redirect. It cannot be replaced
// with a localhost URL. The task-owned browser captures it before delivery.
const BrowserRedirect = "https://bps.openai.com/basispoints/extension/360590d7-f8f9-4d88-bf75-0edfe0a4b9f3/auth/callback"

// NewBrowserClient keeps the token exchange on the same explicit proxy policy
// as the task-owned browser. With no proxy, both connect directly.
func NewBrowserClient(proxyAddress string) (*DeviceClient, error) {
	c, err := NewDeviceClient(proxyAddress)
	if err != nil {
		return nil, err
	}
	if proxyAddress == "" {
		c.HTTP.Transport.(*http.Transport).Proxy = nil
	}
	return c, nil
}

// LoginBrowser creates an independent authorization. authorize must intercept
// the registered callback before network delivery and respect the supplied
// context. No existing browser cookies or credentials are imported.
func (c *DeviceClient) LoginBrowser(ctx context.Context, authorize func(context.Context, string) (string, error)) (*DeviceCredentials, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var random [64]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, errors.New("cannot initialize BPS browser authorization")
	}
	verifier := base64.RawURLEncoding.EncodeToString(random[:32])
	state := base64.RawURLEncoding.EncodeToString(random[32:])
	digest := sha256.Sum256([]byte(verifier))
	params := url.Values{
		"client_id": {ExcelClientID}, "redirect_uri": {BrowserRedirect},
		"response_type": {"code"}, "scope": {"openid offline_access email profile organization.read"},
		"state": {state}, "code_challenge": {base64.RawURLEncoding.EncodeToString(digest[:])},
		"code_challenge_method": {"S256"},
	}
	callback, err := authorize(ctx, "https://auth.openai.com/oauth/authorize?"+params.Encode())
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	code, err := browserAuthorizationCode(callback, state)
	if err != nil {
		return nil, err
	}
	creds, err := c.exchange(ctx, code, verifier, BrowserRedirect)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(creds.RefreshToken) == "" {
		return nil, errors.New("BPS browser authorization did not return a refresh token")
	}
	return creds, nil
}

func browserAuthorizationCode(callback, state string) (string, error) {
	// Keep malformed callback contents out of errors and logs.
	if len(callback) > 16<<10 {
		return "", errors.New("BPS OAuth callback is too large")
	}
	u, err := url.Parse(callback)
	expected, _ := url.Parse(BrowserRedirect)
	if err != nil || u.Scheme != expected.Scheme || u.Host != expected.Host || u.Path != expected.Path || u.RawPath != "" || u.User != nil || strings.Contains(callback, "#") || u.Opaque != "" {
		return "", errors.New("BPS OAuth callback mismatch")
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil || len(values["state"]) != 1 || state == "" || subtle.ConstantTimeCompare([]byte(values.Get("state")), []byte(state)) != 1 {
		return "", errors.New("BPS OAuth state mismatch")
	}
	if _, exists := values["error"]; exists {
		return "", errors.New("BPS browser authorization was rejected")
	}
	if len(values["code"]) != 1 || strings.TrimSpace(values.Get("code")) == "" {
		return "", errors.New("BPS OAuth callback missing a single authorization code")
	}
	return values.Get("code"), nil
}
