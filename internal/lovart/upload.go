package lovart

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
)

// EnsureKit 保证账号下有一个可写素材库，返回 kit_id。
//
// 上游的 kit 更像「用户级素材空间」：创建一次后所有上传都往里放。
// 传 kitID 非空时直接复用（配置文件里可以钉死）。
func (c *Client) EnsureKit(ctx context.Context, kitID, name string) (string, error) {
	if strings.TrimSpace(kitID) != "" {
		return kitID, nil
	}
	if name == "" {
		name = "lovart2api"
	}
	data, err := c.do(ctx, http.MethodPost, joinURL(c.LGWBase, "/v1/kit/create"), map[string]any{"name": name}, true)
	if err != nil {
		return "", err
	}
	var kit Kit
	if err := json.Unmarshal(data, &kit); err != nil {
		return "", fmt.Errorf("解析 kit 响应失败: %w", err)
	}
	if kit.ID == "" {
		return "", fmt.Errorf("上游未返回 kit id (raw=%s)", truncate(string(data), 200))
	}
	return kit.ID, nil
}

// UploadImage 把一份图片字节传到上游素材库，返回可直接用于 input_args.image 的 URL。
//
// 三段式：presign → PUT OSS → confirm。文件名只影响扩展名校验与展示。
func (c *Client) UploadImage(ctx context.Context, kitID, fileName string, data []byte) (string, error) {
	if len(data) == 0 {
		return "", fmt.Errorf("空图片")
	}
	if kitID == "" {
		return "", fmt.Errorf("kit_id 为空")
	}
	ext := strings.ToLower(path.Ext(fileName))
	if ext == "" {
		ext = ".png"
	}
	// 上游按扩展名白名单校验，统一成安全集合
	switch ext {
	case ".png", ".jpg", ".jpeg", ".webp", ".gif", ".bmp":
	default:
		ext = ".png"
	}
	if !strings.HasSuffix(strings.ToLower(fileName), ext) {
		fileName = strings.TrimSuffix(fileName, path.Ext(fileName)) + ext
	}

	pre, err := c.presign(ctx, kitID, "image", fileName)
	if err != nil {
		return "", err
	}
	if pre.MaxSize > 0 && int64(len(data)) > pre.MaxSize {
		return "", fmt.Errorf("图片超过上游上限 %d 字节", pre.MaxSize)
	}
	if err := c.putOSS(ctx, pre.UploadURL, data); err != nil {
		return "", err
	}
	return c.confirm(ctx, pre.CallbackToken)
}

func (c *Client) presign(ctx context.Context, kitID, assetType, fileName string) (*Presign, error) {
	body := map[string]any{
		"kit_id":     kitID,
		"asset_type": assetType,
		"file_name":  fileName,
	}
	data, err := c.do(ctx, http.MethodPost, joinURL(c.LGWBase, "/v1/kit/asset/presign"), body, true)
	if err != nil {
		return nil, err
	}
	var pre Presign
	if err := json.Unmarshal(data, &pre); err != nil {
		return nil, fmt.Errorf("解析 presign 失败: %w", err)
	}
	if pre.UploadURL == "" || pre.CallbackToken == "" {
		return nil, fmt.Errorf("presign 返回缺少字段 (raw=%s)", truncate(string(data), 200))
	}
	return &pre, nil
}

func (c *Client) confirm(ctx context.Context, callbackToken string) (string, error) {
	body := map[string]any{"callback_token": callbackToken}
	data, err := c.do(ctx, http.MethodPost, joinURL(c.LGWBase, "/v1/kit/asset/confirm"), body, true)
	if err != nil {
		return "", err
	}
	var out confirmResp
	if err := json.Unmarshal(data, &out); err != nil {
		return "", fmt.Errorf("解析 confirm 失败: %w", err)
	}
	for _, f := range out.Asset.Files {
		if f.OSSURL != "" {
			return f.OSSURL, nil
		}
	}
	return "", fmt.Errorf("confirm 未返回 oss_url (raw=%s)", truncate(string(data), 200))
}
