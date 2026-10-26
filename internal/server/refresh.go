package server

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/yuanjie-ai/lovart2api/internal/lovart"
	"github.com/yuanjie-ai/lovart2api/internal/pool"
)

// refreshScanInterval 后台续期扫描间隔。
//
// accessToken 有效期 7 天，这里每 6 小时扫一次，只在「剩余不足 2 天」时才真正刷新 ——
// 刷得太勤没必要（每次刷新都会轮换 refreshToken，多刷等于多一次失败机会）。
const refreshScanInterval = 6 * time.Hour

// refreshThreshold 剩余有效期低于它就续期。
const refreshThreshold = 48 * time.Hour

// refreshMinGap 两次刷新之间的最小间隔。
//
// 🔴 上游 WAF 会对 refresh 接口做频控：实测 30 分钟内连刷 3 次就被拦，
// 返回 `405 Sorry, your request has been blocked...`。所以这里强制冷却，
// 避免面板上反复点"续期"把自己点进黑名单。
const refreshMinGap = time.Hour

// StartBackground 启动后台任务（目前只有续期）。
func (s *Server) StartBackground(ctx context.Context) {
	go s.refreshLoop(ctx)
}

// refreshLoop 常驻协程：定期把快过期的账号续上。
func (s *Server) refreshLoop(ctx context.Context) {
	// 启动后先等 1 分钟，避开初始化时的上游请求高峰
	select {
	case <-time.After(time.Minute):
	case <-ctx.Done():
		return
	}
	for {
		s.refreshDue(ctx)
		select {
		case <-time.After(refreshScanInterval):
		case <-ctx.Done():
			return
		}
	}
}

// refreshDue 扫一遍，把该刷的号刷掉。
func (s *Server) refreshDue(ctx context.Context) {
	now := time.Now()
	for _, a := range s.Pool.List() {
		if !a.Enabled || a.RefreshToken == "" {
			continue
		}
		// 过期时间未知（老数据）也刷一次，顺便把 exp 补上
		if a.TokenExpireAt > 0 && time.Unix(a.TokenExpireAt, 0).Sub(now) > refreshThreshold {
			continue
		}
		if err := s.refreshOne(ctx, a.ID, a.RefreshToken); err != nil {
			_ = s.Pool.Update(a.ID, func(x *pool.Account) {
				x.LastError = "续期失败: " + truncateStr(err.Error(), 120)
			})
		}
	}
}

// refreshOne 刷新单个账号并把新凭证写回池子。
func (s *Server) refreshOne(ctx context.Context, id, refreshToken string) error {
	client := lovart.NewClient("", s.Signer)
	client.WWWBase = s.Cfg.Upstream.WWWBase
	client.LGWBase = s.Cfg.Upstream.LGWBase
	client.Lang = s.Cfg.Upstream.Lang

	sess, err := client.RefreshSession(ctx, refreshToken)
	if err != nil {
		return err
	}
	exp := lovart.TokenExpiry(sess.AccessToken)
	return s.Pool.Update(id, func(a *pool.Account) {
		a.Token = sess.AccessToken
		a.RefreshToken = sess.RefreshToken
		if !exp.IsZero() {
			a.TokenExpireAt = exp.Unix()
		}
		a.LastRefreshAt = time.Now().Unix()
		a.LastError = ""
		a.Fails = 0
	})
}

// panelRefreshAccount 手动刷新单个账号。
func (s *Server) panelRefreshAccount(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acct := s.Pool.Get(id)
	if acct == nil {
		writeErr(w, http.StatusNotFound, "not_found", "账号不存在")
		return
	}
	if acct.RefreshToken == "" {
		writeErr(w, http.StatusBadRequest, "no_refresh_token",
			"该账号没有 refreshToken（老数据），需要重新登录一次拿到")
		return
	}
	if wait := refreshCooldownLeft(acct); wait > 0 {
		writeErr(w, http.StatusTooManyRequests, "refresh_too_soon",
			"刚刚刷过，上游 WAF 会拦（405）。请 "+wait.Truncate(time.Minute).String()+" 后再试")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	if err := s.refreshOne(ctx, id, acct.RefreshToken); err != nil {
		writeErr(w, http.StatusBadGateway, "refresh_failed", err.Error())
		return
	}
	updated := s.Pool.Get(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"token_tail": tail(updated.Token, 6),
		"expire_at":  updated.TokenExpireAt,
		"expire_in":  humanizeUntil(updated.TokenExpireAt),
	})
}

// panelRefreshAll 手动刷新全部（有 refreshToken 的）。
func (s *Server) panelRefreshAll(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 120*time.Second)
	defer cancel()
	type res struct {
		ID    string `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	out := make([]res, 0)
	okN, failN := 0, 0
	for _, a := range s.Pool.List() {
		if a.RefreshToken == "" {
			continue
		}
		if refreshCooldownLeft(&a) > 0 {
			continue
		}
		if err := s.refreshOne(ctx, a.ID, a.RefreshToken); err != nil {
			out = append(out, res{ID: a.ID, OK: false, Error: truncateStr(err.Error(), 120)})
			failN++
		} else {
			out = append(out, res{ID: a.ID, OK: true})
			okN++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "refreshed": okN, "failed": failN, "results": out,
	})
}

// refreshCooldownLeft 距离下次允许刷新还有多久（0 = 可以刷）。
func refreshCooldownLeft(a *pool.Account) time.Duration {
	if a.LastRefreshAt == 0 {
		return 0
	}
	next := time.Unix(a.LastRefreshAt, 0).Add(refreshMinGap)
	if d := time.Until(next); d > 0 {
		return d
	}
	return 0
}

// humanizeUntil 把过期时间说成"还有多久"。
func humanizeUntil(expireAt int64) string {
	if expireAt == 0 {
		return "未知"
	}
	d := time.Until(time.Unix(expireAt, 0))
	if d <= 0 {
		return "已过期"
	}
	days := int(d.Hours() / 24)
	if days >= 1 {
		return fmt.Sprintf("%d 天 %d 小时", days, int(d.Hours())%24)
	}
	return d.Truncate(time.Minute).String()
}
