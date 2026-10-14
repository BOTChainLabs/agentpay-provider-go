package agentpayprovider

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	x402 "github.com/x402-foundation/x402/go/v2"
	x402http "github.com/x402-foundation/x402/go/v2/http"
	"github.com/x402-foundation/x402/go/v2/types"
)

type loaderFunc func(context.Context) (types.PaymentRequired, error)

func (f loaderFunc) Load(ctx context.Context) (types.PaymentRequired, error) { return f(ctx) }

func TestMiddlewareWritesOfficialCompatible402(t *testing.T) {
	required := runtimeConfigFixture().PaymentRequired
	provider, err := New(loaderFunc(func(context.Context) (types.PaymentRequired, error) { return required, nil }))
	if err != nil {
		t.Fatal(err)
	}
	nextCalled := false
	handler := provider.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { nextCalled = true }))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "https://provider.example/weather", nil))

	if recorder.Code != http.StatusPaymentRequired || nextCalled {
		t.Fatalf("status=%d nextCalled=%t", recorder.Code, nextCalled)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("402 must not be cached: %q", recorder.Header().Get("Cache-Control"))
	}
	client := x402http.Newx402HTTPClient(x402.Newx402Client())
	decoded, err := client.GetPaymentRequiredResponse(map[string]string{PaymentRequiredHeader: recorder.Header().Get(PaymentRequiredHeader)}, nil)
	if err != nil || decoded.X402Version != 2 || decoded.Accepts[0].Extra["paymentFlow"] != "upfront" {
		t.Fatalf("official x402 client could not read 402: decoded=%+v err=%v", decoded, err)
	}
}

func TestMiddlewarePassesParsedRetryWithoutVerifyingIt(t *testing.T) {
	provider, err := New(loaderFunc(func(context.Context) (types.PaymentRequired, error) {
		return types.PaymentRequired{}, errors.New("loader must not run for a payment retry")
	}))
	if err != nil {
		t.Fatal(err)
	}
	payload := types.PaymentPayload{
		X402Version: 2,
		Accepted: types.PaymentRequirements{
			Scheme: "exact", Network: "eip155:968", Asset: "0x75edC9335175Fc0552D51D48439F229c10420fe3", Amount: "100000", PayTo: "0x71d066Eb5C9402BB8bA8ED406D5D51edd320F862",
			Extra: map[string]interface{}{"assetTransferMethod": "permit2", "paymentFlow": "upfront"},
		},
		Payload: map[string]interface{}{"signature": "not-verified-by-b5.1"},
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	handler := provider.Middleware(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		retry, ok := PaymentRetryFromContext(request.Context())
		if !ok || retry.Payload.Accepted.Amount != "100000" || string(retry.PayloadJSON) != string(raw) {
			t.Fatalf("payment retry was not preserved: %+v ok=%t", retry, ok)
		}
		writer.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "https://provider.example/weather", nil)
	request.Header.Set(PaymentSignatureHeader, base64.StdEncoding.EncodeToString(raw))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestMiddlewareMapsInvalidSignatureAndConfigFailures(t *testing.T) {
	provider, err := New(loaderFunc(func(context.Context) (types.PaymentRequired, error) {
		return types.PaymentRequired{}, ErrTemporaryConfig
	}))
	if err != nil {
		t.Fatal(err)
	}
	handler := provider.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { t.Fatal("next must not be called") }))

	t.Run("invalid signature", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "https://provider.example/weather", nil)
		request.Header.Set(PaymentSignatureHeader, "not-base64")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		assertErrorCode(t, recorder, http.StatusBadRequest, CodePaymentSignatureInvalid, false)
	})
	t.Run("multiple signatures", func(t *testing.T) {
		request := httptest.NewRequest(http.MethodGet, "https://provider.example/weather", nil)
		request.Header.Add(PaymentSignatureHeader, "first")
		request.Header.Add(PaymentSignatureHeader, "second")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		assertErrorCode(t, recorder, http.StatusBadRequest, CodePaymentSignatureInvalid, false)
	})
	t.Run("temporary config failure", func(t *testing.T) {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "https://provider.example/weather", nil))
		assertErrorCode(t, recorder, http.StatusServiceUnavailable, CodeConfigUnavailable, true)
	})
}

func assertErrorCode(t *testing.T, recorder *httptest.ResponseRecorder, status int, code Code, retryable bool) {
	t.Helper()
	var response struct {
		Error struct {
			Code      Code `json:"code"`
			Retryable bool `json:"retryable"`
		} `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil || recorder.Code != status || response.Error.Code != code || response.Error.Retryable != retryable {
		t.Fatalf("status=%d body=%s decodeErr=%v", recorder.Code, recorder.Body.String(), err)
	}
}
