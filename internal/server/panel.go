package server

import (
	"context"
	"crypto/subtle"
	_ "embed"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yuanjie-ai/lovart2api/internal/lovart"
	"github.com/yuanjie-ai/lovart2api/internal/pool"
)

//go:embed panel.html
var panelHTML []byte

// panelRoutes 注册面板路由。
func (s *Server) panelRoutes() {
	s.mux.HandleFunc("GET /panel", s.servePanel)
	s.mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, "/panel", http.StatusFound)
	})

	s.mux.HandleFunc("GET /panel/api/overview", s.guardPanel(s.panelOverview))
	s.mux.HandleFunc("GET /panel/api/accounts", s.guardPanel(s.panelAccounts))
	s.mux.HandleFunc("POST /panel/api/accounts", s.guardPanel(s.panelAddAccount))
	s.mux.HandleFunc("DELETE /panel/api/accounts/{id}", s.guardPanel(s.panelDeleteAccount))
	s.mux.HandleFunc("POST /panel/api/accounts/{id}/toggle", s.guardPanel(s.panelToggleAccount))
	s.mux.HandleFunc("POST /panel/api/accounts/{id}/probe", s.guardPanel(s.panelProbeAccount))
	s.mux.HandleFunc("GET /panel/api/logs", s.guardPanel(s.panelLogs))
	s.mux.HandleFunc("GET /panel/api/models", s.guardPanel(s.panelModels))
}

func (s *Server) servePanel(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 面板是内嵌单文件，禁缓存以免升级后浏览器还跑旧 JS
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	w.Write(panelHTML)
}

// guardPanel 面板口令校验：X-Panel-Key 头或 ?key= 查询参数；未配置口令时只允许本机。
func (s *Server) guardPanel(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.Cfg.Panel.Auth != "" {
			got := firstNonEmpty(r.Header.Get("X-Panel-Key"), r.URL.Query().Get("key"))
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.Cfg.Panel.Auth)) != 1 {
				writeErr(w, http.StatusUnauthorized, "panel_auth", "面板口令错误")
				return
			}
			next(w, r)
			return
		}
		if ip := clientIP(r); ip != "127.0.0.1" && ip != "::1" && !strings.HasPrefix(ip, "192.168.") && !strings.HasPrefix(ip, "10.") {
			writeErr(w, http.StatusForbidden, "panel_local_only", "面板未设置口令，仅允许内网访问")
			return
		}
		next(w, r)
	}
}

func (s *Server) panelOverview(w http.ResponseWriter, r *http.Request) {
	st := s.Pool.Stats()
	writeJSON(w, http.StatusOK, map[string]any{
		"service":  "lovart2api",
		"uptime":   time.Since(serverStart).Round(time.Second).String(),
		"accounts": st,
		"requests": s.Log.Count(),
		"config": map[string]any{
			"listen":       s.Cfg.Listen,
			"www_base":     s.Cfg.Upstream.WWWBase,
			"lgw_base":     s.Cfg.Upstream.LGWBase,
			"default":      s.Cfg.Images.DefaultModel,
			"default_edit": s.Cfg.Images.DefaultEditModel,
			"download":     s.Cfg.Images.DownloadResult,
		},
	})
}

// powerFor 查账号积分，带 60 秒缓存。
func (s *Server) powerFor(id, token string) powerSnapshot {
	s.powerMu.Lock()
	snap, ok := s.powerCache[id]
	s.powerMu.Unlock()
	if ok && time.Since(snap.At) < 60*time.Second {
		return snap
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	client := lovart.NewClient(token, s.Signer)
	client.WWWBase = s.Cfg.Upstream.WWWBase
	client.LGWBase = s.Cfg.Upstream.LGWBase
	client.Lang = s.Cfg.Upstream.Lang
	p, err := client.GetPower(ctx)
	snap = powerSnapshot{Power: p, At: time.Now()}
	if err != nil {
		snap.Err = err.Error()
	}
	s.powerMu.Lock()
	s.powerCache[id] = snap
	s.powerMu.Unlock()
	return snap
}

func (s *Server) panelAccounts(w http.ResponseWriter, r *http.Request) {
	accs := s.Pool.List()
	out := make([]map[string]any, 0, len(accs))
	now := time.Now()
	dailyLimit := s.Cfg.Pool.DailyLimit // 0 表示不本地拦，面板展示为「不限」
	// 并发查积分，最慢一个决定响应时间
	powers := make([]powerSnapshot, len(accs))
	var wg sync.WaitGroup
	for i := range accs {
		if !accs[i].Enabled || accs[i].Token == "" {
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			powers[i] = s.powerFor(accs[i].ID, accs[i].Token)
		}(i)
	}
	wg.Wait()
	for idx, a := range accs {
		cooling := a.Cooling(now)
		var coolLeft int64
		if cooling {
			coolLeft = a.CooldownUntil - now.Unix()
		}
		used := 0
		if a.UsageDay == now.Format("2006-01-02") {
			used = a.UsageCount
		}
		// 按模型冷却的摘要（只列还在冷却中的）
		cooledModels := make([]string, 0, len(a.ModelCooldown))
		for gen, until := range a.ModelCooldown {
			if until > now.Unix() {
				cooledModels = append(cooledModels, gen)
			}
		}
		// 积分
		powerField := map[string]any{}
		if p := powers[idx]; p.Power != nil {
			powerField["power_paid"] = p.Power.Paid
			powerField["power_free"] = p.Power.Free
			powerField["power_daily"] = p.Power.DailyQuota
		} else if p.Err != "" {
			powerField["power_err"] = truncateStr(p.Err, 80)
		}

		row := map[string]any{
			"id":            a.ID,
			"email":         a.Email,
			"uid":           a.UID,
			"enabled":       a.Enabled,
			"project_id":    a.ProjectID,
			"kit_id":        a.KitID,
			"cooldown_sec":  coolLeft,
			"cooled_models": cooledModels,
			"core_cooldown_sec": maxInt64(a.CoreCooldownUntil-now.Unix(), 0),
			"last_error":    a.LastError,
			"usage_today":   used,
			"daily_limit":   dailyLimit,
			"quota_hit":     dailyLimit > 0 && used >= dailyLimit,
			"success_count": a.SuccessCount,
			"fail_count":    a.FailCount,
			"last_used":     a.LastUsed,
			"token_tail":    tail(a.Token, 6),
		}
		for k, v := range powerField {
			row[k] = v
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{"accounts": out})
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

func (s *Server) panelAddAccount(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token string `json:"token"`
		Email string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	body.Token = strings.TrimSpace(body.Token)
	if body.Token == "" {
		writeErr(w, http.StatusBadRequest, "missing_token", "token 不能为空")
		return
	}

	// 立即探测一次，验证 token 并顺带补齐 uid/email/project/kit
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	client := lovart.NewClient(body.Token, s.Signer)
	client.WWWBase = s.Cfg.Upstream.WWWBase
	client.LGWBase = s.Cfg.Upstream.LGWBase
	client.Lang = s.Cfg.Upstream.Lang

	if _, err := client.ListGenerators(ctx); err != nil {
		writeErr(w, http.StatusBadRequest, "token_invalid", "token 校验失败: "+err.Error())
		return
	}
	pid, _ := client.ResolveProjectID(ctx)

	acct, created, err := s.Pool.Add(body.Token, body.Email, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "pool_error", err.Error())
		return
	}
	if pid != "" {
		_ = s.Pool.Update(acct.ID, func(a *pool.Account) { a.ProjectID = pid })
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"created":    created,
		"id":         acct.ID,
		"project_id": pid,
	})
}

func (s *Server) panelDeleteAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.Pool.Remove(id); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) panelToggleAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Enabled bool `json:"enabled"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&body)
	if err := s.Pool.SetEnabled(id, body.Enabled); err != nil {
		writeErr(w, http.StatusNotFound, "not_found", err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": body.Enabled})
}

// panelProbeAccount 用账号跑一次只读探测（模型目录 + 项目），用于面板上的「测试」。
func (s *Server) panelProbeAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var target *pool.Account
	for _, a := range s.Pool.List() {
		if a.ID == id {
			cp := a
			target = &cp
			break
		}
	}
	if target == nil {
		writeErr(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()
	client := lovart.NewClient(target.Token, s.Signer)
	client.WWWBase = s.Cfg.Upstream.WWWBase
	client.LGWBase = s.Cfg.Upstream.LGWBase
	client.Lang = s.Cfg.Upstream.Lang

	gens, err := client.ListGenerators(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	pid, err := client.ResolveProjectID(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "取项目失败: " + err.Error()})
		return
	}
	if target.ProjectID == "" {
		_ = s.Pool.Update(id, func(a *pool.Account) { a.ProjectID = pid })
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "generators": len(gens), "project_id": pid})
}

func (s *Server) panelLogs(w http.ResponseWriter, r *http.Request) {
	n := parseIntDefault(r.URL.Query().Get("limit"), 100)
	writeJSON(w, http.StatusOK, map[string]any{"logs": s.Log.Recent(n), "total": s.Log.Count()})
}

func (s *Server) panelModels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"models": s.Cfg.Models})
}
