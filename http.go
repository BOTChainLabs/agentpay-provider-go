package agentpayprovider

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/x402-foundation/x402/go/v2/types"
)

type paymentRetryContextKey struct{}

// Middleware 保护一个 HTTP Handler：未付款请求返回 402；已解析的付款重试继续传给
// 下游，由 B5.2 通过 PaymentRetryFromContext 消费。
func (p *Provider) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if next == nil {
			writeError(writer, classifyError(ErrRuntimeConfigInvalid))
			return
		}
		// 多个付款 Header 的语义不明确；不能让框架悄悄选择其中一个并把其他值
		// 丢弃，以免不同代理层对同一请求做出不一致的付款判断。
		signatures := request.Header.Values(PaymentSignatureHeader)
		if len(signatures) > 1 {
			writeError(writer, classifyError(ErrPaymentSignatureInvalid))
			return
		}
		paymentSignature := ""
		if len(signatures) == 1 {
			paymentSignature = signatures[0]
		}
		decision, err := p.Decide(request.Context(), paymentSignature)
		if err != nil {
			writeError(writer, classifyError(err))
			return
		}
		switch decision.Kind {
		case DecisionChallenge:
			writeChallenge(writer, decision.PaymentRequired)
		case DecisionPaymentRetry:
			// 付款载荷只保存在当前请求上下文，避免它进入日志或长期缓存。
			ctx := context.WithValue(request.Context(), paymentRetryContextKey{}, *decision.PaymentRetry)
			next.ServeHTTP(writer, request.WithContext(ctx))
		default:
			writeError(writer, classifyError(ErrRuntimeConfigInvalid))
		}
	})
}

// PaymentRetryFromContext 取回 Middleware 传入的已解析付款重试。
func PaymentRetryFromContext(ctx context.Context) (PaymentRetry, bool) {
	retry, ok := ctx.Value(paymentRetryContextKey{}).(PaymentRetry)
	return retry, ok
}

func writeChallenge(writer http.ResponseWriter, required types.PaymentRequired) {
	header, err := EncodePaymentRequired(required)
	if err != nil {
		writeError(writer, classifyError(err))
		return
	}
	// 402 响应包含支付条件且可能随配置变化，禁止中间缓存复用旧收款规则。
	writer.Header().Set(PaymentRequiredHeader, header)
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(http.StatusPaymentRequired)
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{
			"code":      "PAYMENT_REQUIRED",
			"message":   "payment required",
			"retryable": false,
		},
	})
}

func writeError(writer http.ResponseWriter, sdkErr *SDKError) {
	status := http.StatusServiceUnavailable
	if sdkErr != nil {
		switch sdkErr.Code {
		case CodeServiceInactive:
			status = http.StatusNotFound
		case CodeRuntimeConfigInvalid, CodePaymentSignatureInvalid, CodePaymentProfileUnsupported:
			status = http.StatusBadRequest
		}
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	if sdkErr == nil {
		sdkErr = &SDKError{Code: CodeConfigUnavailable, Retryable: true}
	}
	_ = json.NewEncoder(writer).Encode(map[string]any{
		"error": map[string]any{
			"code":      sdkErr.Code,
			"message":   errorMessage(sdkErr.Code),
			"retryable": sdkErr.Retryable,
		},
	})
}

func errorMessage(code Code) string {
	switch code {
	case CodeServiceInactive:
		return "payment service is unavailable"
	case CodeRuntimeConfigInvalid:
		return "payment configuration is invalid"
	case CodePaymentSignatureInvalid:
		return "payment signature is invalid"
	case CodePaymentProfileUnsupported:
		return "payment profile is unsupported"
	default:
		return "payment configuration is temporarily unavailable"
	}
}
