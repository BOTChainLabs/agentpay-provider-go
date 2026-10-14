package agentpayprovider

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestBinderRestoresBodyAndExcludesSensitiveHeaders(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://provider.example/research?b=2&a=1", strings.NewReader(`{"query":"bot"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer must-not-be-bound")
	request.Header.Set(PaymentSignatureHeader, "must-not-be-bound")
	request.Header.Set("X-Request-ID", "must-not-be-bound")
	binder := RequestBinder{SemanticHeaders: []string{"Content-Type", "Authorization", PaymentSignatureHeader, "X-Request-ID"}}
	binding, err := binder.Bind(request, "svc_research", 3, "https://provider.example/research")
	if err != nil {
		t.Fatal(err)
	}
	if binding.Method != http.MethodPost || binding.ResourceURL != "https://provider.example/research" || binding.RequestHashVersion != requestHashVersionV1 || len(binding.RequestHash) != 64 {
		t.Fatalf("unexpected binding: %+v", binding)
	}
	body, err := io.ReadAll(request.Body)
	if err != nil || string(body) != `{"query":"bot"}` {
		t.Fatalf("handler body changed: %q err=%v", body, err)
	}

	// 认证和付款头改变时不能影响订单请求指纹；它们不改变要执行的业务请求。
	request2 := httptest.NewRequest(http.MethodPost, "https://provider.example/research?a=1&b=2", strings.NewReader(`{"query":"bot"}`))
	request2.Header.Set("Content-Type", "application/json")
	request2.Header.Set("Authorization", "Bearer another")
	request2.Header.Set(PaymentSignatureHeader, "another-signature")
	second, err := binder.Bind(request2, "svc_research", 3, "https://provider.example/research")
	if err != nil {
		t.Fatal(err)
	}
	if second.RequestHash != binding.RequestHash {
		t.Fatalf("sensitive headers or query order changed fingerprint: %s != %s", second.RequestHash, binding.RequestHash)
	}
}

func TestRequestBinderRejectsOversizedBodyWithoutExecutingHandler(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "https://provider.example/research", strings.NewReader("12345"))
	_, err := (RequestBinder{MaxRequestBodyBytes: 4}).Bind(request, "svc_research", 1, "https://provider.example/research")
	if err != ErrPaymentRequestTooLarge {
		t.Fatalf("err=%v", err)
	}
}
