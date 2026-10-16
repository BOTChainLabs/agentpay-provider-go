package agentpayprovider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/x402-foundation/x402/go/v2/types"
)

type runtimeConfigLoaderFunc func(context.Context) (RuntimeConfig, error)

func (f runtimeConfigLoaderFunc) LoadRuntimeConfig(ctx context.Context) (RuntimeConfig, error) {
	return f(ctx)
}

type fakePaymentOrderClient struct {
	prepared       PreparedPaymentOrder
	settlement     SettlementResult
	confirmed      SettlementResult
	confirmedErr   error
	prepareInput   PreparePaymentOrderInput
	settleInput    SettlePaymentOrderInput
	prepareCalls   int
	settleCalls    int
	confirmedCalls int
}

func (f *fakePaymentOrderClient) Prepare(_ context.Context, input PreparePaymentOrderInput) (PreparedPaymentOrder, error) {
	f.prepareCalls++
	f.prepareInput = input
	return f.prepared, nil
}

func (f *fakePaymentOrderClient) Settle(_ context.Context, input SettlePaymentOrderInput) (SettlementResult, error) {
	f.settleCalls++
	f.settleInput = input
	return f.settlement, nil
}

func (f *fakePaymentOrderClient) LoadConfirmedSettlement(_ context.Context, paymentOrderID string) (SettlementResult, error) {
	f.confirmedCalls++
	if f.confirmedErr != nil {
		return SettlementResult{}, f.confirmedErr
	}
	if paymentOrderID != f.prepared.PaymentOrderID {
		return SettlementResult{}, ErrPaymentOrderClientInvalid
	}
	return f.confirmed, nil
}

func TestPaymentOrchestratorUsesFrozenConfigAndRestoresBusinessRequest(t *testing.T) {
	orders := &fakePaymentOrderClient{
		prepared:   PreparedPaymentOrder{PaymentOrderID: "ord_1", ServiceID: "svc_1", ConfigVersion: 7, PaymentStatus: "PREPARED", SettlementLeaseAcquired: true, SettlementLeaseToken: "lease_1"},
		settlement: SettlementResult{State: SettlementConfirmed, PaymentOrderID: "ord_1", PaymentStatus: "PAID", CollectionStatus: "CONFIRMED", PaymentResponse: "response"},
	}
	orchestrator := PaymentOrchestrator{
		Configs: runtimeConfigLoaderFunc(func(context.Context) (RuntimeConfig, error) { return orchestrationConfig(), nil }),
		Orders:  orders,
	}
	request := httptest.NewRequest(http.MethodPost, "https://provider.example/research?topic=bot", strings.NewReader(`{"depth":2}`))
	request.Header.Set("Content-Type", "application/json")
	result, err := orchestrator.Settle(context.Background(), request, testPaymentRetry())
	if err != nil || !result.Confirmed() || orders.prepareCalls != 1 || orders.settleCalls != 1 {
		t.Fatalf("result=%+v prepare=%d settle=%d err=%v", result, orders.prepareCalls, orders.settleCalls, err)
	}
	if orders.prepareInput.ServiceID != "svc_1" || orders.prepareInput.ConfigVersion != 7 || orders.prepareInput.Request.ResourceURL != "https://provider.example/research" ||
		!strings.HasPrefix(orders.prepareInput.IdempotencyKey, "prepare_") || !strings.HasPrefix(orders.prepareInput.PaymentIdentifier, "pay_") {
		t.Fatalf("prepare input=%+v", orders.prepareInput)
	}
	if !strings.HasPrefix(orders.settleInput.IdempotencyKey, "settle_") || orders.settleInput.SettlementLeaseToken != "lease_1" {
		t.Fatalf("settle input=%+v", orders.settleInput)
	}
	body, readErr := io.ReadAll(request.Body)
	if readErr != nil || string(body) != `{"depth":2}` {
		t.Fatalf("business request body was not restored: %q err=%v", body, readErr)
	}
}

func TestPaymentOrchestratorDoesNotSettleWithoutLease(t *testing.T) {
	orders := &fakePaymentOrderClient{prepared: PreparedPaymentOrder{PaymentOrderID: "ord_1", ServiceID: "svc_1", ConfigVersion: 7, PaymentStatus: "SETTLING"}}
	orchestrator := PaymentOrchestrator{
		Configs: runtimeConfigLoaderFunc(func(context.Context) (RuntimeConfig, error) { return orchestrationConfig(), nil }),
		Orders:  orders,
	}
	result, err := orchestrator.Settle(context.Background(), httptest.NewRequest(http.MethodGet, "https://provider.example/research", nil), testPaymentRetry())
	if err != nil || result.State != SettlementPending || orders.prepareCalls != 1 || orders.settleCalls != 0 {
		t.Fatalf("result=%+v prepare=%d settle=%d err=%v", result, orders.prepareCalls, orders.settleCalls, err)
	}
}

func TestPaymentOrchestratorUsesConfirmedOrderWithoutReopeningSettlement(t *testing.T) {
	orders := &fakePaymentOrderClient{
		prepared:  PreparedPaymentOrder{PaymentOrderID: "ord_1", ServiceID: "svc_1", ConfigVersion: 7, PaymentStatus: "PAID"},
		confirmed: SettlementResult{State: SettlementConfirmed, PaymentOrderID: "ord_1", PaymentStatus: "PAID", CollectionStatus: "CONFIRMED", TransactionHash: "0xtx", Payer: "0xpayer", PaymentResponse: "response"},
	}
	orchestrator := PaymentOrchestrator{
		Configs: runtimeConfigLoaderFunc(func(context.Context) (RuntimeConfig, error) { return orchestrationConfig(), nil }),
		Orders:  orders,
	}
	result, err := orchestrator.Settle(context.Background(), httptest.NewRequest(http.MethodGet, "https://provider.example/research", nil), testPaymentRetry())
	if err != nil || !result.Confirmed() || result.Settlement.PaymentResponse != "response" || orders.prepareCalls != 1 || orders.settleCalls != 0 || orders.confirmedCalls != 1 {
		t.Fatalf("result=%+v prepare=%d settle=%d confirmed=%d err=%v", result, orders.prepareCalls, orders.settleCalls, orders.confirmedCalls, err)
	}
}

func orchestrationConfig() RuntimeConfig {
	config := runtimeConfigFixture()
	config.ServiceID = "svc_1"
	config.ConfigVersion = 7
	config.PaymentRequired.Resource.URL = "https://provider.example/research"
	return config
}

func testPaymentRetry() PaymentRetry {
	return PaymentRetry{
		RawHeader: "encoded-payment-signature",
		Payload: types.PaymentPayload{
			X402Version: 2,
			Accepted:    runtimeConfigFixture().PaymentRequired.Accepts[0],
			Payload:     map[string]any{"signature": "test"},
		},
	}
}
