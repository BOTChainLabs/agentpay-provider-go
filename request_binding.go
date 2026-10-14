package agentpayprovider

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
)

const (
	requestHashVersionV1       = "agentpay-request-v1"
	defaultMaxRequestBodyBytes = 2 << 20
)

// RequestBinding 是 Provider SDK 交给 Prepare 的原始请求摘要。原始业务请求
// 永远不发送给 AgentPay，避免支付平台获得 Provider 的业务数据。
type RequestBinding struct {
	Method             string
	ResourceURL        string
	RequestHash        string
	RequestHashVersion string
}

// RequestBinder 将 Provider 实际准备执行的 HTTP 请求绑定到支付订单。仅 allowlist
// 中会影响业务语义的 Header 会进入摘要；认证、Cookie、付款载荷与追踪字段一律排除。
type RequestBinder struct {
	SemanticHeaders     []string
	MaxRequestBodyBytes int64
}

// Bind 读取并恢复 request.Body，再生成与 B3.1 一致的 request_hash。恢复 Body 是
// 必需的：支付预校验完成后，业务 Handler 必须仍能看到与摘要完全相同的字节。
func (b RequestBinder) Bind(request *http.Request, serviceID string, configVersion int, resourceURL string) (RequestBinding, error) {
	if request == nil || strings.TrimSpace(serviceID) == "" || configVersion < 1 || strings.TrimSpace(resourceURL) == "" {
		return RequestBinding{}, ErrRuntimeConfigInvalid
	}
	method := strings.ToUpper(strings.TrimSpace(request.Method))
	if method == "" || method != request.Method {
		return RequestBinding{}, ErrRuntimeConfigInvalid
	}
	body, err := b.readAndRestoreBody(request)
	if err != nil {
		return RequestBinding{}, err
	}
	bodyDigest := sha256.Sum256(body)
	headers, err := canonicalSemanticHeaders(request.Header, b.SemanticHeaders)
	if err != nil {
		return RequestBinding{}, err
	}
	headersJSON, err := json.Marshal(headers)
	if err != nil {
		return RequestBinding{}, fmt.Errorf("%w: 无法规范化请求头", ErrRuntimeConfigInvalid)
	}
	target, err := canonicalRequestTarget(request.URL)
	if err != nil {
		return RequestBinding{}, err
	}

	// 长度前缀使字段拼接没有歧义，例如 ["1", "23"] 与 ["12", "3"] 不会
	// 产生同一字节序列。该格式须与架构文档中的 agentpay-request-v1 保持一致。
	fields := []string{
		requestHashVersionV1,
		serviceID,
		fmt.Sprintf("%d", configVersion),
		method,
		target,
		string(headersJSON),
		hex.EncodeToString(bodyDigest[:]),
	}
	var canonical bytes.Buffer
	for _, field := range fields {
		_, _ = fmt.Fprintf(&canonical, "%d:%s", len([]byte(field)), field)
	}
	digest := sha256.Sum256(canonical.Bytes())
	return RequestBinding{
		Method:             method,
		ResourceURL:        resourceURL,
		RequestHash:        hex.EncodeToString(digest[:]),
		RequestHashVersion: requestHashVersionV1,
	}, nil
}

func (b RequestBinder) readAndRestoreBody(request *http.Request) ([]byte, error) {
	if request.Body == nil {
		return nil, nil
	}
	limit := b.MaxRequestBodyBytes
	if limit <= 0 {
		limit = defaultMaxRequestBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, limit+1))
	closeErr := request.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("%w: 读取请求体失败", ErrRuntimeConfigInvalid)
	}
	if closeErr != nil {
		return nil, fmt.Errorf("%w: 关闭请求体失败", ErrRuntimeConfigInvalid)
	}
	if int64(len(body)) > limit {
		return nil, ErrPaymentRequestTooLarge
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	request.ContentLength = int64(len(body))
	return body, nil
}

func canonicalRequestTarget(value *url.URL) (string, error) {
	if value == nil {
		return "", ErrRuntimeConfigInvalid
	}
	path := value.EscapedPath()
	if path == "" {
		path = "/"
	}
	query, err := url.ParseQuery(value.RawQuery)
	if err != nil {
		return "", fmt.Errorf("%w: 请求 query 无法规范化", ErrRuntimeConfigInvalid)
	}
	encoded := query.Encode()
	if encoded == "" {
		return path, nil
	}
	return path + "?" + encoded, nil
}

func canonicalSemanticHeaders(header http.Header, configured []string) (map[string]string, error) {
	allowed := configured
	if len(allowed) == 0 {
		allowed = []string{"Accept", "Content-Type", "Accept-Language"}
	}
	forbidden := map[string]struct{}{
		"authorization": {}, "cookie": {}, "payment-signature": {}, "x-request-id": {}, "x-trace-id": {},
	}
	names := make([]string, 0, len(allowed))
	seen := make(map[string]struct{}, len(allowed))
	for _, name := range allowed {
		name = strings.ToLower(strings.TrimSpace(name))
		if name == "" {
			continue
		}
		if _, prohibited := forbidden[name]; prohibited {
			continue
		}
		if _, duplicate := seen[name]; duplicate {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	sort.Strings(names)
	result := make(map[string]string, len(names))
	for _, name := range names {
		values := header.Values(name)
		if len(values) == 0 {
			continue
		}
		normalized := make([]string, 0, len(values))
		for _, value := range values {
			normalized = append(normalized, strings.TrimSpace(value))
		}
		// 同名 Header 保留到达顺序；部分 HTTP 语义（如 Accept）中值的顺序可能有
		// 含义，SDK 不应为了“排序”而改变 Provider 实际观察到的请求语义。
		result[name] = strings.Join(normalized, ",")
	}
	return result, nil
}
