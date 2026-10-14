package agentpayprovider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestHTTPPaymentOrderClientPrepareAndSettle(t *testing.T) {
	settlementHeaders := make(http.Header)
	settlementHeaders.Set("PAYMENT-RESPONSE", "payment-response-base64")
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.Header.Get("X-API-Key") != "ak_test" || request.Header.Get("Idempotency-Key") == "" {
			t.Fatalf("missing credential or idempotency key: %v", request.Header)
		}
		switch request.URL.Path {
		case "/agentpay/v1/payment-orders:prepare":
			var body struct {
				ServiceID     string `json:"service_id"`
				ConfigVersion int    `json:"config_version"`
				Request       struct {
					RequestHashVersion string `json:"request_hash_version"`
				} `json:"request"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.ServiceID != "svc_1" || body.ConfigVersion != 2 || body.Request.RequestHashVersion != requestHashVersionV1 {
				t.Fatalf("invalid prepare: %+v err=%v", body, err)
			}
			return orderHTTPResponse(http.StatusOK, nil, `{"data":{"payment_order_id":"ord_1","service_id":"svc_1","config_version":2,"payment_status":"PREPARED","expires_at":"2026-10-12T00:00:00Z","settlement_lease_acquired":true,"settlement_lease_token":"lease_1"}}`), nil
		case "/agentpay/v1/payment-orders/ord_1:settle":
			return orderHTTPResponse(http.StatusOK, settlementHeaders, `{"data":{"payment_order_id":"ord_1","payment_status":"PAID","collection_status":"CONFIRMED","tx_hash":"0xtx","payer":"0xpayer","retry_after_seconds":0}}`), nil
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
			return nil, nil
		}
	})
	orders, err := NewHTTPPaymentOrderClient("https://agentpay.example/agentpay", "ak_test", client)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := orders.Prepare(context.Background(), PreparePaymentOrderInput{
		ServiceID: "svc_1", ConfigVersion: 2, PaymentIdentifier: "pay_1", PaymentSignature: "signature", IdempotencyKey: "prepare_1",
		Request: RequestBinding{Method: http.MethodPost, ResourceURL: "https://provider.example/research", RequestHash: strings.Repeat("a", 64), RequestHashVersion: requestHashVersionV1},
	})
	if err != nil || prepared.PaymentOrderID != "ord_1" || !prepared.SettlementLeaseAcquired || prepared.SettlementLeaseToken != "lease_1" || !prepared.ExpiresAt.Equal(time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	settled, err := orders.Settle(context.Background(), SettlePaymentOrderInput{PaymentOrderID: prepared.PaymentOrderID, SettlementLeaseToken: prepared.SettlementLeaseToken, IdempotencyKey: "settle_1"})
	if err != nil || settled.State != SettlementConfirmed || settled.PaymentResponse != "payment-response-base64" || settled.TransactionHash != "0xtx" {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
}

func TestHTTPPaymentOrderClientTreatsUnknownAsPending(t *testing.T) {
	transport := httpClientFunc(func(*http.Request) (*http.Response, error) {
		return orderHTTPResponse(http.StatusAccepted, nil, `{"data":{"payment_order_id":"ord_1","payment_status":"SETTLING","collection_status":"UNKNOWN","tx_hash":"0xtx","retry_after_seconds":12}}`), nil
	})
	client, err := NewHTTPPaymentOrderClient("https://agentpay.example", "ak_test", transport)
	if err != nil {
		t.Fatal(err)
	}
	settled, err := client.Settle(context.Background(), SettlePaymentOrderInput{PaymentOrderID: "ord_1", SettlementLeaseToken: "lease_1", IdempotencyKey: "settle_1"})
	if err != nil || settled.State != SettlementPending || settled.RetryAfterSeconds != 12 || settled.PaymentResponse != "" {
		t.Fatalf("settled=%+v err=%v", settled, err)
	}
}

func TestHTTPPaymentOrderClientFulfillmentLifecycle(t *testing.T) {
	requestHash := strings.Repeat("a", 64)
	resultHash := fulfillmentBodyHash([]byte(`{"ok":true}`))
	client := httpClientFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPost || request.Header.Get("X-API-Key") != "ak_test" || request.Header.Get("Idempotency-Key") == "" {
			t.Fatalf("invalid fulfillment request: %s %v", request.Method, request.Header)
		}
		switch request.URL.Path {
		case "/v1/payment-orders/ord_1:start-fulfillment":
			return orderHTTPResponse(http.StatusOK, nil, `{"data":{"payment_order_id":"ord_1","payment_identifier":"pay_1","invocation_id":"ord_1","fulfillment_status":"RUNNING","attempt_count":1,"request_hash":"`+requestHash+`","fulfillment_lease":{"token":"lease_1","expires_at":"2026-10-13T00:01:00Z"}}}`), nil
		case "/v1/payment-orders/ord_1:store-result":
			var body struct {
				ResultCacheKey string `json:"result_cache_key"`
				BodyBase64     string `json:"body_base64"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil || body.ResultCacheKey != "agentpay-result://proj_1/ord_1/response" || body.BodyBase64 == "" {
				t.Fatalf("invalid store request: %+v err=%v", body, err)
			}
			return orderHTTPResponse(http.StatusOK, nil, `{"data":{"payment_order_id":"ord_1","result_cache_key":"agentpay-result://proj_1/ord_1/response","result_hash":"`+resultHash+`","byte_size":11}}`), nil
		case "/v1/payment-orders/ord_1:complete-fulfillment":
			return orderHTTPResponse(http.StatusOK, nil, `{"data":{"payment_order_id":"ord_1","fulfillment_status":"SUCCEEDED","result_hash":"`+resultHash+`","result_cache_key":"agentpay-result://proj_1/ord_1/response","finished_at":"2026-10-13T00:00:00Z"}}`), nil
		case "/v1/payment-orders/ord_1:fail-fulfillment":
			return orderHTTPResponse(http.StatusOK, nil, `{"data":{"payment_order_id":"ord_1","fulfillment_status":"RETRYABLE","next_retry_at":"2026-10-13T00:02:00Z","attempt_count":1}}`), nil
		default:
			t.Fatalf("unexpected path: %s", request.URL.Path)
			return nil, nil
		}
	})
	orders, err := NewHTTPPaymentOrderClient("https://agentpay.example", "ak_test", client)
	if err != nil {
		t.Fatal(err)
	}
	started, err := orders.StartFulfillment(context.Background(), StartFulfillmentInput{PaymentOrderID: "ord_1", RequestHash: requestHash, IdempotencyKey: "start_1"})
	if err != nil || started.LeaseToken != "lease_1" || started.Reused {
		t.Fatalf("started=%+v err=%v", started, err)
	}
	stored, err := orders.StoreFulfillmentResult(context.Background(), StoreFulfillmentResultInput{PaymentOrderID: "ord_1", ResultCacheKey: "agentpay-result://proj_1/ord_1/response", Body: []byte(`{"ok":true}`), IdempotencyKey: "store_1"})
	if err != nil || stored.ResultHash != resultHash || stored.ByteSize != 11 {
		t.Fatalf("stored=%+v err=%v", stored, err)
	}
	completed, err := orders.CompleteFulfillment(context.Background(), CompleteFulfillmentInput{PaymentOrderID: "ord_1", LeaseToken: started.LeaseToken, ResultHash: resultHash, ResultCacheKey: stored.ResultCacheKey, IdempotencyKey: "complete_1"})
	if err != nil || completed.Status != "SUCCEEDED" || completed.FinishedAt.IsZero() {
		t.Fatalf("completed=%+v err=%v", completed, err)
	}
	failed, err := orders.FailFulfillment(context.Background(), FailFulfillmentInput{PaymentOrderID: "ord_1", LeaseToken: "lease_2", FailureCode: "HANDLER_FAILED", FailureReason: "provider handler execution failed", Retryable: true, IdempotencyKey: "fail_1"})
	if err != nil || failed.Status != "RETRYABLE" || failed.NextRetryAt == nil {
		t.Fatalf("failed=%+v err=%v", failed, err)
	}
}

func orderHTTPResponse(status int, header http.Header, body string) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	header.Set("Content-Type", "application/json")
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}
