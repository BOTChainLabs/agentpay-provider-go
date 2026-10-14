package agentpayprovider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type httpClientFunc func(*http.Request) (*http.Response, error)

func (f httpClientFunc) Do(request *http.Request) (*http.Response, error) { return f(request) }

func TestHTTPRuntimeConfigFetcherReadsB13Document(t *testing.T) {
	config := runtimeConfigFixture()
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://agentpay.example.com/agentpay/v1/payment-services/svc_1/runtime-config" {
			t.Fatalf("endpoint=%q", request.URL)
		}
		if request.Method != http.MethodGet || request.Header.Get("Accept") != "application/json" || request.Header.Get("If-None-Match") != `"old"` {
			t.Fatalf("unexpected request headers=%v", request.Header)
		}
		if request.Header.Get("X-API-Key") != "" {
			t.Fatal("B1.3 public endpoint must not receive an API key")
		}
		return runtimeConfigHTTPResponse(t, config), nil
	})
	fetcher, err := NewHTTPRuntimeConfigFetcher("https://agentpay.example.com/agentpay", "svc_1", client)
	if err != nil {
		t.Fatal(err)
	}
	result, err := fetcher.Fetch(context.Background(), `"old"`)
	if err != nil || result.NotModified || result.Config.ETag != config.ETag || result.Config.ServiceID != "svc_1" || result.Config.ConfigVersion != 1 || result.Config.PaymentRequired.Accepts[0].Amount != "100000" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestHTTPRuntimeConfigFetcherAndCacheUseConditionalETag(t *testing.T) {
	config := runtimeConfigFixture()
	calls := 0
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		switch calls {
		case 1:
			if request.Header.Get("If-None-Match") != "" {
				t.Fatalf("first request must not contain ETag: %q", request.Header.Get("If-None-Match"))
			}
			return runtimeConfigHTTPResponse(t, config), nil
		case 2:
			if request.Header.Get("If-None-Match") != config.ETag {
				t.Fatalf("conditional request etag=%q", request.Header.Get("If-None-Match"))
			}
			header := make(http.Header)
			header.Set("ETag", config.ETag)
			return &http.Response{StatusCode: http.StatusNotModified, Header: header, Body: io.NopCloser(strings.NewReader(""))}, nil
		default:
			t.Fatalf("unexpected fetch count %d", calls)
			return nil, nil
		}
	})
	fetcher, err := NewHTTPRuntimeConfigFetcher("https://agentpay.example.com", "svc_1", client)
	if err != nil {
		t.Fatal(err)
	}
	cache := NewCache(fetcher)
	base := time.Now()
	now := base
	cache.now = func() time.Time { return now }
	if _, err := cache.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = base.Add(config.TTL)
	if required, err := cache.Load(context.Background()); err != nil || required.Accepts[0].PayTo != config.PaymentRequired.Accepts[0].PayTo {
		t.Fatalf("304 reload required=%+v err=%v", required, err)
	}
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
}

func TestHTTPRuntimeConfigFetcherClassifiesB13Responses(t *testing.T) {
	for name, testcase := range map[string]struct {
		status int
		etag   string
		want   error
	}{
		"inactive 404":        {status: http.StatusNotFound, want: ErrServiceInactive},
		"temporary 503":       {status: http.StatusServiceUnavailable, want: ErrTemporaryConfig},
		"credential rejected": {status: http.StatusUnauthorized, want: ErrConfigUnauthorized},
		"304 requires etag":   {status: http.StatusNotModified, want: ErrRuntimeConfigInvalid},
		"unexpected 400":      {status: http.StatusBadRequest, want: ErrRuntimeConfigInvalid},
	} {
		t.Run(name, func(t *testing.T) {
			client := httpClientFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: testcase.status, Header: http.Header{"ETag": []string{testcase.etag}}, Body: io.NopCloser(strings.NewReader(""))}, nil
			})
			fetcher, err := NewHTTPRuntimeConfigFetcher("https://agentpay.example.com", "svc_1", client)
			if err != nil {
				t.Fatal(err)
			}
			_, err = fetcher.Fetch(context.Background(), `"known"`)
			if !errors.Is(err, testcase.want) {
				t.Fatalf("err=%v want=%v", err, testcase.want)
			}
		})
	}
}

func TestNewHTTPRuntimeConfigFetcherRejectsUnsafeTarget(t *testing.T) {
	for _, input := range []struct{ baseURL, serviceID string }{
		{"", "svc_1"},
		{"ftp://agentpay.example.com", "svc_1"},
		{"https://agentpay.example.com?redirect=https://other.example", "svc_1"},
		{"https://agentpay.example.com", "svc_1/other"},
	} {
		if _, err := NewHTTPRuntimeConfigFetcher(input.baseURL, input.serviceID, nil); !errors.Is(err, ErrRuntimeConfigInvalid) {
			t.Fatalf("baseURL=%q serviceID=%q err=%v", input.baseURL, input.serviceID, err)
		}
	}
}

func runtimeConfigHTTPResponse(t *testing.T, config RuntimeConfig) *http.Response {
	t.Helper()
	document := runtimeConfigDocument{
		X402Version:      config.PaymentRequired.X402Version,
		ServiceID:        "svc_1",
		ConfigVersion:    1,
		ETag:             config.ETag,
		Resource:         *config.PaymentRequired.Resource,
		Accepts:          config.PaymentRequired.Accepts,
		Extensions:       config.PaymentRequired.Extensions,
		HTTPMethod:       http.MethodGet,
		SettlementTiming: "SETTLE_BEFORE_RELEASE",
		EffectiveAt:      time.Now().UTC(),
		TTLSeconds:       int(config.TTL / time.Second),
		MaxStaleSeconds:  int(config.MaxStale / time.Second),
	}
	body, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	header := make(http.Header)
	header.Set("ETag", config.ETag)
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: io.NopCloser(strings.NewReader(string(body)))}
}
