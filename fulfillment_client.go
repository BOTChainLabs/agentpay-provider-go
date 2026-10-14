package agentpayprovider

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxFulfillmentResultBytes = 8 << 20

// FulfillmentClient 是 B5.2 到 B6 履约接口的窄边界。它不承载业务 Handler，
// 以便租约和结果持久化规则能在没有 HTTP 服务端依赖的场景下复用。
type FulfillmentClient interface {
	StartFulfillment(context.Context, StartFulfillmentInput) (FulfillmentStartResult, error)
	StoreFulfillmentResult(context.Context, StoreFulfillmentResultInput) (StoredFulfillmentResult, error)
	CompleteFulfillment(context.Context, CompleteFulfillmentInput) (CompletedFulfillment, error)
	FailFulfillment(context.Context, FailFulfillmentInput) (FailedFulfillment, error)
}

type StartFulfillmentInput struct {
	PaymentOrderID string
	RequestHash    string
	// LeaseOwner 可选。通常留空，由 B6 从当前 Provider API Key 推导，避免 SDK
	// 保存或猜测服务端 credential ID。
	LeaseOwner     string
	IdempotencyKey string
}

// FulfillmentStartResult.Reused 表示该订单已经 SUCCEEDED；此时绝不能重新执行业务。
type FulfillmentStartResult struct {
	PaymentOrderID    string
	PaymentIdentifier string
	InvocationID      string
	Status            string
	AttemptCount      int
	RequestHash       string
	LeaseToken        string
	LeaseExpiresAt    time.Time
	Reused            bool
	ResultHash        string
	ResultCacheKey    string
	FinishedAt        time.Time
}

type StoreFulfillmentResultInput struct {
	PaymentOrderID string
	ResultCacheKey string
	Body           []byte
	IdempotencyKey string
}

type StoredFulfillmentResult struct {
	PaymentOrderID string
	ResultCacheKey string
	ResultHash     string
	ByteSize       int64
}

type CompleteFulfillmentInput struct {
	PaymentOrderID string
	LeaseToken     string
	ResultHash     string
	ResultCacheKey string
	IdempotencyKey string
}

type CompletedFulfillment struct {
	PaymentOrderID string    `json:"payment_order_id"`
	Status         string    `json:"fulfillment_status"`
	ResultHash     string    `json:"result_hash"`
	ResultCacheKey string    `json:"result_cache_key"`
	FinishedAt     time.Time `json:"finished_at"`
}

type FailFulfillmentInput struct {
	PaymentOrderID string
	LeaseToken     string
	FailureCode    string
	FailureReason  string
	Retryable      bool
	IdempotencyKey string
}

type FailedFulfillment struct {
	PaymentOrderID string     `json:"payment_order_id"`
	Status         string     `json:"fulfillment_status"`
	NextRetryAt    *time.Time `json:"next_retry_at"`
	AttemptCount   int        `json:"attempt_count"`
}

// StartFulfillment 调用 B6.1 的唯一租约接口。只有拿到 LeaseToken 的调用方才有
// 资格运行 Provider 业务；Reused 结果由上层从结果仓库读取。
func (c *HTTPPaymentOrderClient) StartFulfillment(ctx context.Context, input StartFulfillmentInput) (FulfillmentStartResult, error) {
	if c == nil || c.client == nil || c.settleBaseURL == "" || !validPaymentIdentifier(input.PaymentOrderID) ||
		!validSHA256(input.RequestHash) || !validIdempotencyKey(input.IdempotencyKey) || len(strings.TrimSpace(input.LeaseOwner)) > 128 {
		return FulfillmentStartResult{}, ErrPaymentOrderClientInvalid
	}
	endpoint, err := fulfillmentEndpoint(c.settleBaseURL, input.PaymentOrderID, ":start-fulfillment")
	if err != nil {
		return FulfillmentStartResult{}, err
	}
	body := struct {
		RequestHash string `json:"request_hash"`
		LeaseOwner  string `json:"lease_owner,omitempty"`
	}{RequestHash: input.RequestHash, LeaseOwner: strings.TrimSpace(input.LeaseOwner)}
	var output struct {
		PaymentOrderID    string `json:"payment_order_id"`
		PaymentIdentifier string `json:"payment_identifier"`
		InvocationID      string `json:"invocation_id"`
		Status            string `json:"fulfillment_status"`
		AttemptCount      int    `json:"attempt_count"`
		RequestHash       string `json:"request_hash"`
		Lease             *struct {
			Token     string    `json:"token"`
			ExpiresAt time.Time `json:"expires_at"`
		} `json:"fulfillment_lease"`
		ResultHash     string    `json:"result_hash"`
		ResultCacheKey string    `json:"result_cache_key"`
		FinishedAt     time.Time `json:"finished_at"`
	}
	if err := c.postFulfillment(ctx, endpoint, input.IdempotencyKey, body, &output); err != nil {
		return FulfillmentStartResult{}, err
	}
	if output.PaymentOrderID != input.PaymentOrderID || output.InvocationID != input.PaymentOrderID || output.RequestHash != input.RequestHash ||
		!validPaymentIdentifier(output.PaymentIdentifier) || output.AttemptCount < 0 {
		return FulfillmentStartResult{}, ErrPaymentOrderClientInvalid
	}
	result := FulfillmentStartResult{PaymentOrderID: output.PaymentOrderID, PaymentIdentifier: output.PaymentIdentifier,
		InvocationID: output.InvocationID, Status: output.Status, AttemptCount: output.AttemptCount, RequestHash: output.RequestHash,
		ResultHash: output.ResultHash, ResultCacheKey: output.ResultCacheKey, FinishedAt: output.FinishedAt}
	switch {
	case output.Status == "RUNNING" && output.Lease != nil && strings.TrimSpace(output.Lease.Token) != "" && !output.Lease.ExpiresAt.IsZero():
		result.LeaseToken, result.LeaseExpiresAt = output.Lease.Token, output.Lease.ExpiresAt
	case output.Status == "SUCCEEDED" && output.Lease == nil && validSHA256(output.ResultHash) && validResultCacheKey(output.ResultCacheKey):
		result.Reused = true
	default:
		return FulfillmentStartResult{}, ErrPaymentOrderClientInvalid
	}
	return result, nil
}

func (c *HTTPPaymentOrderClient) StoreFulfillmentResult(ctx context.Context, input StoreFulfillmentResultInput) (StoredFulfillmentResult, error) {
	if c == nil || c.client == nil || c.settleBaseURL == "" || !validPaymentIdentifier(input.PaymentOrderID) ||
		!validResultCacheKey(input.ResultCacheKey) || len(input.Body) > maxFulfillmentResultBytes || !validIdempotencyKey(input.IdempotencyKey) {
		return StoredFulfillmentResult{}, ErrPaymentOrderClientInvalid
	}
	endpoint, err := fulfillmentEndpoint(c.settleBaseURL, input.PaymentOrderID, ":store-result")
	if err != nil {
		return StoredFulfillmentResult{}, err
	}
	body := struct {
		ResultCacheKey string `json:"result_cache_key"`
		BodyBase64     string `json:"body_base64"`
	}{ResultCacheKey: input.ResultCacheKey, BodyBase64: base64.StdEncoding.EncodeToString(input.Body)}
	var output struct {
		PaymentOrderID string `json:"payment_order_id"`
		ResultCacheKey string `json:"result_cache_key"`
		ResultHash     string `json:"result_hash"`
		ByteSize       int64  `json:"byte_size"`
	}
	if err := c.postFulfillment(ctx, endpoint, input.IdempotencyKey, body, &output); err != nil {
		return StoredFulfillmentResult{}, err
	}
	if output.PaymentOrderID != input.PaymentOrderID || output.ResultCacheKey != input.ResultCacheKey || !validSHA256(output.ResultHash) ||
		output.ByteSize != int64(len(input.Body)) {
		return StoredFulfillmentResult{}, ErrPaymentOrderClientInvalid
	}
	return StoredFulfillmentResult(output), nil
}

func (c *HTTPPaymentOrderClient) CompleteFulfillment(ctx context.Context, input CompleteFulfillmentInput) (CompletedFulfillment, error) {
	if c == nil || c.client == nil || c.settleBaseURL == "" || !validPaymentIdentifier(input.PaymentOrderID) ||
		strings.TrimSpace(input.LeaseToken) == "" || !validSHA256(input.ResultHash) || !validResultCacheKey(input.ResultCacheKey) || !validIdempotencyKey(input.IdempotencyKey) {
		return CompletedFulfillment{}, ErrPaymentOrderClientInvalid
	}
	endpoint, err := fulfillmentEndpoint(c.settleBaseURL, input.PaymentOrderID, ":complete-fulfillment")
	if err != nil {
		return CompletedFulfillment{}, err
	}
	body := struct {
		LeaseToken     string `json:"fulfillment_lease_token"`
		ResultHash     string `json:"result_hash"`
		ResultCacheKey string `json:"result_cache_key"`
	}{LeaseToken: input.LeaseToken, ResultHash: input.ResultHash, ResultCacheKey: input.ResultCacheKey}
	var output CompletedFulfillment
	if err := c.postFulfillment(ctx, endpoint, input.IdempotencyKey, body, &output); err != nil {
		return CompletedFulfillment{}, err
	}
	if output.PaymentOrderID != input.PaymentOrderID || output.Status != "SUCCEEDED" || output.ResultHash != input.ResultHash ||
		output.ResultCacheKey != input.ResultCacheKey || output.FinishedAt.IsZero() {
		return CompletedFulfillment{}, ErrPaymentOrderClientInvalid
	}
	return output, nil
}

func (c *HTTPPaymentOrderClient) FailFulfillment(ctx context.Context, input FailFulfillmentInput) (FailedFulfillment, error) {
	if c == nil || c.client == nil || c.settleBaseURL == "" || !validPaymentIdentifier(input.PaymentOrderID) ||
		strings.TrimSpace(input.LeaseToken) == "" || !validFailureCode(input.FailureCode) || !validFailureReason(input.FailureReason) || !validIdempotencyKey(input.IdempotencyKey) {
		return FailedFulfillment{}, ErrPaymentOrderClientInvalid
	}
	endpoint, err := fulfillmentEndpoint(c.settleBaseURL, input.PaymentOrderID, ":fail-fulfillment")
	if err != nil {
		return FailedFulfillment{}, err
	}
	body := struct {
		LeaseToken    string `json:"fulfillment_lease_token"`
		FailureCode   string `json:"failure_code"`
		FailureReason string `json:"failure_reason"`
		Retryable     bool   `json:"retryable"`
	}{LeaseToken: input.LeaseToken, FailureCode: input.FailureCode, FailureReason: input.FailureReason, Retryable: input.Retryable}
	var output FailedFulfillment
	if err := c.postFulfillment(ctx, endpoint, input.IdempotencyKey, body, &output); err != nil {
		return FailedFulfillment{}, err
	}
	if output.PaymentOrderID != input.PaymentOrderID || (output.Status != "RETRYABLE" && output.Status != "COMPENSATION_REQUIRED") || output.AttemptCount < 1 {
		return FailedFulfillment{}, ErrPaymentOrderClientInvalid
	}
	return output, nil
}

func (c *HTTPPaymentOrderClient) postFulfillment(ctx context.Context, endpoint, idempotencyKey string, body, output any) error {
	status, _, err := c.postStatus(ctx, endpoint, idempotencyKey, body, output)
	if err != nil {
		return mapFulfillmentError(err)
	}
	if status != http.StatusOK {
		return ErrPaymentOrderClientInvalid
	}
	return nil
}

func fulfillmentEndpoint(baseURL, paymentOrderID, action string) (string, error) {
	endpoint, err := url.JoinPath(baseURL, paymentOrderID+action)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrPaymentOrderClientInvalid, err)
	}
	return endpoint, nil
}

func mapFulfillmentError(err error) error {
	remote, ok := err.(*SDKError)
	if !ok || remote.Code == CodeAgentPayUnavailable {
		return err
	}
	switch remote.Code {
	case "FULFILLMENT_LEASE_HELD", "FULFILLMENT_RESULT_UNCONFIRMED", "STATE_VERSION_CONFLICT":
		return &SDKError{Code: CodeFulfillmentInProgress, Retryable: true, Cause: remote}
	default:
		if remote.Retryable {
			return &SDKError{Code: CodeAgentPayUnavailable, Retryable: true, Cause: remote}
		}
		return &SDKError{Code: CodeFulfillmentRejected, Retryable: false, Cause: remote}
	}
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func validResultCacheKey(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 512 && !strings.ContainsAny(value, " \t\r\n")
}

func validFailureCode(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 64 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func validFailureReason(value string) bool {
	return len(strings.TrimSpace(value)) <= 512 && !strings.ContainsAny(value, "\r\n")
}
