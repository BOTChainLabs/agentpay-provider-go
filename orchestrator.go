package agentpayprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
)

// PaymentIdentifierResolver 允许 Provider 将自己的稳定业务调用标识传入订单。
// 默认实现由规范化 PaymentPayload 推导，保证同一付款重试不会创建第二张订单。
type PaymentIdentifierResolver func(PaymentRetry) (string, error)

// PaymentOrchestrator 只完成 B5.2 中可独立交付的 Prepare → Settle 编排。
// 它刻意不接收业务 Handler：没有 B6 fulfillment lease 时，任何“PAID 后直接执行”
// 的便利 API 都会绕开履约并发控制，因而是不安全的。
type PaymentOrchestrator struct {
	Configs           RuntimeConfigLoader
	Orders            PaymentOrderClient
	RequestBinder     RequestBinder
	PaymentIdentifier PaymentIdentifierResolver
}

type PaymentOrchestrationResult struct {
	State          SettlementState
	Order          PreparedPaymentOrder
	Settlement     SettlementResult
	RequestBinding RequestBinding
}

// Settle 处理一份已由 B5.1 解析过的 PAYMENT-SIGNATURE。调用方只能在返回
// SettlementConfirmed 后进入 B6 StartFulfillment；SettlementPending 与没有
// lease 的情况都必须停止在此处，不能调用业务 Handler。
func (o PaymentOrchestrator) Settle(ctx context.Context, request *http.Request, retry PaymentRetry) (PaymentOrchestrationResult, error) {
	if o.Configs == nil || o.Orders == nil || request == nil || strings.TrimSpace(retry.RawHeader) == "" {
		return PaymentOrchestrationResult{}, ErrPaymentOrderClientInvalid
	}
	config, err := o.Configs.LoadRuntimeConfig(ctx)
	if err != nil {
		return PaymentOrchestrationResult{}, err
	}
	if strings.TrimSpace(config.ServiceID) == "" || config.ConfigVersion < 1 || config.PaymentRequired.Resource == nil ||
		strings.TrimSpace(config.PaymentRequired.Resource.URL) == "" || validatePaymentRequired(config.PaymentRequired) != nil {
		return PaymentOrchestrationResult{}, ErrRuntimeConfigInvalid
	}
	binding, err := o.RequestBinder.Bind(request, config.ServiceID, config.ConfigVersion, config.PaymentRequired.Resource.URL)
	if err != nil {
		return PaymentOrchestrationResult{}, err
	}
	fingerprint, err := paymentPayloadFingerprint(retry)
	if err != nil {
		return PaymentOrchestrationResult{}, err
	}
	resolver := o.PaymentIdentifier
	if resolver == nil {
		resolver = DefaultPaymentIdentifier
	}
	paymentIdentifier, err := resolver(retry)
	if err != nil || !validPaymentIdentifier(paymentIdentifier) {
		return PaymentOrchestrationResult{}, ErrPaymentOrderClientInvalid
	}
	prepared, err := o.Orders.Prepare(ctx, PreparePaymentOrderInput{
		ServiceID: config.ServiceID, ConfigVersion: config.ConfigVersion, PaymentIdentifier: paymentIdentifier,
		PaymentSignature: retry.RawHeader, Request: binding, IdempotencyKey: "prepare_" + fingerprint,
	})
	if err != nil {
		return PaymentOrchestrationResult{}, err
	}
	result := PaymentOrchestrationResult{Order: prepared, State: SettlementPending, RequestBinding: binding}
	if !prepared.SettlementLeaseAcquired {
		// 另一个实例可能正在广播或等待确认。此分支宁可要求调用方稍后重试，
		// 也不能绕过 lease 再发一次结算，或提前把请求交给业务服务。
		return result, nil
	}
	settlement, err := o.Orders.Settle(ctx, SettlePaymentOrderInput{
		PaymentOrderID: prepared.PaymentOrderID, SettlementLeaseToken: prepared.SettlementLeaseToken,
		IdempotencyKey: settlementIdempotencyKey(prepared.PaymentOrderID),
	})
	if err != nil {
		return PaymentOrchestrationResult{}, err
	}
	result.State, result.Settlement = settlement.State, settlement
	return result, nil
}

// DefaultPaymentIdentifier 基于与 B3 解密后相同的规范化 PaymentPayload 计算。
// 它不暴露签名明文；完整 SHA-256 留在服务端幂等关系中，冲突概率可忽略。
func DefaultPaymentIdentifier(retry PaymentRetry) (string, error) {
	fingerprint, err := paymentPayloadFingerprint(retry)
	if err != nil {
		return "", err
	}
	return "pay_" + fingerprint, nil
}

func paymentPayloadFingerprint(retry PaymentRetry) (string, error) {
	canonical, err := json.Marshal(retry.Payload)
	if err != nil || len(canonical) == 0 {
		return "", ErrPaymentSignatureInvalid
	}
	digest := sha256.Sum256(canonical)
	return hex.EncodeToString(digest[:]), nil
}

func settlementIdempotencyKey(paymentOrderID string) string {
	// 不把订单 ID（可能携带业务可关联信息）直接作为外发 Header；哈希后仍可在
	// 同一订单的 HTTP 重试中稳定复用，且长度始终小于平台的 128 字符限制。
	digest := sha256.Sum256([]byte(paymentOrderID))
	return "settle_" + hex.EncodeToString(digest[:])
}

func (r PaymentOrchestrationResult) Confirmed() bool { return r.State == SettlementConfirmed }
