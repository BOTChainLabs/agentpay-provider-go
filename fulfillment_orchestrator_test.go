package agentpayprovider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeFulfillmentClient struct {
	start     FulfillmentStartResult
	startErr  error
	storeErr  error
	finishErr error
	failErr   error
	events    []string
	stored    StoreFulfillmentResultInput
	completed CompleteFulfillmentInput
	failed    FailFulfillmentInput
}

func (f *fakeFulfillmentClient) StartFulfillment(_ context.Context, input StartFulfillmentInput) (FulfillmentStartResult, error) {
	f.events = append(f.events, "start")
	if f.startErr != nil {
		return FulfillmentStartResult{}, f.startErr
	}
	if input.PaymentOrderID != f.start.PaymentOrderID || input.RequestHash != f.start.RequestHash || !strings.HasPrefix(input.IdempotencyKey, "fulfillment_start_") {
		return FulfillmentStartResult{}, ErrPaymentOrderClientInvalid
	}
	return f.start, nil
}

func (f *fakeFulfillmentClient) StoreFulfillmentResult(_ context.Context, input StoreFulfillmentResultInput) (StoredFulfillmentResult, error) {
	f.events = append(f.events, "store")
	f.stored = input
	if f.storeErr != nil {
		return StoredFulfillmentResult{}, f.storeErr
	}
	return StoredFulfillmentResult{PaymentOrderID: input.PaymentOrderID, ResultCacheKey: input.ResultCacheKey, ResultHash: fulfillmentBodyHash(input.Body), ByteSize: int64(len(input.Body))}, nil
}

func (f *fakeFulfillmentClient) CompleteFulfillment(_ context.Context, input CompleteFulfillmentInput) (CompletedFulfillment, error) {
	f.events = append(f.events, "complete")
	f.completed = input
	if f.finishErr != nil {
		return CompletedFulfillment{}, f.finishErr
	}
	return CompletedFulfillment{PaymentOrderID: input.PaymentOrderID, Status: "SUCCEEDED", ResultHash: input.ResultHash, ResultCacheKey: input.ResultCacheKey, FinishedAt: time.Now()}, nil
}

func (f *fakeFulfillmentClient) FailFulfillment(_ context.Context, input FailFulfillmentInput) (FailedFulfillment, error) {
	f.events = append(f.events, "fail")
	f.failed = input
	if f.failErr != nil {
		return FailedFulfillment{}, f.failErr
	}
	return FailedFulfillment{PaymentOrderID: input.PaymentOrderID, Status: "RETRYABLE", AttemptCount: 1}, nil
}

func TestFulfillmentOrchestratorPersistsThenCompletesAndReuses(t *testing.T) {
	requestHash := strings.Repeat("a", 64)
	cacheKey := "agentpay-result://proj_1/ord_1/response"
	client := &fakeFulfillmentClient{start: FulfillmentStartResult{
		PaymentOrderID: "ord_1", PaymentIdentifier: "pay_1", InvocationID: "ord_1", Status: "RUNNING", AttemptCount: 1,
		RequestHash: requestHash, LeaseToken: "lease_1", LeaseExpiresAt: time.Now().Add(time.Minute),
	}}
	results := NewMemoryFulfillmentResultStore()
	orchestrator := FulfillmentOrchestrator{Fulfillment: client, Results: results, ResultCacheKey: ResultCacheKeyBuilder{ProjectID: "proj_1"}.Build}
	calls := 0
	executor := func(_ context.Context, invocation FulfillmentInvocation, request *http.Request) (FulfillmentResult, error) {
		calls++
		if invocation.InvocationID != "ord_1" || request.URL.Path != "/research" {
			t.Fatalf("invocation=%+v request=%s", invocation, request.URL)
		}
		return FulfillmentResult{StatusCode: http.StatusCreated, Header: http.Header{"Content-Type": {"application/json"}}, Body: []byte(`{"ok":true}`)}, nil
	}
	payment := confirmedPayment("ord_1", requestHash)
	request := httptest.NewRequest(http.MethodPost, "https://provider.example/research", nil)
	first, err := orchestrator.Fulfill(context.Background(), request, payment, executor)
	if err != nil || first.Reused || first.FulfillmentStatus != "SUCCEEDED" || string(first.Result.Body) != `{"ok":true}` || calls != 1 {
		t.Fatalf("first=%+v calls=%d err=%v", first, calls, err)
	}
	if got := strings.Join(client.events, ","); got != "start,store,complete" || client.stored.ResultCacheKey != cacheKey || client.completed.ResultHash != fulfillmentBodyHash(first.Result.Body) {
		t.Fatalf("events=%s store=%+v complete=%+v", got, client.stored, client.completed)
	}

	client.events = nil
	client.start = FulfillmentStartResult{PaymentOrderID: "ord_1", PaymentIdentifier: "pay_1", InvocationID: "ord_1", Status: "SUCCEEDED", AttemptCount: 1,
		RequestHash: requestHash, Reused: true, ResultCacheKey: cacheKey, ResultHash: fulfillmentBodyHash(first.Result.Body), FinishedAt: time.Now()}
	second, err := orchestrator.Fulfill(context.Background(), request, payment, executor)
	if err != nil || !second.Reused || string(second.Result.Body) != `{"ok":true}` || calls != 1 || strings.Join(client.events, ",") != "start" {
		t.Fatalf("second=%+v calls=%d events=%v err=%v", second, calls, client.events, err)
	}
}

func TestFulfillmentOrchestratorUsesSavedResultToRecoverBeforeComplete(t *testing.T) {
	requestHash := strings.Repeat("a", 64)
	cacheKey := "agentpay-result://proj_1/ord_1/response"
	results := NewMemoryFulfillmentResultStore()
	if err := results.Save(context.Background(), cacheKey, FulfillmentResult{Body: []byte(`{"cached":true}`)}); err != nil {
		t.Fatal(err)
	}
	client := &fakeFulfillmentClient{start: FulfillmentStartResult{PaymentOrderID: "ord_1", PaymentIdentifier: "pay_1", InvocationID: "ord_1", Status: "RUNNING", AttemptCount: 2,
		RequestHash: requestHash, LeaseToken: "lease_2", LeaseExpiresAt: time.Now().Add(time.Minute)}}
	orchestrator := FulfillmentOrchestrator{Fulfillment: client, Results: results, ResultCacheKey: ResultCacheKeyBuilder{ProjectID: "proj_1"}.Build}
	_, err := orchestrator.Fulfill(context.Background(), httptest.NewRequest(http.MethodGet, "https://provider.example/research", nil), confirmedPayment("ord_1", requestHash),
		func(context.Context, FulfillmentInvocation, *http.Request) (FulfillmentResult, error) {
			t.Fatal("已有已保存结果时不能再次执行业务")
			return FulfillmentResult{}, nil
		})
	if err != nil || strings.Join(client.events, ",") != "start,store,complete" || string(client.stored.Body) != `{"cached":true}` {
		t.Fatalf("events=%v store=%+v err=%v", client.events, client.stored, err)
	}
}

func TestFulfillmentOrchestratorReportsHandlerFailureWithoutLeakingCause(t *testing.T) {
	requestHash := strings.Repeat("a", 64)
	client := &fakeFulfillmentClient{start: FulfillmentStartResult{PaymentOrderID: "ord_1", PaymentIdentifier: "pay_1", InvocationID: "ord_1", Status: "RUNNING", AttemptCount: 1,
		RequestHash: requestHash, LeaseToken: "lease_1", LeaseExpiresAt: time.Now().Add(time.Minute)}}
	orchestrator := FulfillmentOrchestrator{Fulfillment: client, Results: NewMemoryFulfillmentResultStore(), ResultCacheKey: ResultCacheKeyBuilder{ProjectID: "proj_1"}.Build}
	_, err := orchestrator.Fulfill(context.Background(), httptest.NewRequest(http.MethodGet, "https://provider.example/research", nil), confirmedPayment("ord_1", requestHash),
		func(context.Context, FulfillmentInvocation, *http.Request) (FulfillmentResult, error) {
			return FulfillmentResult{}, errors.New("secret database endpoint")
		})
	var sdkErr *SDKError
	if !errors.As(err, &sdkErr) || sdkErr.Code != CodeFulfillmentRejected || client.failed.FailureCode != "HANDLER_FAILED" || client.failed.FailureReason != "provider handler execution failed" ||
		strings.Join(client.events, ",") != "start,fail" {
		t.Fatalf("err=%v failed=%+v events=%v", err, client.failed, client.events)
	}
}

func TestFulfillmentOrchestratorNeverRunsExecutorBeforeConfirmed(t *testing.T) {
	orchestrator := FulfillmentOrchestrator{}
	outcome, err := orchestrator.Fulfill(context.Background(), httptest.NewRequest(http.MethodGet, "https://provider.example/research", nil), PaymentOrchestrationResult{State: SettlementPending},
		func(context.Context, FulfillmentInvocation, *http.Request) (FulfillmentResult, error) {
			t.Fatal("结算未确认时不能履约")
			return FulfillmentResult{}, nil
		})
	if err != nil || !outcome.FulfillmentPending {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
}

func TestFulfillmentOrchestratorExecutesPaidRetryWithoutSettlementLease(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://provider.example/research", nil)
	binding, err := (RequestBinder{}).Bind(request, "svc_1", 7, "https://provider.example/research")
	if err != nil {
		t.Fatal(err)
	}
	orders := &fakePaymentOrderClient{
		prepared:  PreparedPaymentOrder{PaymentOrderID: "ord_1", ServiceID: "svc_1", ConfigVersion: 7, PaymentStatus: "PAID"},
		confirmed: SettlementResult{State: SettlementConfirmed, PaymentOrderID: "ord_1", PaymentStatus: "PAID", CollectionStatus: "CONFIRMED", TransactionHash: "0xtx", Payer: "0xpayer", PaymentResponse: "response"},
	}
	fulfillment := &fakeFulfillmentClient{start: FulfillmentStartResult{
		PaymentOrderID: "ord_1", PaymentIdentifier: "pay_1", InvocationID: "ord_1", Status: "RUNNING", AttemptCount: 1,
		RequestHash: binding.RequestHash, LeaseToken: "lease_1", LeaseExpiresAt: time.Now().Add(time.Minute),
	}}
	orchestrator := FulfillmentOrchestrator{
		Payments: PaymentOrchestrator{
			Configs: runtimeConfigLoaderFunc(func(context.Context) (RuntimeConfig, error) { return orchestrationConfig(), nil }),
			Orders:  orders,
		},
		Fulfillment:    fulfillment,
		Results:        NewMemoryFulfillmentResultStore(),
		ResultCacheKey: ResultCacheKeyBuilder{ProjectID: "proj_1"}.Build,
	}
	outcome, err := orchestrator.Execute(context.Background(), request, testPaymentRetry(), func(context.Context, FulfillmentInvocation, *http.Request) (FulfillmentResult, error) {
		return FulfillmentResult{StatusCode: http.StatusOK, Body: []byte(`{"ok":true}`)}, nil
	})
	if err != nil || outcome.FulfillmentPending || outcome.Payment.Settlement.PaymentResponse != "response" || outcome.FulfillmentStatus != "SUCCEEDED" ||
		orders.settleCalls != 0 || orders.confirmedCalls != 1 || strings.Join(fulfillment.events, ",") != "start,store,complete" {
		t.Fatalf("outcome=%+v settle=%d confirmed=%d events=%v err=%v", outcome, orders.settleCalls, orders.confirmedCalls, fulfillment.events, err)
	}
}

func TestMemoryFulfillmentResultStoreRejectsDifferentReplay(t *testing.T) {
	store := NewMemoryFulfillmentResultStore()
	key := "agentpay-result://proj_1/ord_1/response"
	if err := store.Save(context.Background(), key, FulfillmentResult{Body: []byte(`{"ok":true}`)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), key, FulfillmentResult{Body: []byte(`{"ok":false}`)}); !errors.Is(err, ErrFulfillmentResultConflict) {
		t.Fatalf("err=%v, want result conflict", err)
	}
}

func confirmedPayment(orderID, requestHash string) PaymentOrchestrationResult {
	return PaymentOrchestrationResult{
		State:          SettlementConfirmed,
		Order:          PreparedPaymentOrder{PaymentOrderID: orderID},
		Settlement:     SettlementResult{State: SettlementConfirmed, PaymentOrderID: orderID, PaymentStatus: "PAID", CollectionStatus: "CONFIRMED"},
		RequestBinding: RequestBinding{RequestHash: requestHash},
	}
}
