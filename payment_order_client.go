package agentpayprovider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxPaymentOrderResponseBytes = 256 << 10

// PaymentOrderClient 是 B5.2 与 AgentPay 订单 API 的窄边界。业务应用可在
// 单元测试中提供替身；SDK 不需要知道订单数据库或 Facilitator 的内部实现。
type PaymentOrderClient interface {
	Prepare(context.Context, PreparePaymentOrderInput) (PreparedPaymentOrder, error)
	Settle(context.Context, SettlePaymentOrderInput) (SettlementResult, error)
}

// ConfirmedSettlementReader is an optional extension to PaymentOrderClient. A
// payment order can become PAID after the request that submitted :settle has
// already returned PENDING. A later signed retry must not get another
// settlement lease, but it still needs the original PAYMENT-RESPONSE before it
// can safely deliver the protected resource.
//
// Keeping this as a separate interface avoids breaking custom PaymentOrderClient
// test doubles and integrations that only implement Prepare and Settle.
type ConfirmedSettlementReader interface {
	LoadConfirmedSettlement(context.Context, string) (SettlementResult, error)
}

type PreparePaymentOrderInput struct {
	ServiceID         string
	ConfigVersion     int
	PaymentIdentifier string
	PaymentSignature  string
	Request           RequestBinding
	IdempotencyKey    string
}

type PreparedPaymentOrder struct {
	PaymentOrderID           string
	ServiceID                string
	ConfigVersion            int
	PaymentStatus            string
	ExpiresAt                time.Time
	SettlementLeaseToken     string
	SettlementLeaseAcquired  bool
	SettlementLeaseExpiresAt *time.Time
}

type SettlePaymentOrderInput struct {
	PaymentOrderID       string
	SettlementLeaseToken string
	IdempotencyKey       string
}

type SettlementState string

const (
	SettlementConfirmed SettlementState = "CONFIRMED"
	SettlementPending   SettlementState = "PENDING"
)

// SettlementResult 只陈述平台已确认的资金事实。PaymentResponse 保存原始
// PAYMENT-RESPONSE Header，Provider 成功交付时应原样返回，不能自行构造。
type SettlementResult struct {
	State             SettlementState
	PaymentOrderID    string
	PaymentStatus     string
	CollectionStatus  string
	TransactionHash   string
	Payer             string
	RetryAfterSeconds int
	PaymentResponse   string
}

// HTTPPaymentOrderClient 是 AgentPay Order API 的安全 HTTP 实现。
type HTTPPaymentOrderClient struct {
	client          HTTPClient
	prepareEndpoint string
	settleBaseURL   string
	apiKey          string
}

func NewHTTPPaymentOrderClient(baseURL, apiKey string, client HTTPClient) (*HTTPPaymentOrderClient, error) {
	baseURL = strings.TrimSpace(baseURL)
	if strings.TrimSpace(apiKey) == "" {
		return nil, ErrPaymentOrderClientInvalid
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return nil, ErrPaymentOrderClientInvalid
	}
	prepareEndpoint, err := url.JoinPath(baseURL, "v1", "payment-orders:prepare")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPaymentOrderClientInvalid, err)
	}
	settleBaseURL, err := url.JoinPath(baseURL, "v1", "payment-orders")
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrPaymentOrderClientInvalid, err)
	}
	if client == nil {
		client = http.DefaultClient
	}
	return &HTTPPaymentOrderClient{client: client, prepareEndpoint: prepareEndpoint, settleBaseURL: settleBaseURL, apiKey: apiKey}, nil
}

func (c *HTTPPaymentOrderClient) Prepare(ctx context.Context, input PreparePaymentOrderInput) (PreparedPaymentOrder, error) {
	if c == nil || c.client == nil || c.prepareEndpoint == "" || !validOrderInput(input.ServiceID, input.ConfigVersion, input.PaymentIdentifier, input.PaymentSignature, input.IdempotencyKey) ||
		input.Request.Method == "" || input.Request.ResourceURL == "" || len(input.Request.RequestHash) != 64 || input.Request.RequestHashVersion != requestHashVersionV1 {
		return PreparedPaymentOrder{}, ErrPaymentOrderClientInvalid
	}
	body := struct {
		ServiceID         string `json:"service_id"`
		ConfigVersion     int    `json:"config_version"`
		PaymentIdentifier string `json:"payment_identifier"`
		PaymentSignature  string `json:"payment_signature"`
		Request           struct {
			Method             string `json:"method"`
			ResourceURL        string `json:"resource_url"`
			RequestHash        string `json:"request_hash"`
			RequestHashVersion string `json:"request_hash_version"`
		} `json:"request"`
	}{ServiceID: input.ServiceID, ConfigVersion: input.ConfigVersion, PaymentIdentifier: input.PaymentIdentifier, PaymentSignature: input.PaymentSignature}
	body.Request.Method = input.Request.Method
	body.Request.ResourceURL = input.Request.ResourceURL
	body.Request.RequestHash = input.Request.RequestHash
	body.Request.RequestHashVersion = input.Request.RequestHashVersion

	var output struct {
		PaymentOrderID           string     `json:"payment_order_id"`
		ServiceID                string     `json:"service_id"`
		ConfigVersion            int        `json:"config_version"`
		PaymentStatus            string     `json:"payment_status"`
		ExpiresAt                time.Time  `json:"expires_at"`
		SettlementLeaseToken     string     `json:"settlement_lease_token"`
		SettlementLeaseAcquired  bool       `json:"settlement_lease_acquired"`
		SettlementLeaseExpiresAt *time.Time `json:"settlement_lease_expires_at"`
	}
	if err := c.post(ctx, c.prepareEndpoint, input.IdempotencyKey, body, http.StatusOK, &output); err != nil {
		return PreparedPaymentOrder{}, mapOrderError(err, true)
	}
	if !validPaymentIdentifier(output.PaymentOrderID) || output.ServiceID != input.ServiceID || output.ConfigVersion != input.ConfigVersion || output.PaymentStatus == "" || output.ExpiresAt.IsZero() ||
		(output.SettlementLeaseAcquired && output.SettlementLeaseToken == "") {
		return PreparedPaymentOrder{}, ErrPaymentOrderClientInvalid
	}
	return PreparedPaymentOrder{
		PaymentOrderID: output.PaymentOrderID, ServiceID: output.ServiceID, ConfigVersion: output.ConfigVersion,
		PaymentStatus: output.PaymentStatus, ExpiresAt: output.ExpiresAt, SettlementLeaseToken: output.SettlementLeaseToken,
		SettlementLeaseAcquired: output.SettlementLeaseAcquired, SettlementLeaseExpiresAt: output.SettlementLeaseExpiresAt,
	}, nil
}

func (c *HTTPPaymentOrderClient) Settle(ctx context.Context, input SettlePaymentOrderInput) (SettlementResult, error) {
	if c == nil || c.client == nil || c.settleBaseURL == "" || !validPaymentIdentifier(input.PaymentOrderID) ||
		strings.TrimSpace(input.SettlementLeaseToken) == "" || !validIdempotencyKey(input.IdempotencyKey) {
		return SettlementResult{}, ErrPaymentOrderClientInvalid
	}
	endpoint, err := url.JoinPath(c.settleBaseURL, input.PaymentOrderID+":settle")
	if err != nil {
		return SettlementResult{}, fmt.Errorf("%w: %v", ErrPaymentOrderClientInvalid, err)
	}
	body := struct {
		SettlementLeaseToken string `json:"settlement_lease_token"`
	}{SettlementLeaseToken: input.SettlementLeaseToken}
	var output struct {
		PaymentOrderID    string `json:"payment_order_id"`
		PaymentStatus     string `json:"payment_status"`
		CollectionStatus  string `json:"collection_status"`
		TxHash            string `json:"tx_hash"`
		Payer             string `json:"payer"`
		RetryAfterSeconds int    `json:"retry_after_seconds"`
	}
	status, headers, err := c.postStatus(ctx, endpoint, input.IdempotencyKey, body, &output)
	if err != nil {
		return SettlementResult{}, mapOrderError(err, false)
	}
	if output.PaymentOrderID != input.PaymentOrderID || output.PaymentStatus == "" || output.CollectionStatus == "" || output.RetryAfterSeconds < 0 {
		return SettlementResult{}, ErrPaymentOrderClientInvalid
	}
	result := SettlementResult{
		PaymentOrderID: output.PaymentOrderID, PaymentStatus: output.PaymentStatus, CollectionStatus: output.CollectionStatus,
		TransactionHash: output.TxHash, Payer: output.Payer, RetryAfterSeconds: output.RetryAfterSeconds,
	}
	switch {
	case status == http.StatusOK && output.PaymentStatus == "PAID" && output.CollectionStatus == "CONFIRMED":
		response := strings.TrimSpace(headers.Get("PAYMENT-RESPONSE"))
		if response == "" {
			return SettlementResult{}, ErrPaymentOrderClientInvalid
		}
		result.State, result.PaymentResponse = SettlementConfirmed, response
		return result, nil
	case status == http.StatusAccepted || output.PaymentStatus == "SETTLING" && (output.CollectionStatus == "BROADCAST" || output.CollectionStatus == "UNKNOWN"):
		// 任何非确认态都按 pending 处理，即使远端未来增加更多中间状态，也不会被
		// SDK 误判成“已付款可履约”。
		result.State = SettlementPending
		return result, nil
	default:
		return SettlementResult{}, ErrPaymentOrderClientInvalid
	}
}

// LoadConfirmedSettlement reads the immutable confirmed settlement fact for an
// existing order. It is used only after Prepare reports PAID without issuing a
// settlement lease; it never creates a new authorization or calls :settle.
func (c *HTTPPaymentOrderClient) LoadConfirmedSettlement(ctx context.Context, paymentOrderID string) (SettlementResult, error) {
	if c == nil || c.client == nil || c.settleBaseURL == "" || !validPaymentIdentifier(paymentOrderID) {
		return SettlementResult{}, ErrPaymentOrderClientInvalid
	}
	endpoint, err := url.JoinPath(c.settleBaseURL, paymentOrderID)
	if err != nil {
		return SettlementResult{}, fmt.Errorf("%w: %v", ErrPaymentOrderClientInvalid, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return SettlementResult{}, fmt.Errorf("%w: %v", ErrPaymentOrderClientInvalid, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("X-API-Key", c.apiKey)
	response, err := doWithoutRedirect(c.client, request)
	if err != nil {
		return SettlementResult{}, &SDKError{Code: CodeAgentPayUnavailable, Retryable: true, Cause: err}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPaymentOrderResponseBytes+1))
	if err != nil || len(raw) > maxPaymentOrderResponseBytes {
		return SettlementResult{}, &SDKError{Code: CodeAgentPayUnavailable, Retryable: true, Cause: err}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return SettlementResult{}, mapOrderError(decodeOrderAPIError(response.StatusCode, raw), false)
	}
	var envelope struct {
		Data struct {
			PaymentOrderID   string `json:"payment_order_id"`
			PaymentStatus    string `json:"payment_status"`
			CollectionStatus string `json:"collection_status"`
			TxHash           string `json:"tx_hash"`
			PayerAddress     string `json:"payer_address"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Data.PaymentOrderID != paymentOrderID ||
		envelope.Data.PaymentStatus != "PAID" || envelope.Data.CollectionStatus != "CONFIRMED" ||
		strings.TrimSpace(envelope.Data.TxHash) == "" || strings.TrimSpace(envelope.Data.PayerAddress) == "" {
		return SettlementResult{}, ErrPaymentOrderClientInvalid
	}
	paymentResponse := strings.TrimSpace(response.Header.Get("PAYMENT-RESPONSE"))
	if paymentResponse == "" {
		return SettlementResult{}, ErrPaymentOrderClientInvalid
	}
	return SettlementResult{
		State: SettlementConfirmed, PaymentOrderID: envelope.Data.PaymentOrderID,
		PaymentStatus: envelope.Data.PaymentStatus, CollectionStatus: envelope.Data.CollectionStatus,
		TransactionHash: envelope.Data.TxHash, Payer: envelope.Data.PayerAddress, PaymentResponse: paymentResponse,
	}, nil
}

func (c *HTTPPaymentOrderClient) post(ctx context.Context, endpoint, idempotencyKey string, body any, expectedStatus int, output any) error {
	status, _, err := c.postStatus(ctx, endpoint, idempotencyKey, body, output)
	if err != nil {
		return err
	}
	if status != expectedStatus {
		return ErrPaymentOrderClientInvalid
	}
	return nil
}

func (c *HTTPPaymentOrderClient) postStatus(ctx context.Context, endpoint, idempotencyKey string, body any, output any) (int, http.Header, error) {
	encoded, err := json.Marshal(body)
	if err != nil {
		return 0, nil, fmt.Errorf("%w: 请求序列化失败", ErrPaymentOrderClientInvalid)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(encoded))
	if err != nil {
		return 0, nil, fmt.Errorf("%w: %v", ErrPaymentOrderClientInvalid, err)
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", c.apiKey)
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := doWithoutRedirect(c.client, request)
	if err != nil {
		return 0, nil, &SDKError{Code: CodeAgentPayUnavailable, Retryable: true, Cause: err}
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxPaymentOrderResponseBytes+1))
	if err != nil || len(raw) > maxPaymentOrderResponseBytes {
		return 0, nil, &SDKError{Code: CodeAgentPayUnavailable, Retryable: true, Cause: err}
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return response.StatusCode, response.Header, decodeOrderAPIError(response.StatusCode, raw)
	}
	var envelope struct {
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || len(envelope.Data) == 0 || json.Unmarshal(envelope.Data, output) != nil {
		return 0, nil, ErrPaymentOrderClientInvalid
	}
	return response.StatusCode, response.Header, nil
}

func decodeOrderAPIError(status int, raw []byte) error {
	var envelope struct {
		Error struct {
			Code      string `json:"code"`
			Retryable bool   `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil || envelope.Error.Code == "" {
		return &SDKError{Code: CodeAgentPayUnavailable, Retryable: status >= http.StatusInternalServerError, Cause: ErrPaymentOrderClientInvalid}
	}
	return &SDKError{Code: Code(envelope.Error.Code), Retryable: envelope.Error.Retryable}
}

func mapOrderError(err error, prepare bool) error {
	remote, ok := err.(*SDKError)
	if !ok {
		return err
	}
	if remote.Code == CodeAgentPayUnavailable {
		return remote
	}
	if remote.Code == "IDEMPOTENCY_IN_PROGRESS" || remote.Code == "SETTLEMENT_LEASE_REJECTED" {
		// 这些冲突不代表付款失败，而是另一个同订单请求尚在推进。SDK 的上层
		// 必须查询/重试原订单，绝不能生成新的 PAYMENT-SIGNATURE。
		return &SDKError{Code: CodeSettlementInProgress, Retryable: true, Cause: remote}
	}
	if remote.Retryable {
		return &SDKError{Code: CodeAgentPayUnavailable, Retryable: true, Cause: remote}
	}
	if prepare {
		return &SDKError{Code: CodePaymentPreparationFailed, Retryable: false, Cause: remote}
	}
	return &SDKError{Code: CodeSettlementRejected, Retryable: false, Cause: remote}
}

func validOrderInput(serviceID string, configVersion int, paymentIdentifier, paymentSignature, idempotencyKey string) bool {
	return validPaymentIdentifier(serviceID) && configVersion > 0 && validPaymentIdentifier(paymentIdentifier) &&
		strings.TrimSpace(paymentSignature) != "" && validIdempotencyKey(idempotencyKey)
}

func validPaymentIdentifier(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '-' {
			continue
		}
		return false
	}
	return true
}

func validIdempotencyKey(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 128 && !strings.ContainsAny(value, "\r\n")
}
