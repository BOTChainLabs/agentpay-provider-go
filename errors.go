package agentpayprovider

import "errors"

// Code 是 Provider SDK 对外稳定、可由程序识别的错误码。
type Code string

const (
	CodeConfigUnavailable         Code = "RUNTIME_CONFIG_UNAVAILABLE"
	CodeServiceInactive           Code = "PAYMENT_SERVICE_INACTIVE"
	CodeRuntimeConfigInvalid      Code = "RUNTIME_CONFIG_INVALID"
	CodePaymentSignatureInvalid   Code = "INVALID_PAYMENT_SIGNATURE"
	CodePaymentProfileUnsupported Code = "UNSUPPORTED_PAYMENT_PROFILE"
	CodePaymentPreparationFailed  Code = "PAYMENT_PREPARATION_FAILED"
	CodeSettlementRejected        Code = "PAYMENT_SETTLEMENT_REJECTED"
	CodeSettlementInProgress      Code = "PAYMENT_SETTLEMENT_IN_PROGRESS"
	CodeAgentPayUnavailable       Code = "AGENTPAY_UNAVAILABLE"
	CodeFulfillmentInProgress     Code = "FULFILLMENT_IN_PROGRESS"
	CodeFulfillmentRejected       Code = "FULFILLMENT_REJECTED"
	CodeFulfillmentResultMissing  Code = "FULFILLMENT_RESULT_MISSING"
	CodeFulfillmentResultInvalid  Code = "FULFILLMENT_RESULT_INVALID"
	CodeFulfillmentCompletionWait Code = "FULFILLMENT_COMPLETION_PENDING"
)

var (
	// ErrTemporaryConfig 表示配置源的短暂故障；仅此类错误允许缓存有限度地
	// 使用陈旧快照。
	ErrTemporaryConfig = errors.New("运行时配置暂时不可用")
	// ErrServiceInactive 表示服务被明确暂停或移除，必须立刻使本地收费规则失效。
	ErrServiceInactive = errors.New("收费服务不可用")
	// ErrConfigUnauthorized 表示 Provider 凭证被拒绝。
	ErrConfigUnauthorized = errors.New("运行时配置凭证无效")
	// ErrRuntimeConfigInvalid 表示配置结果不可信或格式不合法。
	ErrRuntimeConfigInvalid = errors.New("运行时配置无效")
	// ErrConfigStale 表示没有任何最近成功校验的配置仍处于 max-stale 窗口内。
	ErrConfigStale = errors.New("运行时配置超过最大陈旧时间")
	// ErrPaymentSignatureInvalid 只表示 Wire 格式或基础结构错误，不代表已做
	// 密码学验签。
	ErrPaymentSignatureInvalid = errors.New("PAYMENT-SIGNATURE 无效")
	// ErrPaymentProfileUnsupported 表示重试未采用公开 V1 的
	// exact + permit2 + upfront Profile。
	ErrPaymentProfileUnsupported = errors.New("不支持的付款 Profile")
	// ErrPaymentOrderClientInvalid 表示 B5.2 的 AgentPay HTTP 客户端参数或响应
	// 不可信。它不是付款方可通过重试修复的链上错误。
	ErrPaymentOrderClientInvalid = errors.New("支付订单客户端配置或响应无效")
	// ErrPaymentRequestTooLarge 在计算请求指纹时触发。SDK 此时拒绝执行下游
	// Handler，避免仅为支付而无上限地缓冲业务请求体。
	ErrPaymentRequestTooLarge = errors.New("支付请求体超过 SDK 限制")
	// ErrFulfillmentResultNotFound 仅由 Provider 配置的结果仓库返回。SDK 收到它时
	// 不会在已成功的订单上重新执行 Handler。
	ErrFulfillmentResultNotFound  = errors.New("履约结果不存在")
	ErrFulfillmentResultTooLarge  = errors.New("履约结果超过 SDK 限制")
	ErrFulfillmentResultInvalid   = errors.New("履约结果无效")
	ErrFulfillmentResultConflict  = errors.New("履约结果与已保存结果冲突")
	ErrFulfillmentExecutorInvalid = errors.New("履约执行器无效")
)

// SDKError 仅保留可安全返回 HTTP 调用方的错误事实。原始原因可通过 Unwrap
// 供应用日志使用，但绝不能序列化进 HTTP 响应。
type SDKError struct {
	Code      Code
	Retryable bool
	Cause     error
}

func (e *SDKError) Error() string {
	if e == nil {
		return ""
	}
	return string(e.Code)
}

func (e *SDKError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func classifyError(err error) *SDKError {
	var sdkErr *SDKError
	if errors.As(err, &sdkErr) {
		return sdkErr
	}
	switch {
	case errors.Is(err, ErrServiceInactive), errors.Is(err, ErrConfigUnauthorized):
		return &SDKError{Code: CodeServiceInactive, Retryable: false, Cause: err}
	case errors.Is(err, ErrRuntimeConfigInvalid):
		return &SDKError{Code: CodeRuntimeConfigInvalid, Retryable: false, Cause: err}
	case errors.Is(err, ErrPaymentSignatureInvalid):
		return &SDKError{Code: CodePaymentSignatureInvalid, Retryable: false, Cause: err}
	case errors.Is(err, ErrPaymentProfileUnsupported):
		return &SDKError{Code: CodePaymentProfileUnsupported, Retryable: false, Cause: err}
	default:
		return &SDKError{Code: CodeConfigUnavailable, Retryable: true, Cause: err}
	}
}
