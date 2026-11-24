package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"runtime"
	"sort"
	"strings"
	"time"
)

const bpsCatalogURL = "https://bps.openai.com/basispoints/api/responses/access?include_models=true"
const catalogMaxStale = 15 * time.Minute

type modelCatalog struct {
	Models       []modelInfo
	DefaultModel string
}
type catalogEntry struct {
	catalog   modelCatalog
	fetched   time.Time
	nextFetch time.Time
	err       *rpcError
	lastError string
	stale     bool
}

func (p *plugin) models(raw []byte) (any, error) {
	if err := p.beginRun(); err != nil {
		return nil, err
	}
	defer p.runs.Done()
	var req authMaterial
	if err := decode(raw, &req); err != nil {
		return nil, err
	}
	if req.AuthProvider != "" && req.AuthProvider != provider {
		return map[string]any{"Provider": provider, "Models": []modelInfo{}}, nil
	}
	c, err := readCredential(materialJSON(req.StorageJSON, req.Metadata))
	if err != nil {
		return nil, err
	}
	if _, _, err = c.Credentials(); err != nil {
		return nil, err
	}
	if c.Disabled {
		return map[string]any{"Provider": provider, "Models": []modelInfo{}}, nil
	}
	p.rememberHost(req.Host)
	cfg := p.getConfig()
	proxy := firstNonempty(c.ProxyURL, req.Host.ProxyURL, cfg.ProxyURL)
	digest := sha256.Sum256([]byte(req.AuthID + "\x00" + c.AccessToken + "\x00" + c.AccountID + "\x00" + proxy))
	key := hex.EncodeToString(digest[:])
	for {
		now := time.Now()
		p.catalogMu.Lock()
		old, exists := p.catalogs[key]
		if exists && now.Before(old.nextFetch) {
			p.catalogMu.Unlock()
			if old.err != nil {
				return nil, old.err
			}
			return map[string]any{"Provider": provider, "Models": cloneModels(old.catalog.Models)}, nil
		}
		if done := p.catalogFlights[key]; done != nil {
			p.catalogMu.Unlock()
			select {
			case <-done:
				continue
			case <-p.ctx.Done():
				return nil, catalogError("connection_lifecycle", "BPS model discovery cancelled", 499)
			}
		}
		if len(p.catalogFlights) >= 32 {
			p.catalogMu.Unlock()
			return nil, catalogError("transient_rate_limit", "BPS model discovery is busy; retry later", 429)
		}
		done := make(chan struct{})
		p.catalogFlights[key] = done
		p.catalogMu.Unlock()

		catalog, fetchErr := p.fetchCatalog(req, c, proxy)
		next := catalogEntry{catalog: catalog, fetched: time.Now(), nextFetch: time.Now().Add(time.Duration(cfg.ModelCacheSeconds) * time.Second)}
		if fetchErr != nil {
			next = catalogEntry{err: fetchErr, lastError: fetchErr.Code, nextFetch: time.Now().Add(30 * time.Second)}
			if catalogTransient(fetchErr) && exists && old.err == nil && time.Since(old.fetched) < catalogMaxStale {
				next = old
				next.nextFetch = time.Now().Add(30 * time.Second)
				next.lastError = fetchErr.Code
				next.stale = true
			}
		}
		p.catalogMu.Lock()
		if _, present := p.catalogs[key]; !present && len(p.catalogs) >= 256 {
			var oldestKey string
			var oldest time.Time
			for candidate, entry := range p.catalogs {
				if oldestKey == "" || entry.fetched.Before(oldest) {
					oldestKey, oldest = candidate, entry.fetched
				}
			}
			delete(p.catalogs, oldestKey)
		}
		p.catalogs[key] = next
		delete(p.catalogFlights, key)
		close(done)
		p.catalogMu.Unlock()
		if next.err != nil {
			return nil, next.err
		}
		return map[string]any{"Provider": provider, "Models": cloneModels(next.catalog.Models)}, nil
	}
}

func firstNonempty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
func catalogError(code, message string, status int) *rpcError {
	return &rpcError{Code: code, Message: message, HTTPStatus: status}
}
func catalogTransient(err *rpcError) bool {
	return err.Code == "model_catalog_transport" || err.HTTPStatus == 429 || err.Code == "model_catalog_upstream"
}
func cloneModels(models []modelInfo) []modelInfo {
	out := make([]modelInfo, len(models))
	for i, model := range models {
		out[i] = model
		out[i].SupportedParameters = append([]string(nil), model.SupportedParameters...)
		out[i].SupportedInputModalities = append([]string(nil), model.SupportedInputModalities...)
		out[i].SupportedOutputModalities = append([]string(nil), model.SupportedOutputModalities...)
		if model.Thinking != nil {
			out[i].Thinking = &thinkingSupport{Levels: append([]string(nil), model.Thinking.Levels...)}
		}
	}
	return out
}

// Host operations bind discovery cancellation to both the RPC and plugin
// lifecycle. Older hosts without operation callbacks retain their RPC scope.
type catalogHostTransport struct {
	call       hostCall
	callbackID string
}

func (t catalogHostTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var operation struct {
		ID string `json:"operation_id"`
	}
	_ = t.call("host.http.operation_open", map[string]any{"host_callback_id": t.callbackID}, &operation)
	request := map[string]any{"host_callback_id": t.callbackID, "method": req.Method, "url": req.URL.String(), "headers": req.Header}
	if operation.ID != "" {
		request["operation_id"] = operation.ID
		cancel := func() {
			_ = t.call("host.http.cancel", map[string]any{"operation_id": operation.ID, "host_callback_id": t.callbackID}, nil)
		}
		cancelDone := make(chan struct{})
		stop := context.AfterFunc(req.Context(), func() { defer close(cancelDone); cancel() })
		defer func() {
			if stop() {
				close(cancelDone)
			}
			cancel()
			<-cancelDone
		}()
	}
	var result hostHTTPResponse
	if err := t.call("host.http.do", request, &result); err != nil {
		return nil, errors.New("BPS model HTTP callback failed")
	}
	if len(result.Body) > 2<<20 {
		return nil, errors.New("BPS model response exceeds limit")
	}
	return &http.Response{StatusCode: result.StatusCode, Header: result.Headers, Body: io.NopCloser(bytes.NewReader(result.Body)), Request: req}, nil
}

func (p *plugin) fetchCatalog(material authMaterial, c credential, proxy string) (modelCatalog, *rpcError) {
	ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, bpsCatalogURL, nil)
	req.Header = http.Header{
		"Authorization": {"Bearer " + c.AccessToken}, "Chatgpt-Account-Id": {c.AccountID}, "X-Openai-Account-Id": {c.AccountID},
		"X-Basispoints-Auth-Mode": {"chatgpt"}, "Origin": {"https://bps.openai.com"}, "User-Agent": {"Mozilla/5.0"}, "Accept": {"application/json"},
		"X-Openai-Internal-Basispoints-Client-Product":       {"basispoints-excel-plugin"},
		"X-Openai-Internal-Basispoints-Client-Agent-Profile": {"excel"},
		"X-Openai-Internal-Basispoints-Client-Platform":      {"excel"}, "X-Openai-Internal-Basispoints-Client-Editor": {"excel"},
		"X-Openai-Internal-Basispoints-Client-Host": {"office"}, "X-Openai-Internal-Basispoints-Client-Runtime": {"desktop"},
		"X-Openai-Internal-Basispoints-Office-Host": {"Excel"}, "X-Openai-Internal-Basispoints-Browser-Name": {"unknown"},
	}
	platform := "PC"
	if runtime.GOOS == "darwin" {
		platform = "Mac"
	}
	req.Header.Set("X-Openai-Internal-Basispoints-Client-Platform-Class", platform)
	req.Header.Set("X-Openai-Internal-Basispoints-Office-Platform", platform)
	client := &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	// Normal requests reuse CPA's proxy/cancellation/logging. A credential or
	// plugin-specific proxy needs an explicit transport because the host HTTP
	// callback has no per-request proxy override.
	if p.host != nil && c.ProxyURL == "" && (material.Host.ProxyURL != "" || p.getConfig().ProxyURL == "") {
		client.Transport = catalogHostTransport{call: p.host, callbackID: material.HostCallbackID}
	} else {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		if proxy != "" {
			parsed, err := url.Parse(proxy)
			if err != nil {
				return modelCatalog{}, catalogError("model_catalog_proxy", "Invalid BPS catalog proxy", 502)
			}
			transport.Proxy = http.ProxyURL(parsed)
		}
		defer transport.CloseIdleConnections()
		client.Transport = transport
	}
	response, err := client.Do(req)
	if err != nil {
		if p.ctx.Err() != nil {
			return modelCatalog{}, catalogError("connection_lifecycle", "BPS model discovery cancelled", 499)
		}
		return modelCatalog{}, catalogError("model_catalog_transport", "Could not retrieve the BPS model catalog; check proxy and connectivity", 502)
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == 401 || response.StatusCode == 403:
		return modelCatalog{}, catalogError("model_catalog_access_denied", "BPS rejected model discovery for this credential", response.StatusCode)
	case response.StatusCode == 429:
		return modelCatalog{}, catalogError("model_catalog_rate_limited", "BPS model discovery is rate limited", 429)
	case response.StatusCode >= 500:
		return modelCatalog{}, catalogError("model_catalog_upstream", "BPS model catalog is temporarily unavailable", 502)
	case response.StatusCode != 200:
		return modelCatalog{}, catalogError("model_catalog_http_error", "BPS returned an unexpected model catalog status", 502)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (2<<20)+1))
	if err != nil || len(raw) > 2<<20 {
		return modelCatalog{}, catalogError("model_catalog_transport", "Could not read the BPS model catalog", 502)
	}
	return parseCatalog(raw)
}

func parseCatalog(raw []byte) (modelCatalog, *rpcError) {
	var reply struct {
		Allowed *bool `json:"allowed"`
		Catalog *struct {
			Models *[]struct {
				ID          string `json:"id"`
				Label       string `json:"label"`
				Description string `json:"description"`
				Efforts     []struct {
					Value string `json:"value"`
				} `json:"efforts"`
			} `json:"models"`
			DefaultModel string   `json:"default_model"`
			Restricted   []string `json:"restricted_models"`
		} `json:"model_catalog"`
	}
	invalid := func() (modelCatalog, *rpcError) {
		return modelCatalog{}, catalogError("model_catalog_invalid", "BPS returned an invalid model catalog; no fixed model fallback is used", 502)
	}
	if json.Unmarshal(raw, &reply) != nil || reply.Allowed == nil {
		return invalid()
	}
	if !*reply.Allowed {
		return modelCatalog{}, catalogError("model_catalog_access_denied", "This credential is not allowed to use BPS", 403)
	}
	if reply.Catalog == nil || reply.Catalog.Models == nil || len(*reply.Catalog.Models) > 512 {
		return invalid()
	}
	restricted := map[string]bool{}
	for _, id := range reply.Catalog.Restricted {
		restricted[id] = true
	}
	catalog := modelCatalog{Models: []modelInfo{}, DefaultModel: reply.Catalog.DefaultModel}
	seen := map[string]bool{}
	for _, model := range *reply.Catalog.Models {
		if model.ID == "" || len(model.ID) > 256 || strings.TrimSpace(model.ID) != model.ID || strings.ContainsAny(model.ID, " \t\r\n\x00") {
			return invalid()
		}
		if seen[model.ID] || restricted[model.ID] {
			continue
		}
		seen[model.ID] = true
		label := model.Label
		if label == "" {
			label = model.ID
		}
		info := modelInfo{ID: model.ID, Object: "model", OwnedBy: provider, Type: provider, DisplayName: label + " (BPS)", Description: model.Description,
			SupportedParameters:      []string{"tools", "tool_choice", "response_format"},
			SupportedInputModalities: []string{"text", "image"}, SupportedOutputModalities: []string{"text"}}
		levels := []string{}
		levelSeen := map[string]bool{}
		for _, effort := range model.Efforts {
			// Only advertise the upstream levels implemented by the unchanged
			// BPS bridge. "none" means default upstream, not disabled reasoning.
			switch effort.Value {
			case "low", "medium", "high", "xhigh":
				if !levelSeen[effort.Value] {
					levels = append(levels, effort.Value)
					levelSeen[effort.Value] = true
				}
			}
		}
		if len(levels) > 0 {
			info.Thinking = &thinkingSupport{Levels: levels}
			info.SupportedParameters = append(info.SupportedParameters, "reasoning_effort")
		}
		catalog.Models = append(catalog.Models, info)
	}
	if !seen[catalog.DefaultModel] {
		catalog.DefaultModel = ""
	}
	return catalog, nil
}

func (p *plugin) catalogStatus() ([]string, []map[string]any) {
	p.catalogMu.Lock()
	defer p.catalogMu.Unlock()
	ids := map[string]bool{}
	snapshots := []map[string]any{}
	for key, entry := range p.catalogs {
		record := map[string]any{"credential_fingerprint": key[:12], "fetched_at": entry.fetched, "next_fetch_after": entry.nextFetch,
			"stale": entry.stale, "last_error_code": entry.lastError, "default_model": entry.catalog.DefaultModel}
		names := []string{}
		if entry.err == nil && ((!entry.stale && time.Now().Before(entry.nextFetch)) || time.Since(entry.fetched) < catalogMaxStale) {
			for _, model := range entry.catalog.Models {
				names = append(names, model.ID)
				ids[model.ID] = true
			}
		}
		record["models"] = names
		snapshots = append(snapshots, record)
	}
	names := []string{}
	for id := range ids {
		names = append(names, id)
	}
	sort.Strings(names)
	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i]["credential_fingerprint"].(string) < snapshots[j]["credential_fingerprint"].(string)
	})
	return names, snapshots
}
