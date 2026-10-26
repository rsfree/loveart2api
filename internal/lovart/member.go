package lovart

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Power 账号的算力（积分）余额。
//
// 上游把积分拆成两池：
//   - 付费/赠送池（type 3）：长期有效，用完就没了
//   - 每日免费池（type 4）：每天分配（实测 70），当日清零
//
// 注意积分和「核心模型 5 次/天」是**两套独立限制**：积分够也可能被次数配额挡住。
type Power struct {
	Paid int `json:"paid"`
	Free int `json:"free"`
	// 今日免费额度分配情况（拿不到时为 0）
	DailyQuota int `json:"daily_quota"`
	// AllocatedCount 当日已领取/已用次数（上游字段 allocatedCount）
	AllocatedCount int `json:"allocated_count"`
}

// Total 两池合计。
func (p *Power) Total() int { return p.Paid + p.Free }

// powerDetailResp 对应 /api/www/lovart/member/power/detail 的 data 数组。
type powerDetailResp []struct {
	Amount  int    `json:"amount"`
	Type    int    `json:"type"`
	Details []any  `json:"details"`
}

// GetPower 查积分余额。走 www 域（不要签名头）。
func (c *Client) GetPower(ctx context.Context) (*Power, error) {
	data, err := c.do(ctx, http.MethodGet, joinURL(c.WWWBase, "/api/www/lovart/member/power/detail"), nil, false)
	if err != nil {
		return nil, err
	}
	var rows powerDetailResp
	if err := json.Unmarshal(data, &rows); err != nil {
		return nil, fmt.Errorf("解析积分余额失败: %w", err)
	}
	p := &Power{}
	for _, r := range rows {
		switch r.Type {
		case 3:
			p.Paid = r.Amount
		case 4:
			p.Free = r.Amount
		}
	}
	// 免费池的每日分配信息在另一个接口，失败不影响余额
	if data, err := c.do(ctx, http.MethodGet, joinURL(c.WWWBase, "/api/www/lovart/member/free/power"), nil, false); err == nil {
		var fp struct {
			AllocatedToday int `json:"allocatedToday"`
			AllocatedCount int `json:"allocatedCount"`
		}
		if json.Unmarshal(data, &fp) == nil {
			p.DailyQuota = fp.AllocatedToday
			p.AllocatedCount = fp.AllocatedCount
		}
	}
	return p, nil
}
