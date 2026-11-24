package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/zyxzjyzjj/cpa-gptbps-plugin/internal/gateway"
	"github.com/zyxzjyzjj/cpa-gptbps-plugin/internal/proxy"
	"gopkg.in/yaml.v3"
)

type config struct {
	StateDir      string   `yaml:"state_dir"`
	ProxyURL      string   `yaml:"proxy_url"`
	Models        []string `yaml:"models"`
	Concurrency   int      `yaml:"concurrency"`
	Queue         int      `yaml:"queue"`
	WaitSeconds   int      `yaml:"wait_seconds"`
	UpstreamRPS   int      `yaml:"upstream_rps"`
	WSIdleSeconds int      `yaml:"ws_idle_seconds"`
	LoginMethod   string   `yaml:"login_method"`
	// Upstream is intentionally not user-configurable. Tests can inject fixtures.
	upstream string
}

func defaultConfig() config {
	return config{Models: []string{"gpt-5.6-sol"}, Concurrency: 4, Queue: 64, WaitSeconds: 30, WSIdleSeconds: 120, LoginMethod: "browser"}
}

type plugin struct {
	mu         sync.Mutex
	cfg        config
	host       hostCall
	hostConfig hostConfig
	state      *gateway.StandaloneState
	handler    *gateway.StandaloneHandler
	ctx        context.Context
	cancel     context.CancelFunc
	logins     map[string]*loginSession
	closed     bool
	refreshMu  sync.Mutex
	runs       sync.WaitGroup
}

func newPlugin(call hostCall) *plugin {
	ctx, cancel := context.WithCancel(context.Background())
	return &plugin{cfg: defaultConfig(), host: call, ctx: ctx, cancel: cancel, logins: map[string]*loginSession{}}
}
func (p *plugin) configure(raw []byte) error {
	var req lifecycleRequest
	if err := decode(raw, &req); err != nil {
		return err
	}
	cfg := defaultConfig()
	if len(req.ConfigYAML) > 0 && yaml.Unmarshal(req.ConfigYAML, &cfg) != nil {
		return badRequest("invalid codex-bps YAML configuration")
	}
	cfg.StateDir = strings.TrimSpace(cfg.StateDir)
	if cfg.StateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return errors.New("home directory unavailable")
		}
		cfg.StateDir = filepath.Join(home, ".cpa-codex-bps", "state")
	}
	if cfg.Concurrency < 1 || cfg.Concurrency > 10000 || cfg.Queue < 0 || cfg.WaitSeconds < 1 || cfg.WaitSeconds > 3600 || cfg.UpstreamRPS < 0 || cfg.WSIdleSeconds < 1 {
		return badRequest("invalid codex-bps concurrency, queue, wait, pacing or idle limit")
	}
	if cfg.LoginMethod != "browser" && cfg.LoginMethod != "device" {
		return badRequest("login_method must be browser or device")
	}
	if len(cfg.Models) == 0 {
		return badRequest("models must contain at least one BPS model ID")
	}
	seen := map[string]bool{}
	for _, name := range cfg.Models {
		if strings.TrimSpace(name) != name || name == "" || strings.ContainsAny(name, "\r\n") || seen[name] {
			return badRequest("models must contain unique nonempty IDs")
		}
		seen[name] = true
	}
	if err := validateProxy(cfg.ProxyURL); err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return &rpcError{Code: "plugin_draining", Message: "codex-bps is shutting down", HTTPStatus: 503}
	}
	if p.handler != nil && !reflect.DeepEqual(cfg, p.cfg) {
		// Keep task ownership and account admission stable. Restart applies changes
		// atomically without sharing live slots with the previous configuration.
		return badRequest("restart CPA to change codex-bps configuration after first execution")
	}
	p.cfg = cfg
	return nil
}
func (p *plugin) getConfig() config {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cfg
}
func (p *plugin) gateway() (*gateway.StandaloneHandler, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, &rpcError{Code: "plugin_draining", Message: "codex-bps is shutting down", HTTPStatus: 503}
	}
	if p.handler != nil {
		return p.handler, nil
	}
	state, err := gateway.OpenStandaloneState(p.cfg.StateDir)
	if err != nil {
		return nil, &rpcError{Code: "state_unavailable", Message: "codex-bps task state unavailable or already owned; use a private state_dir per CPA instance", HTTPStatus: 503}
	}
	bridge, err := proxy.New(proxy.Config{ProxyURL: p.cfg.ProxyURL, Upstream: p.cfg.upstream, UpstreamTransport: "ws",
		WSIdleTimeout: time.Duration(p.cfg.WSIdleSeconds) * time.Second, UpstreamRPS: p.cfg.UpstreamRPS,
		RateQueueLimit: p.cfg.Queue, RateWaitTimeout: time.Duration(p.cfg.WaitSeconds) * time.Second}, credential{})
	if err != nil {
		state.Close()
		return nil, err
	}
	handler, err := gateway.NewStandaloneHandler(bridge, credential{}, state.Key(), gateway.Limits{
		PerAccount: p.cfg.Concurrency, Queue: p.cfg.Queue, Wait: time.Duration(p.cfg.WaitSeconds) * time.Second})
	if err != nil {
		state.Close()
		return nil, err
	}
	p.state, p.handler = state, handler
	return handler, nil
}
func (p *plugin) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	p.cancel()
	handler, state := p.handler, p.state
	for _, session := range p.logins {
		session.cancel()
	}
	p.mu.Unlock()
	if handler != nil {
		handler.Close()
		handler.Wait()
	}
	p.runs.Wait()
	if state != nil {
		state.Close()
	}
}

func (p *plugin) beginRun() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return &rpcError{Code: "plugin_draining", Message: "codex-bps is shutting down", HTTPStatus: 503}
	}
	p.runs.Add(1)
	return nil
}
func (p *plugin) dispatch(method string, raw []byte) (any, error) {
	switch method {
	case "plugin.register", "plugin.reconfigure":
		if err := p.configure(raw); err != nil {
			return nil, err
		}
		return registration(), nil
	case "plugin.quiesce", "plugin.shutdown":
		p.close()
		return struct{}{}, nil
	case "auth.identifier", "executor.identifier":
		return map[string]any{"identifier": provider}, nil
	case "auth.parse":
		return p.parseAuth(raw)
	case "auth.login.start":
		var req loginRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return p.startLogin(req)
	case "auth.login.poll":
		var req loginRequest
		if err := decode(raw, &req); err != nil {
			return nil, err
		}
		return p.pollLogin(req)
	case "auth.refresh":
		return p.refresh(raw)
	case "model.static":
		return map[string]any{"Provider": provider, "Models": []modelInfo{}}, nil
	case "model.for_auth":
		return p.models(raw)
	case "executor.execute", "executor.execute_stream":
		return p.execute(raw, method == "executor.execute_stream")
	case "response.normalize_after":
		return normalizeWireResponse(raw)
	case "executor.count_tokens", "executor.http_request":
		return nil, &rpcError{Code: "not_implemented", Message: "BPS has no standalone token-count or arbitrary HTTP endpoint", HTTPStatus: 501}
	case "management.register":
		return managementRegistration(), nil
	case "management.handle":
		return p.manage(raw)
	default:
		return nil, &rpcError{Code: "unknown_method", Message: "unsupported plugin method", HTTPStatus: 400}
	}
}
func registration() any {
	fields := []map[string]any{
		{"Name": "state_dir", "Type": "string", "Description": "Persistent private task state; separate directory per CPA process. Restart required after first request."},
		{"Name": "proxy_url", "Type": "string", "Description": "Fallback outbound proxy. Auth proxy_url then host proxy take precedence. Credentials are never returned by the panel."},
		{"Name": "models", "Type": "array", "Default": []string{"gpt-5.6-sol"}, "Description": "BPS model IDs allowed by your account; static configuration does not prove entitlement."},
		{"Name": "concurrency", "Type": "integer", "Default": 4, "Description": "Original BPS per-account concurrency."},
		{"Name": "queue", "Type": "integer", "Default": 64, "Description": "Original BPS waiting queue capacity."},
		{"Name": "wait_seconds", "Type": "integer", "Default": 30, "Description": "Queue and upstream start budget."},
		{"Name": "upstream_rps", "Type": "integer", "Default": 0, "Description": "Per-account dial and response.create pacing; 0 disables."},
		{"Name": "ws_idle_seconds", "Type": "integer", "Default": 120, "Description": "Upstream message idle bound; no total generation timeout."},
		{"Name": "login_method", "Type": "enum", "Default": "browser", "EnumValues": []string{"browser", "device"}, "Description": "BPS PKCE browser flow or device code (some workspaces disable device auth)."},
	}
	return map[string]any{
		// Only schema 1 capabilities are used, so older ABI 1 hosts can load it.
		// Current CPA v8/schema 10 is used for acceptance and usage accounting.
		"schema_version": 1,
		"metadata":       map[string]any{"Name": "Codex BPS", "Version": version, "Author": "zyxzjyzjj", "GitHubRepository": repository, "ConfigFields": fields},
		"capabilities": map[string]any{"model_provider": true, "auth_provider": true, "executor": true,
			"executor_model_scope": "oauth", "executor_input_formats": []string{"openai-response", "codex"}, "executor_output_formats": []string{"codex"},
			"response_after_translator": true, "management_api": true},
	}
}
func (p *plugin) models(raw []byte) (any, error) {
	var req authMaterial
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	if req.AuthProvider != "" && req.AuthProvider != provider {
		return map[string]any{"Provider": provider, "Models": []modelInfo{}}, nil
	}
	if _, err := readCredential(materialJSON(req.StorageJSON, req.Metadata)); err != nil {
		return nil, err
	}
	models := []modelInfo{}
	for _, id := range p.getConfig().Models {
		models = append(models, modelInfo{ID: id, Object: "model", OwnedBy: provider, Type: provider, DisplayName: id + " (BPS)",
			SupportedParameters:      []string{"tools", "tool_choice", "reasoning_effort", "response_format"},
			SupportedInputModalities: []string{"text", "image"}, SupportedOutputModalities: []string{"text"},
			Thinking: &thinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}})
	}
	return map[string]any{"Provider": provider, "Models": models}, nil
}
func materialJSON(raw []byte, metadata map[string]any) []byte {
	if len(raw) > 0 {
		return raw
	}
	data, _ := json.Marshal(metadata)
	return data
}
