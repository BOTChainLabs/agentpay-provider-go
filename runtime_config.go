package agentpayprovider

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/x402-foundation/x402/go/v2/types"
)

// RuntimeConfig 是生成 402 所需的最小审核配置。B1.3 的 HTTP 响应可适配为该
// 类型，使公共 SDK 不耦合 AgentPay 的内部数据库模型。
type RuntimeConfig struct {
	// ServiceID 与 ConfigVersion 是生成这份 402 的权威服务快照标识。B5.2
	// 必须把它们原样带入 Prepare，不能根据付款载荷猜测价格或配置版本。
	ServiceID       string
	ConfigVersion   int
	PaymentRequired types.PaymentRequired
	ETag            string
	TTL             time.Duration
	MaxStale        time.Duration
}

// RuntimeConfigResult 是条件刷新结果。NotModified 表示远端接受 ETag，当前
// 缓存条目仍然有效。
type RuntimeConfigResult struct {
	Config      RuntimeConfig
	NotModified bool
}

// RuntimeConfigFetcher 是 Runtime Config 来源的窄集成边界。SDK 提供 B1.3
// HTTP 实现，测试或其他运行环境也可以替换为自己的实现。
type RuntimeConfigFetcher interface {
	Fetch(context.Context, string) (RuntimeConfigResult, error)
}

// PaymentRequiredLoader 让 Provider 获取当前 x402 挑战。Cache 实现该接口；
// 测试或非 HTTP 环境可提供其他实现。
type PaymentRequiredLoader interface {
	Load(context.Context) (types.PaymentRequired, error)
}

// RuntimeConfigLoader 是 B5.2 读取完整收费快照的边界。保留独立接口使
// Provider SDK 不依赖内部 payment_service 数据模型，也方便应用以测试替身接入。
type RuntimeConfigLoader interface {
	LoadRuntimeConfig(context.Context) (RuntimeConfig, error)
}

// Cache 是单个服务的进程内 Runtime Config 缓存。只有已确认的短暂故障才可复用
// 陈旧条目；已知暂停、鉴权失败或非法配置都会清空条目并失败关闭。
type Cache struct {
	mu        sync.Mutex
	fetcher   RuntimeConfigFetcher
	now       func() time.Time
	current   *RuntimeConfig
	validated time.Time
}

func NewCache(fetcher RuntimeConfigFetcher) *Cache {
	return &Cache{fetcher: fetcher, now: time.Now}
}

// NewHTTPProvider 使用 B1.3 的公开 Runtime Config 接口创建 Provider 和其
// 缓存。调用方应保留返回的 Cache，以便在收到服务暂停通知时调用 Invalidate。
func NewHTTPProvider(baseURL, serviceID string, client HTTPClient) (*Provider, *Cache, error) {
	fetcher, err := NewHTTPRuntimeConfigFetcher(baseURL, serviceID, client)
	if err != nil {
		return nil, nil, err
	}
	cache := NewCache(fetcher)
	provider, err := New(cache)
	if err != nil {
		return nil, nil, err
	}
	return provider, cache, nil
}

// Invalidate 应在收到带外的暂停或配置变更通知后调用，确保旧收费规则不会继续可用。
func (c *Cache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.current = nil
	c.validated = time.Time{}
}

func (c *Cache) Load(ctx context.Context) (types.PaymentRequired, error) {
	config, err := c.LoadRuntimeConfig(ctx)
	if err != nil {
		return types.PaymentRequired{}, err
	}
	return clonePaymentRequired(config.PaymentRequired), nil
}

// LoadRuntimeConfig 返回可用于 Prepare 的完整、不可变副本。缓存仍沿用 B5.1
// 的“仅短暂故障可使用有限陈旧值”规则；服务暂停或配置非法时始终失败关闭。
func (c *Cache) LoadRuntimeConfig(ctx context.Context) (RuntimeConfig, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fetcher == nil {
		return RuntimeConfig{}, ErrRuntimeConfigInvalid
	}

	now := c.now()
	if c.current != nil && fresh(c.current.TTL, now.Sub(c.validated)) {
		return cloneRuntimeConfig(*c.current), nil
	}

	etag := ""
	if c.current != nil {
		etag = c.current.ETag
	}
	result, err := c.fetcher.Fetch(ctx, etag)
	now = c.now()
	age := now.Sub(c.validated)
	if err != nil {
		if ctx.Err() != nil {
			return RuntimeConfig{}, ctx.Err()
		}
		if errors.Is(err, ErrTemporaryConfig) && c.current != nil && usableStale(c.current.MaxStale, age) {
			return cloneRuntimeConfig(*c.current), nil
		}
		if !errors.Is(err, ErrTemporaryConfig) {
			c.clear()
			return RuntimeConfig{}, err
		}
		return RuntimeConfig{}, fmt.Errorf("%w: %v", ErrConfigStale, err)
	}

	if result.NotModified {
		if c.current == nil || result.Config.ETag == "" || result.Config.ETag != c.current.ETag {
			c.clear()
			return RuntimeConfig{}, ErrRuntimeConfigInvalid
		}
		c.validated = now
		return cloneRuntimeConfig(*c.current), nil
	}
	if err := validateRuntimeConfig(result.Config); err != nil {
		c.clear()
		return RuntimeConfig{}, err
	}
	copy := cloneRuntimeConfig(result.Config)
	c.current = &copy
	c.validated = now
	return cloneRuntimeConfig(copy), nil
}

func (c *Cache) clear() {
	c.current = nil
	c.validated = time.Time{}
}

func fresh(ttl, age time.Duration) bool {
	return age >= 0 && age < ttl
}

func usableStale(maxStale, age time.Duration) bool {
	return age >= 0 && age <= maxStale
}

func validateRuntimeConfig(config RuntimeConfig) error {
	if config.ETag == "" || config.TTL < time.Second || config.MaxStale < config.TTL || config.MaxStale > 5*time.Minute {
		return ErrRuntimeConfigInvalid
	}
	if err := validatePaymentRequired(config.PaymentRequired); err != nil {
		return err
	}
	return nil
}

func validatePaymentRequired(required types.PaymentRequired) error {
	if required.X402Version != 2 || required.Resource == nil || required.Resource.URL == "" || len(required.Accepts) != 1 {
		return ErrRuntimeConfigInvalid
	}
	accepted := required.Accepts[0]
	if accepted.Scheme != "exact" || accepted.Network == "" || accepted.Asset == "" || accepted.Amount == "" || accepted.PayTo == "" ||
		accepted.Extra["assetTransferMethod"] != "permit2" || accepted.Extra["paymentFlow"] != "upfront" {
		return ErrRuntimeConfigInvalid
	}
	return nil
}

// 深拷贝避免调用方经由 map 或 slice 修改缓存中的挑战内容，进而悄然篡改未来的
// 402 响应。
func cloneRuntimeConfig(config RuntimeConfig) RuntimeConfig {
	config.PaymentRequired = clonePaymentRequired(config.PaymentRequired)
	return config
}

func clonePaymentRequired(required types.PaymentRequired) types.PaymentRequired {
	copy := required
	if required.Resource != nil {
		resource := *required.Resource
		resource.Tags = append([]string(nil), required.Resource.Tags...)
		copy.Resource = &resource
	}
	copy.Accepts = append([]types.PaymentRequirements(nil), required.Accepts...)
	for index := range copy.Accepts {
		if extra := required.Accepts[index].Extra; extra != nil {
			copy.Accepts[index].Extra = make(map[string]interface{}, len(extra))
			for key, value := range extra {
				copy.Accepts[index].Extra[key] = value
			}
		}
	}
	if required.Extensions != nil {
		copy.Extensions = make(map[string]interface{}, len(required.Extensions))
		for key, value := range required.Extensions {
			copy.Extensions[key] = value
		}
	}
	return copy
}
