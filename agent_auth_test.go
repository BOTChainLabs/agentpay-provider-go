package agentpayprovider

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestAgentAuthHeadersAreOptionalForLegacyRequests(t *testing.T) {
	r, _ := http.NewRequest(http.MethodGet, "https://provider.test/resource", nil)
	m, err := agentAuthFromRequest(r)
	if err != nil || m != nil {
		t.Fatalf("legacy request: metadata=%v err=%v", m, err)
	}
	r.Header.Set(paymentExecutionHeader, "ape_123")
	if _, err := agentAuthFromRequest(r); err != ErrAgentAuthMetadataInvalid {
		t.Fatalf("partial headers: %v", err)
	}
	r.Header.Set(claimReceiptHeader, "signed.receipt.value")
	m, err = agentAuthFromRequest(r)
	if err != nil || m == nil || m.PaymentExecutionID != "ape_123" || m.ClaimReceipt != "signed.receipt.value" {
		t.Fatalf("complete headers: metadata=%v err=%v", m, err)
	}
	r.Header.Add(claimReceiptHeader, "second.receipt.value")
	if _, err := agentAuthFromRequest(r); err != ErrAgentAuthMetadataInvalid {
		t.Fatalf("duplicate headers: %v", err)
	}
}

func TestAgentAuthMetadataReachesBothOrderRequests(t *testing.T) {
	metadata := &AgentAuthMetadata{PaymentExecutionID: "ape_1", ClaimReceipt: "signed.receipt.value"}
	client, err := NewHTTPPaymentOrderClient("https://agentpay.example", "ak_test", httpClientFunc(func(r *http.Request) (*http.Response, error) {
		var body struct {
			AgentAuth *AgentAuthMetadata `json:"agent_auth"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.AgentAuth == nil || *body.AgentAuth != *metadata {
			t.Fatalf("Auth metadata missing on %s: %+v err=%v", r.URL.Path, body, err)
		}
		if strings.HasSuffix(r.URL.Path, ":prepare") {
			return orderHTTPResponse(200, nil, `{"data":{"payment_order_id":"ord_1","service_id":"svc_1","config_version":1,"payment_status":"PREPARED","expires_at":"2026-10-12T00:00:00Z","settlement_lease_acquired":true,"settlement_lease_token":"lease_1"}}`), nil
		}
		headers := make(http.Header)
		headers.Set("PAYMENT-RESPONSE", "payment-response-base64")
		return orderHTTPResponse(200, headers, `{"data":{"payment_order_id":"ord_1","payment_status":"PAID","collection_status":"CONFIRMED","retry_after_seconds":0}}`), nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Prepare(context.Background(), PreparePaymentOrderInput{ServiceID: "svc_1", ConfigVersion: 1, PaymentIdentifier: "pay_1", PaymentSignature: "signature", IdempotencyKey: "prepare_1", Request: RequestBinding{Method: "POST", ResourceURL: "https://provider.example/item", RequestHash: strings.Repeat("a", 64), RequestHashVersion: requestHashVersionV1}, AgentAuth: metadata})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Settle(context.Background(), SettlePaymentOrderInput{PaymentOrderID: "ord_1", SettlementLeaseToken: "lease_1", IdempotencyKey: "settle_1", AgentAuth: metadata})
	if err != nil {
		t.Fatal(err)
	}
}
