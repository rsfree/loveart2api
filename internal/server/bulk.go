package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yuanjie-ai/lovart2api/internal/lovart"
	"github.com/yuanjie-ai/lovart2api/internal/pool"
)

// bulkConcurrency 批量校验的并发度。
//
// 上游对同一 IP 的登录/查询类接口有软限速，并发太高会被 Cloudflare 拦；
// 4 路实测稳定，100 个账号约 40 秒跑完。
const bulkConcurrency = 4

// bulkItem 单个 token 的导入结果。
type bulkItem struct {
	TokenTail string `json:"token_tail"`
	OK        bool   `json:"ok"`
	Created   bool   `json:"created"`
	AccountID string `json:"account_id,omitempty"`
	ProjectID string `json:"project_id,omitempty"`
	Error     string `json:"error,omitempty"`
}

// panelBulkAccounts 批量导入账号。
//
// 请求体两种形态都收：
//
//	{"tokens": "tok1\ntok2\ntok3"}          // 多行文本（面板粘贴）
//	{"tokens": ["tok1","tok2"]}             // 数组
//
// 分隔符兼容换行 / 逗号 / 空白 / 分号 —— 用户从别处复制过来什么形态都能吃。
func (s *Server) panelBulkAccounts(w http.ResponseWriter, r *http.Request) {
	var raw struct {
		Tokens json.RawMessage `json:"tokens"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&raw); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_json", err.Error())
		return
	}
	tokens := parseTokenList(raw.Tokens)
	if len(tokens) == 0 {
		writeErr(w, http.StatusBadRequest, "missing_tokens", "没解析到任何 token")
		return
	}
	if len(tokens) > 200 {
		writeErr(w, http.StatusBadRequest, "too_many", "单次最多 200 个 token")
		return
	}

	// 并发校验：每个 token 都要先证明能用（拉模型目录），顺手探 project_id
	results := make([]bulkItem, len(tokens))
	sem := make(chan struct{}, bulkConcurrency)
	var wg sync.WaitGroup
	for i, tok := range tokens {
		wg.Add(1)
		go func(i int, tok string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			item := bulkItem{TokenTail: tail(tok, 6)}
			ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
			defer cancel()

			client := lovart.NewClient(tok, s.Signer)
			client.WWWBase = s.Cfg.Upstream.WWWBase
			client.LGWBase = s.Cfg.Upstream.LGWBase
			client.Lang = s.Cfg.Upstream.Lang

			if _, err := client.ListGenerators(ctx); err != nil {
				item.Error = truncateStr(err.Error(), 160)
				results[i] = item
				return
			}
			pid, _ := client.ResolveProjectID(ctx)
			acct, created, err := s.Pool.Add(tok, "", 0)
			if err != nil {
				item.Error = err.Error()
				results[i] = item
				return
			}
			if pid != "" {
				_ = s.Pool.Update(acct.ID, func(a *pool.Account) { a.ProjectID = pid })
			}
			item.OK, item.Created, item.AccountID, item.ProjectID = true, created, acct.ID, pid
			results[i] = item
		}(i, tok)
	}
	wg.Wait()

	added, failed := 0, 0
	for _, it := range results {
		if it.OK {
			added++
		} else {
			failed++
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"total":   len(tokens),
		"added":   added,
		"failed":  failed,
		"results": results,
	})
}

// parseTokenList 把各种形态的输入拆成 token 列表（去重、去空）。
func parseTokenList(raw json.RawMessage) []string {
	var s string
	var arr []string
	if err := json.Unmarshal(raw, &s); err != nil {
		if err2 := json.Unmarshal(raw, &arr); err2 != nil {
			return nil
		}
	} else {
		arr = strings.FieldsFunc(s, func(r rune) bool {
			switch r {
			case '\n', '\r', ',', ';', '\t', ' ':
				return true
			}
			return false
		})
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(arr))
	for _, t := range arr {
		t = strings.TrimSpace(t)
		// 容忍从 DevTools / curl 里带出来的引号和 JSON 包裹
		t = strings.Trim(t, `"'`)
		t = strings.TrimPrefix(t, "token=")
		if t == "" || len(t) < 20 || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}
