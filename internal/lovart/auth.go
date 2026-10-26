package lovart

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Session 登录返回的一对凭证。
//
// accessToken 是网关鉴权用的（放 `token` 头），有效期 **7 天**；
// refreshToken 有效期约 90 天，且**每次刷新都会轮换**——用完必须把新值落库，
// 否则下一次刷新会失败。
type Session struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	UserID       int64  `json:"userId"`
	Email        string `json:"email"`
}

// RefreshSession 用 refreshToken 换一对新凭证。
//
// 🔴 必须带 `X-LVT-FP-REQUEST-ID` 头：缺了上游返回
// `1500000400 token refresh error: fpRequestId is empty`。该值是客户端设备指纹
// 请求 ID，服务端只校验"非空"，实测 `fp`+随机串即可通过。
func (c *Client) RefreshSession(ctx context.Context, refreshToken string) (*Session, error) {
	if strings.TrimSpace(refreshToken) == "" {
		return nil, fmt.Errorf("refreshToken 为空")
	}
	fp := "fp" + randomHex(16)
	data, err := c.do(ctx, http.MethodPost,
		joinURL(c.WWWBase, "/api/www/lovart/login/v2/token/refresh"),
		map[string]any{"refreshToken": refreshToken}, false,
		withHeader("X-LVT-FP-REQUEST-ID", fp),
		withHeader("Origin", c.WWWBase),
	)
	if err != nil {
		return nil, err
	}
	var out struct {
		AccessToken  string `json:"accessToken"`
		RefreshToken string `json:"refreshToken"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析刷新响应失败: %w (raw=%s)", err, truncate(string(data), 200))
	}
	if out.AccessToken == "" {
		return nil, fmt.Errorf("刷新未返回 accessToken (raw=%s)", truncate(string(data), 200))
	}
	if out.RefreshToken == "" {
		// 上游偶尔不回新 RT，此时沿用旧值（旧值在有效期内仍然可用）
		out.RefreshToken = refreshToken
	}
	return &Session{AccessToken: out.AccessToken, RefreshToken: out.RefreshToken}, nil
}

// TokenUserID 从 accessToken 的 JWT 里取 userId。
//
// 🔴 用途：refresh 会轮换 token，如果账号池按 token 去重就会"每刷新一次多一个号"。
// 用 userId 作为稳定标识才能把同一账号的多次登录收敛到一条记录。
func TokenUserID(token string) int64 {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return 0
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var claims struct {
		UserID int64 `json:"userId"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return 0
	}
	return claims.UserID
}

// TokenExpiry 解析 JWT 的 exp，返回 accessToken 的过期时间。
// 解析失败返回零值（调用方当作"未知"，选择保守策略）。
func TokenExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}
	}
	return time.Unix(claims.Exp, 0)
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
