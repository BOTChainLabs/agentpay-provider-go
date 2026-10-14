package agentpayprovider

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/x402-foundation/x402/go/v2/types"
)

const maxRuntimeConfigResponseBytes = 256 << 10

// HTTPClient 是 Runtime Config HTTP 传输的最小依赖，可由测试或调用方替换。
type HTTPClient interface {
	Do(*http.Request) (*http.Response, error)
}

// HTTPRuntimeConfigFetcher 通过 B1.3 的公开接口按 ETag 拉取收费配置。
// 接口不要求 API Key；SDK 因而不会把任意凭证发送给该公开读取端点。
type HTTPRuntimeConfigFetcher struct {
	client    HTTPClient
	endpoint  string
	serviceID string
}

// NewHTTPRuntimeConfigFetcher 构造 B1.3 Runtime Config Fetcher。baseURL 可以
// 包含部署前缀，例如 https://gateway.example.com/agentpay。
func NewHTTPRuntimeConfigFetcher(baseURL, serviceID string, client HTTPClient) (*HTTPRuntimeConfigFetcher, error) {
	baseURL = strings.TrimSpace(baseURL)
	serviceID = strings.TrimSpace(serviceID)
	if baseURL == "" || serviceID == "" || strings.ContainsAny(serviceID, "/?#") {
		return nil, ErrRuntimeConfigInvalid
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, ErrRuntimeConfigInvalid
	}
	endpoint, err := url.JoinPath(baseURL, "v1", "payment-services", serviceID, "runtime-config")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrRuntimeConfigInvalid, err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPRuntimeConfigFetcher{client: client, endpoint: endpoint, serviceID: serviceID}, nil
}

// Fetch 使用 If-None-Match 进行条件读取。重定向会被拒绝，避免 SDK 因部署错误
// 跟随到与已审核 AgentPay 端点不同的来源。
func (f *HTTPRuntimeConfigFetcher) Fetch(ctx context.Context, etag string) (RuntimeConfigResult, error) {
	if f == nil || f.client == nil || f.endpoint == "" || f.serviceID == "" {
		return RuntimeConfigResult{}, ErrRuntimeConfigInvalid
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, f.endpoint, nil)
	if err != nil {
		return RuntimeConfigResult{}, fmt.Errorf("%w: %v", ErrRuntimeConfigInvalid, err)
	}
	req.Header.Set("Accept", "application/json")
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}

	response, err := doWithoutRedirect(f.client, req)
	if err != nil {
		return RuntimeConfigResult{}, fmt.Errorf("%w: %v", ErrTemporaryConfig, err)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
		return decodeRuntimeConfigResponse(response, f.serviceID)
	case http.StatusNotModified:
		etag := response.Header.Get("ETag")
		if etag == "" {
			return RuntimeConfigResult{}, ErrRuntimeConfigInvalid
		}
		return RuntimeConfigResult{Config: RuntimeConfig{ETag: etag}, NotModified: true}, nil
	case http.StatusNotFound, http.StatusGone, http.StatusConflict:
		return RuntimeConfigResult{}, ErrServiceInactive
	case http.StatusUnauthorized, http.StatusForbidden:
		return RuntimeConfigResult{}, ErrConfigUnauthorized
	default:
		if response.StatusCode >= http.StatusInternalServerError {
			return RuntimeConfigResult{}, fmt.Errorf("%w: HTTP %d", ErrTemporaryConfig, response.StatusCode)
		}
		return RuntimeConfigResult{}, fmt.Errorf("%w: HTTP %d", ErrRuntimeConfigInvalid, response.StatusCode)
	}
}

func doWithoutRedirect(client HTTPClient, request *http.Request) (*http.Response, error) {
	standardClient, ok := client.(*http.Client)
	if !ok {
		return client.Do(request)
	}
	copy := *standardClient
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return copy.Do(request)
}

type runtimeConfigDocument struct {
	X402Version      int                         `json:"x402Version"`
	ServiceID        string                      `json:"service_id"`
	ConfigVersion    int                         `json:"config_version"`
	ETag             string                      `json:"etag"`
	Resource         types.ResourceInfo          `json:"resource"`
	Accepts          []types.PaymentRequirements `json:"accepts"`
	Extensions       map[string]interface{}      `json:"extensions"`
	HTTPMethod       string                      `json:"http_method"`
	SettlementTiming string                      `json:"settlement_timing"`
	EffectiveAt      time.Time                   `json:"effective_at"`
	TTLSeconds       int                         `json:"ttl_seconds"`
	MaxStaleSeconds  int                         `json:"max_stale_seconds"`
}

func decodeRuntimeConfigResponse(response *http.Response, serviceID string) (RuntimeConfigResult, error) {
	body, err := io.ReadAll(io.LimitReader(response.Body, maxRuntimeConfigResponseBytes+1))
	if err != nil || len(body) > maxRuntimeConfigResponseBytes {
		return RuntimeConfigResult{}, ErrRuntimeConfigInvalid
	}
	var document runtimeConfigDocument
	if err := json.Unmarshal(body, &document); err != nil {
		return RuntimeConfigResult{}, ErrRuntimeConfigInvalid
	}
	if document.ServiceID != serviceID || document.ConfigVersion < 1 || document.ETag == "" || response.Header.Get("ETag") != document.ETag ||
		document.EffectiveAt.IsZero() || document.SettlementTiming != "SETTLE_BEFORE_RELEASE" || !validHTTPMethod(document.HTTPMethod) ||
		document.TTLSeconds < 1 || document.MaxStaleSeconds < document.TTLSeconds || document.MaxStaleSeconds > 300 {
		return RuntimeConfigResult{}, ErrRuntimeConfigInvalid
	}
	required := types.PaymentRequired{
		X402Version: document.X402Version,
		Resource:    &document.Resource,
		Accepts:     document.Accepts,
		Extensions:  document.Extensions,
	}
	config := RuntimeConfig{
		ServiceID:       document.ServiceID,
		ConfigVersion:   document.ConfigVersion,
		PaymentRequired: required,
		ETag:            document.ETag,
		TTL:             time.Duration(document.TTLSeconds) * time.Second,
		MaxStale:        time.Duration(document.MaxStaleSeconds) * time.Second,
	}
	if err := validateRuntimeConfig(config); err != nil {
		return RuntimeConfigResult{}, err
	}
	return RuntimeConfigResult{Config: config}, nil
}

func validHTTPMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return true
	default:
		return false
	}
}
