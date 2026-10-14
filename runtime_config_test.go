package agentpayprovider

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/x402-foundation/x402/go/v2/types"
)

type fakeConfigFetcher struct {
	result RuntimeConfigResult
	err    error
	calls  int
	etag   string
}

func (f *fakeConfigFetcher) Fetch(_ context.Context, etag string) (RuntimeConfigResult, error) {
	f.calls++
	f.etag = etag
	return f.result, f.err
}

func runtimeConfigFixture() RuntimeConfig {
	return RuntimeConfig{
		ETag:     `"revision-1"`,
		TTL:      time.Second,
		MaxStale: 30 * time.Second,
		PaymentRequired: types.PaymentRequired{
			X402Version: 2,
			Resource:    &types.ResourceInfo{URL: "https://provider.example/weather", ServiceName: "Weather"},
			Accepts: []types.PaymentRequirements{{
				Scheme: "exact", Network: "eip155:968", Asset: "0x75edC9335175Fc0552D51D48439F229c10420fe3",
				Amount: "100000", PayTo: "0x71d066Eb5C9402BB8bA8ED406D5D51edd320F862",
				Extra: map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"},
			}},
		},
	}
}

func TestCacheUsesETagBoundedStaleAndFailsClosed(t *testing.T) {
	config := runtimeConfigFixture()
	fetcher := &fakeConfigFetcher{result: RuntimeConfigResult{Config: config}}
	cache := NewCache(fetcher)
	base := time.Now()
	now := base
	cache.now = func() time.Time { return now }

	if _, err := cache.Load(context.Background()); err != nil || fetcher.calls != 1 {
		t.Fatalf("initial load calls=%d err=%v", fetcher.calls, err)
	}
	now = base.Add(500 * time.Millisecond)
	if _, err := cache.Load(context.Background()); err != nil || fetcher.calls != 1 {
		t.Fatalf("fresh config must not fetch again: calls=%d err=%v", fetcher.calls, err)
	}

	now = base.Add(time.Second)
	fetcher.result = RuntimeConfigResult{Config: RuntimeConfig{ETag: config.ETag}, NotModified: true}
	if _, err := cache.Load(context.Background()); err != nil || fetcher.etag != config.ETag {
		t.Fatalf("304 refresh etag=%q err=%v", fetcher.etag, err)
	}

	now = base.Add(2 * time.Second)
	fetcher.err = ErrTemporaryConfig
	if required, err := cache.Load(context.Background()); err != nil || required.Accepts[0].Amount != "100000" {
		t.Fatalf("short transient outage must use bounded stale config: required=%+v err=%v", required, err)
	}

	now = base.Add(32 * time.Second)
	if _, err := cache.Load(context.Background()); !errors.Is(err, ErrConfigStale) {
		t.Fatalf("expired stale config must fail closed, got %v", err)
	}

	fetcher.err = ErrServiceInactive
	if _, err := cache.Load(context.Background()); !errors.Is(err, ErrServiceInactive) {
		t.Fatalf("known suspension must be returned, got %v", err)
	}
	fetcher.err = ErrTemporaryConfig
	if _, err := cache.Load(context.Background()); !errors.Is(err, ErrConfigStale) {
		t.Fatalf("suspension must clear reusable config, got %v", err)
	}
}

func TestCacheInvalidatesOnExplicitNotification(t *testing.T) {
	config := runtimeConfigFixture()
	fetcher := &fakeConfigFetcher{result: RuntimeConfigResult{Config: config}}
	cache := NewCache(fetcher)
	if _, err := cache.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	cache.Invalidate()
	fetcher.err = ErrTemporaryConfig
	if _, err := cache.Load(context.Background()); !errors.Is(err, ErrConfigStale) {
		t.Fatalf("invalidated config must not be returned during outage, got %v", err)
	}
}

func TestCacheReturnsCompleteIndependentRuntimeSnapshot(t *testing.T) {
	config := runtimeConfigFixture()
	config.ServiceID = "svc_1"
	config.ConfigVersion = 9
	cache := NewCache(&fakeConfigFetcher{result: RuntimeConfigResult{Config: config}})
	first, err := cache.LoadRuntimeConfig(context.Background())
	if err != nil || first.ServiceID != "svc_1" || first.ConfigVersion != 9 {
		t.Fatalf("first=%+v err=%v", first, err)
	}
	first.PaymentRequired.Resource.URL = "https://mutated.example"
	second, err := cache.LoadRuntimeConfig(context.Background())
	if err != nil || second.PaymentRequired.Resource.URL != "https://provider.example/weather" {
		t.Fatalf("cache snapshot was mutated: second=%+v err=%v", second, err)
	}
}

func TestCacheRejectsMismatchedNotModifiedETag(t *testing.T) {
	config := runtimeConfigFixture()
	fetcher := &fakeConfigFetcher{result: RuntimeConfigResult{Config: config}}
	cache := NewCache(fetcher)
	base := time.Now()
	now := base
	cache.now = func() time.Time { return now }
	if _, err := cache.Load(context.Background()); err != nil {
		t.Fatal(err)
	}
	now = base.Add(time.Second)
	fetcher.result = RuntimeConfigResult{Config: RuntimeConfig{ETag: `"another"`}, NotModified: true}
	if _, err := cache.Load(context.Background()); !errors.Is(err, ErrRuntimeConfigInvalid) {
		t.Fatalf("mismatched 304 must clear cache, got %v", err)
	}
}
