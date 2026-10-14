package agentpayprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/x402-foundation/x402/go/v2/types"
)

const (
	PaymentRequiredHeader  = "PAYMENT-REQUIRED"
	PaymentSignatureHeader = "PAYMENT-SIGNATURE"
	maxPaymentHeaderBytes  = 64 << 10
)

// DecisionKind 表示调用方应收到新的付款挑战，还是已经提交可交由 B5.2 处理的付款重试。
type DecisionKind uint8

const (
	DecisionChallenge DecisionKind = iota + 1
	DecisionPaymentRetry
)

// PaymentRetry 保存已解析但刻意未验真的 x402 付款。密码学验签和结算的唯一权威
// 仍然是 B2。
type PaymentRetry struct {
	Payload     types.PaymentPayload
	RawHeader   string
	PayloadJSON []byte
}

// Decision 是 B5.1 与后续 Prepare/Settle 流程之间的边界对象。
type Decision struct {
	Kind            DecisionKind
	PaymentRequired types.PaymentRequired
	PaymentRetry    *PaymentRetry
}

// Provider 检查受保护请求，但不会创建支付订单或执行结算；可被多个 HTTP 请求安全共享。
type Provider struct {
	loader PaymentRequiredLoader
}

func New(loader PaymentRequiredLoader) (*Provider, error) {
	if loader == nil {
		return nil, fmt.Errorf("%w: 缺少运行时配置加载器", ErrRuntimeConfigInvalid)
	}
	return &Provider{loader: loader}, nil
}

// Decide 将入站请求转换为新的 402 挑战或已解析的付款重试。只有缺少 Header 的分支
// 才会读取运行时配置。
func (p *Provider) Decide(ctx context.Context, paymentSignature string) (Decision, error) {
	if p == nil || p.loader == nil {
		return Decision{}, ErrRuntimeConfigInvalid
	}
	if strings.TrimSpace(paymentSignature) == "" {
		required, err := p.loader.Load(ctx)
		if err != nil {
			return Decision{}, err
		}
		if err := validatePaymentRequired(required); err != nil {
			return Decision{}, err
		}
		return Decision{Kind: DecisionChallenge, PaymentRequired: required}, nil
	}
	retry, err := ParsePaymentSignature(paymentSignature)
	if err != nil {
		return Decision{}, err
	}
	return Decision{Kind: DecisionPaymentRetry, PaymentRetry: &retry}, nil
}

// ParsePaymentSignature 仅校验 x402 v2 的 Wire 结构，不得将其误用为付款有效性
// 判断：Permit2 密码学验签以及金额、收款人等权威校验属于 B2/B3。
func ParsePaymentSignature(header string) (PaymentRetry, error) {
	header = strings.TrimSpace(header)
	if header == "" || len(header) > base64.StdEncoding.EncodedLen(maxPaymentHeaderBytes) {
		return PaymentRetry{}, ErrPaymentSignatureInvalid
	}
	raw, err := base64.StdEncoding.DecodeString(header)
	if err != nil || len(raw) == 0 || len(raw) > maxPaymentHeaderBytes {
		return PaymentRetry{}, ErrPaymentSignatureInvalid
	}
	var payload types.PaymentPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return PaymentRetry{}, ErrPaymentSignatureInvalid
	}
	if payload.X402Version != 2 || payload.Payload == nil || payload.Accepted.Scheme == "" || payload.Accepted.Network == "" {
		return PaymentRetry{}, ErrPaymentSignatureInvalid
	}
	if payload.Accepted.Scheme != "exact" || payload.Accepted.Extra["assetTransferMethod"] != "permit2" || payload.Accepted.Extra["paymentFlow"] != "upfront" {
		return PaymentRetry{}, ErrPaymentProfileUnsupported
	}
	return PaymentRetry{Payload: payload, RawHeader: header, PayloadJSON: append([]byte(nil), raw...)}, nil
}

// EncodePaymentRequired 按官方 x402 Wire 格式编码 v2 HTTP Header：即
// PaymentRequired JSON 的 Base64。
func EncodePaymentRequired(required types.PaymentRequired) (string, error) {
	if err := validatePaymentRequired(required); err != nil {
		return "", err
	}
	raw, err := json.Marshal(required)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrRuntimeConfigInvalid, err)
	}
	return base64.StdEncoding.EncodeToString(raw), nil
}
