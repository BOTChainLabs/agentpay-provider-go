package agentpayprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
)

// FulfillmentResult 是 Provider 业务调用的可重放结果。B6 保存 Body 用于校验，
// Provider 结果仓库额外保存 HTTP 状态和 Header，才能在同订单重试时完整回放响应。
type FulfillmentResult struct {
	StatusCode int
	Header     http.Header
	Body       []byte
}

// FulfillmentResultStore 必须由生产 Provider 实现为持久化存储；不能只依赖进程内
// map。B6 订单查询只返回 result_cache_key/result_hash，不公开结果正文，SDK 通过
// 此接口在 B6 已 SUCCEEDED 时返回原来的业务结果，且绝不二次执行业务。Save 必须
// 具备“同 key 同结果可重放、同 key 不同结果拒绝”的原子语义。
type FulfillmentResultStore interface {
	Load(context.Context, string) (FulfillmentResult, error)
	Save(context.Context, string, FulfillmentResult) error
}

// MemoryFulfillmentResultStore 仅适用于单测和本地演示。生产环境应替换成与业务
// 生命周期一致的数据库或对象存储实现。
type MemoryFulfillmentResultStore struct {
	mu      sync.Mutex
	results map[string]FulfillmentResult
}

func NewMemoryFulfillmentResultStore() *MemoryFulfillmentResultStore {
	return &MemoryFulfillmentResultStore{results: make(map[string]FulfillmentResult)}
}

func (s *MemoryFulfillmentResultStore) Load(_ context.Context, key string) (FulfillmentResult, error) {
	if s == nil {
		return FulfillmentResult{}, ErrFulfillmentResultNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	result, ok := s.results[key]
	if !ok {
		return FulfillmentResult{}, ErrFulfillmentResultNotFound
	}
	return cloneFulfillmentResult(result), nil
}

func (s *MemoryFulfillmentResultStore) Save(_ context.Context, key string, result FulfillmentResult) error {
	if s == nil || !validResultCacheKey(key) {
		return ErrFulfillmentResultInvalid
	}
	normalized, err := normalizeFulfillmentResult(result)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.results == nil {
		s.results = make(map[string]FulfillmentResult)
	}
	if existing, ok := s.results[key]; ok && !reflect.DeepEqual(existing, normalized) {
		return ErrFulfillmentResultConflict
	}
	s.results[key] = cloneFulfillmentResult(normalized)
	return nil
}

// FulfillmentExecutor 由 Provider 适配自己的业务 Handler。InvocationID 恒等于
// payment_order_id；Handler 必须将其作为幂等键，而不是自行生成新的调用 ID。
type FulfillmentExecutor func(context.Context, FulfillmentInvocation, *http.Request) (FulfillmentResult, error)

type FulfillmentInvocation struct {
	PaymentOrderID    string
	PaymentIdentifier string
	InvocationID      string
	RequestHash       string
	AttemptCount      int
}

// FulfillmentResultKeyBuilder 生成 B6 配置的受信结果引用。Builder 中的 project ID
// 必须和 Provider API Key 所属项目一致，否则 B6 会拒绝 Store/Complete。
type FulfillmentResultKeyBuilder func(FulfillmentInvocation) (string, error)

// ResultCacheKeyBuilder 是默认命名空间的可配置实现。Namespace 必须与 AgentPay
// 服务端 fulfillment.result_namespace 完全相同，不能由 SDK 猜测或拼接其他项目。
type ResultCacheKeyBuilder struct {
	Namespace  string
	ProjectID  string
	ObjectName string
}

func (b ResultCacheKeyBuilder) Build(invocation FulfillmentInvocation) (string, error) {
	namespace := strings.TrimSpace(b.Namespace)
	if namespace == "" {
		namespace = "agentpay-result://"
	}
	projectID := strings.TrimSpace(b.ProjectID)
	objectName := strings.TrimSpace(b.ObjectName)
	if objectName == "" {
		objectName = "response"
	}
	if !strings.HasSuffix(namespace, "/") || !validResultSegment(projectID) || !validPaymentIdentifier(invocation.PaymentOrderID) || !validResultSegment(objectName) {
		return "", ErrFulfillmentResultInvalid
	}
	key := namespace + projectID + "/" + invocation.PaymentOrderID + "/" + objectName
	if !validResultCacheKey(key) {
		return "", ErrFulfillmentResultInvalid
	}
	return key, nil
}

// FulfillmentFailure 允许业务层安全地声明失败类型。Reason 会进入 AgentPay 的
// failure_reason，故不得放入用户输入、密钥或内部错误堆栈。
type FulfillmentFailure struct {
	Cause     error
	Code      string
	Reason    string
	Retryable bool
}

func (e *FulfillmentFailure) Error() string {
	if e == nil || e.Cause == nil {
		return "fulfillment execution failed"
	}
	return e.Cause.Error()
}

func (e *FulfillmentFailure) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

// FulfillmentOrchestrator 是 B5.2 的完整编排器：Prepare → Settle →
// Start → 保存结果 → Store → Complete/Fail。它从不在 UNKNOWN 或没有
// fulfillment lease 时调用 Executor。
type FulfillmentOrchestrator struct {
	Payments       PaymentOrchestrator
	Fulfillment    FulfillmentClient
	Results        FulfillmentResultStore
	ResultCacheKey FulfillmentResultKeyBuilder
}

type FulfillmentOutcome struct {
	Payment            PaymentOrchestrationResult
	FulfillmentStatus  string
	Result             FulfillmentResult
	Reused             bool
	FulfillmentPending bool
}

// Execute 适合直接放在 B5.1 Middleware 的下游。请求正文在 Prepare 的摘要计算后
// 已被恢复，Executor 读取到的仍是原始业务请求；成功后返回的 PAYMENT-RESPONSE
// 位于 outcome.Payment.Settlement.PaymentResponse，应用应原样写回调用方。
func (o FulfillmentOrchestrator) Execute(ctx context.Context, request *http.Request, retry PaymentRetry, executor FulfillmentExecutor) (FulfillmentOutcome, error) {
	payment, err := o.Payments.Settle(ctx, request, retry)
	if err != nil {
		return FulfillmentOutcome{}, err
	}
	return o.Fulfill(ctx, request, payment, executor)
}

// Fulfill 允许已有 Prepare/Settle 编排的应用逐步接入 B6。payment 只能来自本 SDK
// 的 Settle；Confirmed 以外的任何状态都直接返回 pending，业务 Handler 不会运行。
func (o FulfillmentOrchestrator) Fulfill(ctx context.Context, request *http.Request, payment PaymentOrchestrationResult, executor FulfillmentExecutor) (FulfillmentOutcome, error) {
	outcome := FulfillmentOutcome{Payment: payment}
	if !payment.Confirmed() {
		outcome.FulfillmentPending = true
		return outcome, nil
	}
	if o.Fulfillment == nil || o.Results == nil || o.ResultCacheKey == nil || executor == nil || request == nil ||
		!validPaymentIdentifier(payment.Order.PaymentOrderID) || !validSHA256(payment.RequestBinding.RequestHash) {
		return FulfillmentOutcome{}, ErrFulfillmentExecutorInvalid
	}
	start, err := o.Fulfillment.StartFulfillment(ctx, StartFulfillmentInput{
		PaymentOrderID: payment.Order.PaymentOrderID,
		RequestHash:    payment.RequestBinding.RequestHash,
		IdempotencyKey: fulfillmentIdempotencyKey("start", payment.Order.PaymentOrderID, payment.RequestBinding.RequestHash),
	})
	if err != nil {
		return FulfillmentOutcome{}, err
	}
	invocation := FulfillmentInvocation{PaymentOrderID: start.PaymentOrderID, PaymentIdentifier: start.PaymentIdentifier,
		InvocationID: start.InvocationID, RequestHash: start.RequestHash, AttemptCount: start.AttemptCount}
	if start.Reused {
		result, err := o.loadVerifiedResult(ctx, start.ResultCacheKey, start.ResultHash)
		if err != nil {
			return FulfillmentOutcome{}, err
		}
		return FulfillmentOutcome{Payment: payment, FulfillmentStatus: start.Status, Result: result, Reused: true}, nil
	}

	cacheKey, err := o.ResultCacheKey(invocation)
	if err != nil || !validResultCacheKey(cacheKey) {
		return FulfillmentOutcome{}, ErrFulfillmentResultInvalid
	}
	result, cached, err := o.loadResultForLease(ctx, cacheKey)
	if err != nil {
		return FulfillmentOutcome{}, err
	}
	if !cached {
		result, err = executor(ctx, invocation, request)
		if err != nil {
			return FulfillmentOutcome{}, o.failExecution(ctx, start, err)
		}
		result, err = normalizeFulfillmentResult(result)
		if err != nil {
			return FulfillmentOutcome{}, o.failExecution(ctx, start, err)
		}
		// 先把完整响应持久化到 Provider 的结果仓库。若这里失败，不能调用
		// Complete；否则 B6 虽标成功，SDK 却无法在同订单重试时回放原响应。
		if err := o.Results.Save(ctx, cacheKey, result); err != nil {
			return FulfillmentOutcome{}, o.failExecution(ctx, start, err)
		}
	}
	resultHash := fulfillmentBodyHash(result.Body)
	stored, err := o.Fulfillment.StoreFulfillmentResult(ctx, StoreFulfillmentResultInput{
		PaymentOrderID: start.PaymentOrderID, ResultCacheKey: cacheKey, Body: result.Body,
		IdempotencyKey: fulfillmentIdempotencyKey("store", start.PaymentOrderID, resultHash),
	})
	if err != nil {
		return FulfillmentOutcome{}, completionPendingError(err)
	}
	if stored.ResultHash != resultHash || stored.ResultCacheKey != cacheKey {
		return FulfillmentOutcome{}, completionPendingError(ErrFulfillmentResultInvalid)
	}
	completed, err := o.Fulfillment.CompleteFulfillment(ctx, CompleteFulfillmentInput{
		PaymentOrderID: start.PaymentOrderID, LeaseToken: start.LeaseToken, ResultHash: resultHash, ResultCacheKey: cacheKey,
		IdempotencyKey: fulfillmentIdempotencyKey("complete", start.PaymentOrderID, resultHash),
	})
	if err != nil {
		// Store 已完成时，不能再 Fail：B6 会在租约回收时校验已存对象并收敛，
		// 这样进程在 Complete 前崩溃也不会促成第二次业务执行。
		return FulfillmentOutcome{}, completionPendingError(err)
	}
	return FulfillmentOutcome{Payment: payment, FulfillmentStatus: completed.Status, Result: result}, nil
}

func (o FulfillmentOrchestrator) loadResultForLease(ctx context.Context, cacheKey string) (FulfillmentResult, bool, error) {
	result, err := o.Results.Load(ctx, cacheKey)
	if errors.Is(err, ErrFulfillmentResultNotFound) {
		return FulfillmentResult{}, false, nil
	}
	if err != nil {
		return FulfillmentResult{}, false, &SDKError{Code: CodeFulfillmentResultInvalid, Retryable: true, Cause: err}
	}
	result, err = normalizeFulfillmentResult(result)
	if err != nil {
		return FulfillmentResult{}, false, &SDKError{Code: CodeFulfillmentResultInvalid, Retryable: false, Cause: err}
	}
	return result, true, nil
}

func (o FulfillmentOrchestrator) loadVerifiedResult(ctx context.Context, cacheKey, expectedHash string) (FulfillmentResult, error) {
	result, err := o.Results.Load(ctx, cacheKey)
	if errors.Is(err, ErrFulfillmentResultNotFound) {
		return FulfillmentResult{}, &SDKError{Code: CodeFulfillmentResultMissing, Retryable: false, Cause: err}
	}
	if err != nil {
		return FulfillmentResult{}, &SDKError{Code: CodeFulfillmentResultInvalid, Retryable: true, Cause: err}
	}
	result, err = normalizeFulfillmentResult(result)
	if err != nil || fulfillmentBodyHash(result.Body) != expectedHash {
		return FulfillmentResult{}, &SDKError{Code: CodeFulfillmentResultInvalid, Retryable: false, Cause: ErrFulfillmentResultInvalid}
	}
	return result, nil
}

func (o FulfillmentOrchestrator) failExecution(ctx context.Context, start FulfillmentStartResult, cause error) error {
	failure := fulfillmentFailureFrom(cause)
	_, failErr := o.Fulfillment.FailFulfillment(ctx, FailFulfillmentInput{
		PaymentOrderID: start.PaymentOrderID, LeaseToken: start.LeaseToken, FailureCode: failure.Code,
		FailureReason: failure.Reason, Retryable: failure.Retryable,
		IdempotencyKey: fulfillmentIdempotencyKey("fail", start.PaymentOrderID, start.LeaseToken, failure.Code),
	})
	if failErr != nil {
		return completionPendingError(fmt.Errorf("%w: %v", cause, failErr))
	}
	return &SDKError{Code: CodeFulfillmentRejected, Retryable: failure.Retryable, Cause: cause}
}

func fulfillmentFailureFrom(err error) FulfillmentFailure {
	var declared *FulfillmentFailure
	if errors.As(err, &declared) && declared != nil && validFailureCode(declared.Code) && validFailureReason(declared.Reason) {
		return *declared
	}
	// 不把 Handler 的 Error() 直接写给 B6：它可能含请求参数、内部拓扑或密钥片段。
	return FulfillmentFailure{Cause: err, Code: "HANDLER_FAILED", Reason: "provider handler execution failed", Retryable: true}
}

func completionPendingError(cause error) error {
	return &SDKError{Code: CodeFulfillmentCompletionWait, Retryable: true, Cause: cause}
}

func normalizeFulfillmentResult(result FulfillmentResult) (FulfillmentResult, error) {
	if len(result.Body) > maxFulfillmentResultBytes {
		return FulfillmentResult{}, ErrFulfillmentResultTooLarge
	}
	if result.StatusCode == 0 {
		result.StatusCode = http.StatusOK
	}
	if result.StatusCode < 100 || result.StatusCode > 599 {
		return FulfillmentResult{}, ErrFulfillmentResultInvalid
	}
	return cloneFulfillmentResult(result), nil
}

func cloneFulfillmentResult(result FulfillmentResult) FulfillmentResult {
	cloned := FulfillmentResult{StatusCode: result.StatusCode, Header: result.Header.Clone(), Body: append([]byte(nil), result.Body...)}
	return cloned
}

func fulfillmentBodyHash(body []byte) string {
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:])
}

func fulfillmentIdempotencyKey(action, paymentOrderID string, values ...string) string {
	canonical := action + "\x00" + paymentOrderID + "\x00" + strings.Join(values, "\x00")
	digest := sha256.Sum256([]byte(canonical))
	return "fulfillment_" + action + "_" + hex.EncodeToString(digest[:])
}

func validResultSegment(value string) bool {
	if value == "" || len(value) > 200 || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' ||
			char == '.' || char == '_' || char == '~' || char == '+' || char == '-' {
			continue
		}
		return false
	}
	return true
}
