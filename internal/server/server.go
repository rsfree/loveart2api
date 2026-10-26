// Package server 提供 OpenAI 兼容的 HTTP 入口与内嵌面板。
package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yuanjie-ai/lovart2api/internal/config"
	"github.com/yuanjie-ai/lovart2api/internal/lovart"
	"github.com/yuanjie-ai/lovart2api/internal/pool"
	"github.com/yuanjie-ai/lovart2api/internal/sign"
)

// Server 网关。
type Server struct {
	Cfg    *config.Config
	Pool   *pool.Pool
	Signer *sign.Signer
	Log    *Logger

	mux *http.ServeMux
}

// serverStart 进程启动时间，面板展示 uptime 用。
var serverStart = time.Now()

// New 构造网关。
func New(cfg *config.Config, p *pool.Pool, sg *sign.Signer) *Server {
	s := &Server{
		Cfg:    cfg,
		Pool:   p,
		Signer: sg,
		Log:    NewLogger(cfg.Logging.MaxEntries),
		mux:    http.NewServeMux(),
	}
	s.routes()
	return s
}

// Handler 返回根 handler（含 CORS 与访问日志）。
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, X-API-Key")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		s.mux.ServeHTTP(w, r)
	})
}

func (s *Server) routes() {
	s.mux.HandleFunc("GET /healthz", s.handleHealth)
	s.mux.HandleFunc("GET /v1/models", s.withAuth(s.handleModels))
	s.mux.HandleFunc("POST /v1/images/generations", s.withAuth(s.handleGenerations))
	s.mux.HandleFunc("POST /v1/images/edits", s.withAuth(s.handleEdits))
	s.mux.HandleFunc("GET /v1/usage", s.withAuth(s.handleUsage))

	// 面板（自带口令校验）
	s.panelRoutes()
}

// withAuth 校验 Bearer token / x-api-key。
func (s *Server) withAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.checkAPIKey(r) {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key", "API key 无效")
			return
		}
		next(w, r)
	}
}

func (s *Server) checkAPIKey(r *http.Request) bool {
	want := s.Cfg.APIKey
	if want == "" {
		return true
	}
	got := ""
	if h := r.Header.Get("Authorization"); h != "" {
		if strings.HasPrefix(strings.ToLower(h), "bearer ") {
			got = strings.TrimSpace(h[7:])
		} else {
			got = strings.TrimSpace(h)
		}
	}
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if got == "" {
		// 部分 SDK 把 key 放 query
		got = r.URL.Query().Get("api_key")
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	st := s.Pool.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "lovart2api",
		"accounts": map[string]any{
			"total":     st.Total,
			"available": st.Available,
			"cooling":   st.Cooling,
		},
		"time": time.Now().Format(time.RFC3339),
	})
}

// ModelsResponse 是 /v1/models 的返回。
type ModelsResponse struct {
	Object string      `json:"object"`
	Data   []ModelCard `json:"data"`
}

// ModelCard OpenAI 模型卡片。
type ModelCard struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	Created int64  `json:"created"`
	OwnedBy string `json:"owned_by"`
	// 扩展字段，方便客户端挑选
	Display      string `json:"display_name,omitempty"`
	SupportsEdit bool   `json:"supports_edit"`
	Generator    string `json:"upstream_generator,omitempty"`
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	out := ModelsResponse{Object: "list"}
	for name, spec := range s.Cfg.Models {
		if spec.Type != "" && spec.Type != "image" && spec.Type != "image-modify" {
			continue
		}
		out.Data = append(out.Data, ModelCard{
			ID:           name,
			Object:       "model",
			Created:      now,
			OwnedBy:      "lovart",
			Display:      spec.Display,
			SupportsEdit: spec.SupportsEdit,
			Generator:    spec.Generator,
		})
	}
	// 稳定顺序，方便客户端 diff
	for i := 1; i < len(out.Data); i++ {
		for j := i; j > 0 && out.Data[j].ID < out.Data[j-1].ID; j-- {
			out.Data[j], out.Data[j-1] = out.Data[j-1], out.Data[j]
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleUsage(w http.ResponseWriter, r *http.Request) {
	st := s.Pool.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts": st,
		"requests": s.Log.Count(),
	})
}

// writeJSON 统一 JSON 输出。
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		log.Printf("写响应失败: %v", err)
	}
}

// OpenAIError OpenAI 风格错误体。
type OpenAIError struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code,omitempty"`
		Param   string `json:"param,omitempty"`
	} `json:"error"`
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	var oe OpenAIError
	oe.Error.Message = msg
	oe.Error.Type = typeForStatus(status)
	oe.Error.Code = code
	writeJSON(w, status, oe)
}

func typeForStatus(status int) string {
	switch {
	case status == http.StatusBadRequest:
		return "invalid_request_error"
	case status == http.StatusUnauthorized:
		return "authentication_error"
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status >= 500:
		return "upstream_error"
	default:
		return "api_error"
	}
}

// mapUpstreamErr 把上游错误翻成 HTTP 状态 + 错误码。
func mapUpstreamErr(err error) (int, string, string) {
	var ae *lovart.APIError
	if errors.As(err, &ae) {
		switch ae.Kind {
		case lovart.ErrQuota:
			return http.StatusTooManyRequests, "upstream_quota_exhausted", ae.Message
		case lovart.ErrConcurrency:
			return http.StatusServiceUnavailable, "upstream_concurrency", ae.Message
		case lovart.ErrAuth:
			return http.StatusBadGateway, "upstream_auth", ae.Message + "（账号 token 失效）"
		case lovart.ErrUnsupported:
			return http.StatusBadRequest, "model_not_supported", ae.Message
		case lovart.ErrSignature:
			return http.StatusBadGateway, "upstream_signature", ae.Message + "（可能是上游改了签名 wasm）"
		default:
			return http.StatusBadGateway, "upstream_error", ae.Message
		}
	}
	if errors.Is(err, pool.ErrNoAccount) {
		return http.StatusServiceUnavailable, "no_account", "没有可用账号（全部冷却中或未配置）"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return http.StatusGatewayTimeout, "timeout", "上游处理超时"
	}
	return http.StatusBadGateway, "upstream_error", err.Error()
}

// parseIntDefault 读 int 参数。
func parseIntDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// logf 写运行日志（同时进内存环与 stderr）。
func (s *Server) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Print(msg)
}
