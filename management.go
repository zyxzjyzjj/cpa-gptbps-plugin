package main

import (
	_ "embed"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

//go:embed panel.html
var panel []byte

const managementBase = "/v0/management/plugins/codex-bps"

func managementRegistration() any {
	routes := []map[string]string{}
	for path, method := range map[string]string{"/status": "GET", "/import": "POST", "/login": "POST", "/poll": "POST", "/callback": "POST", "/cancel": "POST"} {
		routes = append(routes, map[string]string{"Method": method, "Path": managementBase + path})
	}
	return map[string]any{"routes": routes, "resources": []map[string]string{{"Path": "/panel", "Menu": "Codex BPS", "Description": "BPS authorization, credential import and capability information"}}}
}
func jsonResponse(status int, body any) managementResponse {
	raw, _ := json.Marshal(body)
	return managementResponse{StatusCode: status, Headers: http.Header{"Content-Type": {"application/json; charset=utf-8"}, "Cache-Control": {"no-store"}}, Body: raw}
}
func (p *plugin) saveAuth(data authData) error {
	return p.host("host.auth.save", map[string]any{"name": data.FileName, "json": json.RawMessage(data.StorageJSON)}, nil)
}
func (p *plugin) manage(raw []byte) (any, error) {
	var req managementRequest
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	// Resource GET returns only bundled UI. No query/body can trigger an action.
	if req.Method == "GET" && (req.Path == "/panel" || req.Path == "/v0/resource/plugins/codex-bps/panel") {
		return managementResponse{StatusCode: 200, Headers: http.Header{
			"Content-Type": {"text/html; charset=utf-8"}, "Cache-Control": {"no-store"}, "X-Content-Type-Options": {"nosniff"},
			"Content-Security-Policy": {"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'self'"},
		}, Body: panel}, nil
	}
	if !strings.HasPrefix(req.Path, managementBase+"/") {
		return jsonResponse(404, map[string]string{"error": "unknown route"}), nil
	}
	if len(req.Body) > 4<<20 {
		return jsonResponse(413, map[string]string{"error": "request too large"}), nil
	}
	var result any
	var err error
	switch req.Method + " " + strings.TrimPrefix(req.Path, managementBase) {
	case "GET /status":
		cfg := p.getConfig()
		p.mu.Lock()
		active := 0
		for _, s := range p.logins {
			if time.Now().Before(s.expires) {
				active++
			}
		}
		ready := p.handler != nil
		p.mu.Unlock()
		models, catalogs := p.catalogStatus()
		result = map[string]any{"provider": provider, "version": version, "gateway_initialized": ready, "pending_logins": active, "models": models,
			"model_catalog_source": bpsCatalogURL, "model_catalogs": catalogs, "model_cache_seconds": cfg.ModelCacheSeconds,
			"concurrency": cfg.Concurrency, "queue": cfg.Queue, "login_method": cfg.LoginMethod,
			"client_tools": true, "hosted_tools": false, "previous_response_id": false, "image_input": "https URL only",
			"usage": "CPA native reporter; cache reads come from upstream usage", "refresh": "BPS client only; rotating grants are never automatically retried"}
	case "POST /import":
		var c credential
		c, err = readCredential(req.Body)
		if err == nil {
			data := credentialData(c, "")
			err = p.saveAuth(data)
			if err == nil {
				result = map[string]any{"status": "ok", "filename": data.FileName, "provider": provider}
			}
		}
	case "POST /login":
		var options struct {
			Method   string `json:"method"`
			ProxyURL string `json:"proxy_url"`
		}
		err = decode(req.Body, &options)
		if err == nil {
			result, err = p.startLogin(loginRequest{Metadata: map[string]any{"method": options.Method, "proxy_url": options.ProxyURL}})
		}
	case "POST /callback":
		var submission struct {
			State       string `json:"state"`
			RedirectURL string `json:"redirect_url"`
		}
		err = decode(req.Body, &submission)
		if err == nil {
			err = p.submitCallback(submission.State, submission.RedirectURL)
		}
		result = map[string]string{"status": "pending"}
	case "POST /poll":
		var options struct {
			State string `json:"state"`
		}
		err = decode(req.Body, &options)
		if err == nil {
			result, err = p.pollLogin(loginRequest{State: options.State})
			if poll, ok := result.(loginPoll); ok && poll.Auth != nil {
				err = p.saveAuth(*poll.Auth)
				if err == nil {
					result = map[string]string{"status": "success", "filename": poll.Auth.FileName}
				}
			} else if ok {
				result = map[string]string{"status": poll.Status, "message": poll.Message}
			}
		}
	case "POST /cancel":
		var options struct {
			State string `json:"state"`
		}
		err = decode(req.Body, &options)
		if err == nil {
			var s *loginSession
			s, err = p.findLogin(options.State)
			if err == nil {
				s.cancel()
				p.mu.Lock()
				delete(p.logins, options.State)
				p.mu.Unlock()
			}
		}
		result = map[string]string{"status": "cancelled"}
	default:
		return jsonResponse(404, map[string]string{"error": "unknown route"}), nil
	}
	if err != nil {
		var env envelope
		_ = json.Unmarshal(failure(err), &env)
		status := 500
		if env.Error != nil && env.Error.HTTPStatus > 0 {
			status = env.Error.HTTPStatus
		}
		return jsonResponse(status, map[string]any{"error": env.Error}), nil
	}
	return jsonResponse(200, result), nil
}
