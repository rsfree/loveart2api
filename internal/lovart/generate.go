package lovart

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// CreateTask 提交生成任务。返回 generator_task_id。
//
// input 直接透传给上游的 input_args（含 prompt / image[] / aspect_ratio …）。
func (c *Client) CreateTask(ctx context.Context, projectID, generator string, input map[string]any) (string, error) {
	body := map[string]any{
		"generator_name": generator,
		"input_args":     input,
		"project_id":     projectID,
	}
	data, err := c.do(ctx, http.MethodPost, joinURL(c.LGWBase, "/v1/generator/tasks"), body, true)
	if err != nil {
		return "", err
	}
	var out struct {
		GeneratorTaskID string `json:"generator_task_id"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("解析任务响应失败: %w (raw=%s)", err, truncate(string(data), 200))
	}
	if out.GeneratorTaskID == "" {
		return "", &APIError{Kind: ErrUpstream, Message: "上游未返回 generator_task_id", Raw: truncate(string(data), 300)}
	}
	return out.GeneratorTaskID, nil
}

// GetTask 查询任务。task_id 走 query（放 body 会 422）。
func (c *Client) GetTask(ctx context.Context, taskID string) (*Task, error) {
	u := escapeQuery(c.LGWBase, "/v1/generator/tasks", map[string]string{"task_id": taskID})
	data, err := c.do(ctx, http.MethodGet, u, nil, true)
	if err != nil {
		return nil, err
	}
	var t Task
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("解析任务状态失败: %w (raw=%s)", err, truncate(string(data), 200))
	}
	return &t, nil
}

// WaitTask 轮询到终态。interval 为首次间隔，之后按 1.35 倍退避到 maxInterval。
func (c *Client) WaitTask(ctx context.Context, taskID string, interval, maxInterval time.Duration) (*Task, error) {
	if interval <= 0 {
		interval = time.Second
	}
	if maxInterval <= 0 {
		maxInterval = 5 * time.Second
	}
	deadline := time.Now().Add(15 * time.Minute)
	for {
		t, err := c.GetTask(ctx, taskID)
		if err != nil {
			return nil, err
		}
		if t.Done() {
			if t.Failed() {
				msg := firstNonEmpty(t.Error, t.ErrorCode, "任务失败")
				kind := ErrUpstream
				// 参数校验类失败是请求本身的问题，重试与换号都没用 —— 归到 unsupported，
				// 让上层直接返回 400 而不是把账号打进冷却。
				if isParamFailure(t.ErrorCode) || isParamFailure(t.Error) {
					kind = ErrUnsupported
				}
				return t, &APIError{Kind: kind, Code: t.ErrorCode, Message: msg}
			}
			return t, nil
		}
		if time.Now().After(deadline) {
			return t, fmt.Errorf("任务 %s 轮询超时（status=%s）", taskID, t.Status)
		}
		select {
		case <-ctx.Done():
			return t, ctx.Err()
		case <-time.After(interval):
		}
		if interval < maxInterval {
			interval = time.Duration(float64(interval) * 1.35)
			if interval > maxInterval {
				interval = maxInterval
			}
		}
	}
}

// ListGenerators 拉模型目录（www 域，不需要签名头）。
func (c *Client) ListGenerators(ctx context.Context) ([]Generator, error) {
	data, err := c.do(ctx, http.MethodPost, joinURL(c.WWWBase, "/api/canva/agent/v1/generators/list"), map[string]any{}, false)
	if err != nil {
		return nil, err
	}
	var out generatorListResp
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析模型目录失败: %w", err)
	}
	return out.Items, nil
}

// ListProjects 拉项目列表，用于自动挑选 project_id。
func (c *Client) ListProjects(ctx context.Context) ([]Project, error) {
	body := map[string]any{"page": 1, "pageSize": 20}
	data, err := c.do(ctx, http.MethodPost, joinURL(c.WWWBase, "/api/canva/project/projectList"), body, false)
	if err != nil {
		return nil, err
	}
	var out projectListResp
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析项目列表失败: %w", err)
	}
	return out.Data, nil
}

// ListThreads 拉会话列表（找 projectId 的兜底路径）。
func (c *Client) ListThreads(ctx context.Context) ([]Thread, error) {
	body := map[string]any{"page": 1, "pageSize": 20}
	data, err := c.do(ctx, http.MethodPost, joinURL(c.WWWBase, "/api/canva/agent/agentThreadList"), body, false)
	if err != nil {
		return nil, err
	}
	var out threadListResp
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("解析会话列表失败: %w", err)
	}
	return out.Data, nil
}

// ResolveProjectID 找到一个可用的 project_id：优先项目列表，其次会话列表。
//
// 上游要求 project_id 必填，产物会落到该项目；账号第一次使用时通常至少有一个
// （网页端随便发过一条消息就会建项目）。
func (c *Client) ResolveProjectID(ctx context.Context) (string, error) {
	if ps, err := c.ListProjects(ctx); err == nil {
		for _, p := range ps {
			if p.ProjectID != "" {
				return p.ProjectID, nil
			}
		}
	}
	if ts, err := c.ListThreads(ctx); err == nil {
		for _, t := range ts {
			if t.ProjectID != "" {
				return t.ProjectID, nil
			}
		}
	}
	return "", fmt.Errorf("账号下找不到任何项目，请先在网页端随便发一条消息创建一个项目")
}

// Download 拉取产物字节（用于把结果转 base64 返回）。
func (c *Client) Download(ctx context.Context, rawURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("user-agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, "", fmt.Errorf("下载产物失败: HTTP %d", resp.StatusCode)
	}
	buf, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, "", err
	}
	return buf, resp.Header.Get("content-type"), nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// isParamFailure 判断错误码/文案是否属于「请求参数不合法」。
func isParamFailure(s string) bool {
	up := strings.ToUpper(s)
	for _, k := range []string{"VALIDATION", "INVALID_ARGUMENT", "INVALID_PARAM", "BAD_REQUEST"} {
		if strings.Contains(up, k) {
			return true
		}
	}
	return false
}

// putOSS 直传 OSS。
//
// 🔴 绝不能设置 Content-Type：上游预签名时把 content-type 留空，
// OSS 的 StringToSign 会带上请求里的 content-type，多带一个头就 403。
func (c *Client) putOSS(ctx context.Context, uploadURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return err
	}
	// 显式不设置任何头（含 Content-Type / Content-Length 由 Go 自己按需补）
	req.Header.Del("Content-Type")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("上传 OSS 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("上传 OSS 失败: HTTP %d %s", resp.StatusCode, truncate(string(body), 200))
	}
	io.Copy(io.Discard, resp.Body)
	return nil
}
