// Package lovart 封装 Lovart 上游：鉴权头、wasm 签名、素材上传、生成任务。
//
// 上游有两套 base：
//   - www 域（/api/canva、/api/www）：只要 token 头
//   - lgw 网关（/v1/*）：额外要 X-Send-Timestamp / X-Req-Uuid / X-Client-Signature
//
// 详见 docs/upstream/lovart-api.md。
package lovart

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjie-ai/lovart2api/internal/sign"
)

// 默认上游地址。可在配置里覆盖（私有化/镜像场景）。
const (
	DefaultWWWBase = "https://www.lovart.ai"
	DefaultLGWBase = "https://lgw.lovart.ai"
)

// ErrKind 把上游失败归成网关可以据此做决策的几类。
type ErrKind string

const (
	ErrAuth        ErrKind = "auth"        // token 失效
	ErrQuota       ErrKind = "quota"       // 当日额度耗尽
	ErrConcurrency ErrKind = "concurrency" // 上游并发闸门
	ErrSignature   ErrKind = "signature"   // 签名/头校验失败（多为实现问题）
	ErrUnsupported ErrKind = "unsupported" // 该 generator 在网关侧没有映射
	ErrUpstream    ErrKind = "upstream"    // 其他上游错误
)

// APIError 上游业务错误（HTTP 200 但 code != 0，或 4xx/5xx）。
type APIError struct {
	Kind    ErrKind `json:"kind"`
	Status  int     `json:"status"`
	Code    string  `json:"code"`
	Message string  `json:"message"`
	Raw     string  `json:"raw,omitempty"`
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("lovart %s: %s (code=%s status=%d)", e.Kind, e.Message, e.Code, e.Status)
	}
	return fmt.Sprintf("lovart %s: %s (status=%d)", e.Kind, e.Message, e.Status)
}

// ErrorKind 供上层（账号池）按类型决定冷却策略，返回 "quota" / "auth" / "concurrency" 等。
func (e *APIError) ErrorKind() string { return string(e.Kind) }

// IsAuth 判断错误是否为鉴权失败。
func IsAuth(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Kind == ErrAuth
}

// IsQuota 判断错误是否为额度耗尽。
func IsQuota(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Kind == ErrQuota
}

// IsConcurrency 判断错误是否为上游并发闸门。
func IsConcurrency(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Kind == ErrConcurrency
}

// IsUnsupported 判断错误是否为「该模型在网关侧无映射」。
func IsUnsupported(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Kind == ErrUnsupported
}

// Client 绑定一个账号凭证的上游客户端。并发安全。
type Client struct {
	HTTP *http.Client
	Sign *sign.Signer

	Token   string
	WWWBase string
	LGWBase string
	Lang    string
	// UserAgent 出站 UA。上游按 web 客户端行为放行，这里保持浏览器形态。
	UserAgent string
}

// NewClient 构造客户端。
func NewClient(token string, sg *sign.Signer) *Client {
	return &Client{
		HTTP: &http.Client{
			Timeout: 120 * time.Second,
			Transport: &http.Transport{
				Proxy:               nil, // 明文声明：不吃环境代理，避免 HTTP_PROXY 把请求送进本地代理
				MaxIdleConns:        64,
				MaxIdleConnsPerHost: 16,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		Sign:      sg,
		Token:     token,
		WWWBase:   DefaultWWWBase,
		LGWBase:   DefaultLGWBase,
		Lang:      "zh",
		UserAgent: "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/153.0.0.0 Safari/537.36",
	}
}

// envelope 上游统一信封。
type envelope struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
	Msg     string          `json:"msg"`
	Data    json.RawMessage `json:"data"`
}

// codeString 把 code 归一成字符串（上游既有数字也有字符串）。
func (e *envelope) codeString() string {
	if len(e.Code) == 0 {
		return ""
	}
	s := strings.Trim(string(e.Code), `"`)
	return s
}

// msg 兼容 message / msg 两种字段。
func (e *envelope) msg() string {
	if e.Message != "" {
		return e.Message
	}
	return e.Msg
}

// okCode 判断业务码是否成功：0 / "0" / "SUCCESS"。
func okCode(c string) bool {
	return c == "" || c == "0" || c == "SUCCESS" || c == "success"
}

// classify 把上游返回映射成 ErrKind。
func classify(status int, code, msg, raw string) *APIError {
	e := &APIError{Status: status, Code: code, Message: msg, Raw: raw}
	switch {
	case status == 401 || code == "INVALID_TOKEN" || code == "401":
		e.Kind = ErrAuth
	case strings.Contains(code, "1200000409") || strings.Contains(raw, "1200000409"):
		e.Kind = ErrQuota
		e.Message = firstNonEmpty(e.Message, "每日生成额度已用尽")
	case strings.Contains(code, "1200000200") || strings.Contains(raw, "1200000200"):
		e.Kind = ErrConcurrency
		e.Message = firstNonEmpty(e.Message, "上游并发已满")
	case code == "MISSING_HEADER" || code == "INVALID_SIGNATURE":
		e.Kind = ErrSignature
	case strings.Contains(raw, "no API path found for generator"):
		e.Kind = ErrUnsupported
	case status >= 400:
		e.Kind = ErrUpstream
	default:
		e.Kind = ErrUpstream
	}
	return e
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// newReqUUID 生成 32 位无横线 uuid（对齐前端 `crypto.randomUUID().replace(/-/g,"")`）。
func newReqUUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// 极端情况下退回时间戳，签名只要求两次调用间唯一
		return fmt.Sprintf("%032x", time.Now().UnixNano())
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return hex.EncodeToString(b[:])
}

// do 发一个上游请求。signed=true 时补 lgw 需要的三个头。
// reqOpt 给单次请求加自定义头（目前只用于 refresh 的指纹头）。
type reqOpt func(*http.Request)

func withHeader(k, v string) reqOpt {
	return func(r *http.Request) { r.Header.Set(k, v) }
}

func (c *Client) do(ctx context.Context, method, rawURL string, body any, signed bool, opts ...reqOpt) (json.RawMessage, error) {
	var rdr io.Reader
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		rdr = bytes.NewReader(buf)
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rdr)
	if err != nil {
		return nil, err
	}
	req.Header.Set("accept", "application/json, text/plain, */*")
	req.Header.Set("accept-language", c.Lang)
	req.Header.Set("user-agent", c.UserAgent)
	req.Header.Set("origin", c.WWWBase)
	req.Header.Set("referer", c.WWWBase+"/")
	if body != nil {
		req.Header.Set("content-type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("token", c.Token)
	}
	for _, opt := range opts {
		opt(req)
	}
	if signed {
		ts := strconv.FormatInt(time.Now().UnixMilli(), 10)
		uid := newReqUUID()
		sig, err := c.Sign.Sign(ctx, ts, uid)
		if err != nil {
			return nil, fmt.Errorf("签名失败: %w", err)
		}
		req.Header.Set("X-Send-Timestamp", ts)
		req.Header.Set("X-Req-Uuid", uid)
		req.Header.Set("X-Client-Signature", sig)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求上游失败: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("读取上游响应失败: %w", err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		// 非 JSON（Kong 的纯文本 404、Cloudflare 质询页等）
		return nil, classify(resp.StatusCode, "", strings.TrimSpace(string(raw)), string(raw))
	}
	code := env.codeString()
	msg := env.msg()
	if !okCode(code) {
		return nil, classify(resp.StatusCode, code, msg, string(raw))
	}
	if resp.StatusCode >= 400 {
		return nil, classify(resp.StatusCode, code, msg, string(raw))
	}
	return env.Data, nil
}

// joinURL 拼 base + path。
func joinURL(base, path string) string {
	return strings.TrimRight(base, "/") + path
}

// escapeQuery 构造带 query 的 URL。
func escapeQuery(base, path string, kv map[string]string) string {
	u := joinURL(base, path)
	if len(kv) == 0 {
		return u
	}
	q := url.Values{}
	for k, v := range kv {
		q.Set(k, v)
	}
	return u + "?" + q.Encode()
}
