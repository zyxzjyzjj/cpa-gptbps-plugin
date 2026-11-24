package auth

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
)

// CaptureBrowserAuthorizationInTab uses a caller-created authorization tab in
// an existing browser. The endpoint must identify that task-owned page on a
// loopback CDP server. Cleanup closes only that tab, preserving other tabs and
// the shared browser's profile. Callback interception and navigation boundaries
// are identical to isolated browser authorization.
func CaptureBrowserAuthorizationInTab(ctx context.Context, authorizeURL, endpoint string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	entry, err := url.Parse(authorizeURL)
	if err != nil || entry.Scheme != "https" || entry.Host != "auth.openai.com" ||
		entry.Path != "/oauth/authorize" || entry.RawPath != "" || entry.User != nil ||
		strings.Contains(authorizeURL, "#") {
		return "", errors.New("invalid browser authorization URL")
	}
	target, err := url.Parse(endpoint)
	if err != nil || target.Scheme != "ws" || target.User != nil ||
		target.RawQuery != "" || target.ForceQuery || target.Fragment != "" ||
		target.RawPath != "" || target.Port() == "" ||
		!strings.HasPrefix(target.Path, "/devtools/page/") ||
		strings.TrimPrefix(target.Path, "/devtools/page/") == "" {
		return "", errors.New("authorization tab requires a loopback page CDP endpoint")
	}
	ip := net.ParseIP(target.Hostname())
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("authorization tab requires a loopback page CDP endpoint")
	}
	return captureBrowserPageLifecycleMode(ctx, endpoint, authorizeURL, BrowserRedirect, browserDocumentAllowed, nil, false)
}
