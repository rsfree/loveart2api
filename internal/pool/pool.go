// Package pool 管理 Lovart 账号池：选号、并发闸门、冷却与状态持久化。
//
// 上游的限制都是**按账号**的（核心模型 5 次/天、并发 1），所以池的核心职责是
// 把「不能用的号」尽快摘出去，并在可用号之间轮转。
package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// ErrNoAccount 池里没有可用账号。
var ErrNoAccount = errors.New("没有可用账号（全部冷却中或未配置）")

// Account 一个 Lovart 账号。
type Account struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	Email string `json:"email,omitempty"`
	UID   int64  `json:"uid,omitempty"`
	// ProjectID 生成任务必填；首次使用时自动探测并写回。
	ProjectID string `json:"project_id,omitempty"`
	// KitID 素材库；首次上传时创建并写回，避免每次上传都建新库。
	KitID string `json:"kit_id,omitempty"`

	Enabled bool `json:"enabled"`

	// —— 运行时状态（会持久化，重启后仍然生效）——
	CooldownUntil int64  `json:"cooldown_until,omitempty"`
	// ModelCooldown 按 generator 的冷却：核心模型当日额度满了不该带着 flux 一起躺下。
	ModelCooldown map[string]int64 `json:"model_cooldown,omitempty"`
	LastError     string `json:"last_error,omitempty"`
	Fails         int    `json:"fails,omitempty"`
	UsageDay      string `json:"usage_day,omitempty"`
	UsageCount    int    `json:"usage_count,omitempty"`
	SuccessCount  int64  `json:"success_count,omitempty"`
	FailCount     int64  `json:"fail_count,omitempty"`
	LastUsed      int64  `json:"last_used,omitempty"`
}

// Cooling 是否在冷却中。
func (a *Account) Cooling(now time.Time) bool {
	return a.CooldownUntil > now.Unix()
}

// ModelCooling 指定 generator 是否在冷却中。
func (a *Account) ModelCooling(generator string, now time.Time) bool {
	if generator == "" || len(a.ModelCooldown) == 0 {
		return false
	}
	return a.ModelCooldown[generator] > now.Unix()
}

// QuotaExhaustedToday 本地估算的当日额度是否已满。
func (a *Account) QuotaExhaustedToday(now time.Time, dailyLimit int) bool {
	if dailyLimit <= 0 {
		return false
	}
	return a.UsageDay == now.Format("2006-01-02") && a.UsageCount >= dailyLimit
}

// state 落盘结构。
type state struct {
	Accounts []*Account `json:"accounts"`
	SavedAt  int64      `json:"saved_at"`
}

// Pool 账号池。
type Pool struct {
	mu       sync.Mutex
	accounts []*Account
	stateFile string

	maxPerAccount int
	quotaCooldown time.Duration
	errCooldown   time.Duration
	dailyLimit    int

	// sem 按账号 ID 做并发闸门（上游并发 1）。
	sem map[string]chan struct{}
}

// Options 池参数。
type Options struct {
	StateFile     string
	MaxPerAccount int
	QuotaCooldown time.Duration
	ErrorCooldown time.Duration
	// DailyLimit 本地额度估算上限（0 表示不限制，仅靠上游 1200000409 兜底）。
	DailyLimit int
}

// New 加载或初始化账号池。
func New(opt Options) (*Pool, error) {
	if opt.MaxPerAccount <= 0 {
		opt.MaxPerAccount = 1
	}
	if opt.StateFile == "" {
		opt.StateFile = "./data/state.json"
	}
	p := &Pool{
		stateFile:     opt.StateFile,
		maxPerAccount: opt.MaxPerAccount,
		quotaCooldown: opt.QuotaCooldown,
		errCooldown:   opt.ErrorCooldown,
		dailyLimit:    opt.DailyLimit,
		sem:           make(map[string]chan struct{}),
	}
	if err := p.load(); err != nil {
		return nil, err
	}
	return p, nil
}

func (p *Pool) load() error {
	b, err := os.ReadFile(p.stateFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("解析状态文件 %s: %w", p.stateFile, err)
	}
	p.accounts = s.Accounts
	for _, a := range p.accounts {
		if a.ID == "" {
			a.ID = AccountID(a.Token)
		}
		if a.Enabled == false && a.Token == "" {
			continue
		}
		// 历史文件里 Enabled 默认 false 会让账号全变成禁用，这里按 token 存在与否校正
		if a.Token != "" {
			a.Enabled = true
		}
	}
	return nil
}

// save 原子落盘。调用方需持有 p.mu。
func (p *Pool) save() error {
	dir := filepath.Dir(p.stateFile)
	if dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return err
		}
	}
	s := state{Accounts: p.accounts, SavedAt: time.Now().Unix()}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp := p.stateFile + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, p.stateFile)
}

// Save 显式落盘（面板改配置后调用）。
func (p *Pool) Save() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.save()
}

// AccountID 用 token 派生稳定 id（同一 token 重复导入不会产生新账号）。
func AccountID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "lv_" + hex.EncodeToString(sum[:6])
}

// Add 添加或更新账号（按 token 哈希去重）。
func (p *Pool) Add(token, email string, uid int64) (*Account, bool, error) {
	token = trim(token)
	if token == "" {
		return nil, false, errors.New("token 为空")
	}
	id := AccountID(token)
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.ID == id {
			a.Token = token
			if email != "" {
				a.Email = email
			}
			if uid != 0 {
				a.UID = uid
			}
			a.Enabled = true
			_ = p.save()
			return a, false, nil
		}
	}
	a := &Account{ID: id, Token: token, Email: email, UID: uid, Enabled: true}
	p.accounts = append(p.accounts, a)
	_ = p.save()
	return a, true, nil
}

// Remove 删除账号。
func (p *Pool) Remove(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := p.accounts[:0]
	found := false
	for _, a := range p.accounts {
		if a.ID == id {
			found = true
			continue
		}
		out = append(out, a)
	}
	if !found {
		return fmt.Errorf("账号 %s 不存在", id)
	}
	p.accounts = out
	delete(p.sem, id)
	return p.save()
}

// SetEnabled 启停账号。重新启用视为「人工确认可以再试」，一并清掉冷却与本地额度估算。
func (p *Pool) SetEnabled(id string, on bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.ID == id {
			a.Enabled = on
			if on {
				a.CooldownUntil = 0
				a.Fails = 0
				a.LastError = ""
				a.UsageDay = ""
				a.UsageCount = 0
				a.ModelCooldown = nil
			}
			return p.save()
		}
	}
	return fmt.Errorf("账号 %s 不存在", id)
}

// Update 用回调修改账号并落盘（面板改 project_id / kit_id 用）。
func (p *Pool) Update(id string, fn func(*Account)) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, a := range p.accounts {
		if a.ID == id {
			fn(a)
			return p.save()
		}
	}
	return fmt.Errorf("账号 %s 不存在", id)
}

// List 返回账号快照（深拷贝，避免外部改到内部状态）。
func (p *Pool) List() []Account {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]Account, 0, len(p.accounts))
	for _, a := range p.accounts {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed < out[j].LastUsed })
	return out
}

// Stats 汇总。
type Stats struct {
	Total     int `json:"total"`
	Enabled   int `json:"enabled"`
	Available int `json:"available"`
	Cooling   int `json:"cooling"`
	UsageDay  string `json:"usage_day"`
	UsageSum  int    `json:"usage_sum"`
}

// Stats 当前池状态。
func (p *Pool) Stats() Stats {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	today := now.Format("2006-01-02")
	st := Stats{Total: len(p.accounts), UsageDay: today}
	for _, a := range p.accounts {
		if a.Enabled {
			st.Enabled++
		}
		if a.Enabled && !a.Cooling(now) {
			st.Available++
		}
		if a.Cooling(now) {
			st.Cooling++
		}
		if a.UsageDay == today {
			st.UsageSum += a.UsageCount
		}
	}
	return st
}

// Lease 是一次账号占用。
type Lease struct {
	Account *Account
	pool    *Pool
	// generator 本次请求用的上游模型，用于按模型隔离冷却。
	generator string
	once      sync.Once
}

// Release 归还账号。err 决定是否冷却。
func (l *Lease) Release(err error) {
	l.once.Do(func() {
		l.pool.report(l.Account.ID, l.generator, err)
	})
}

// Pick 按「最近最少使用」选一个可用账号，并占用其并发槽位。
//
// exclude 用于重试时排除刚失败的账号。
func (p *Pool) Pick(ctx context.Context, exclude map[string]bool, generator string) (*Lease, error) {
	deadline := time.Now().Add(30 * time.Second)
	for {
		p.mu.Lock()
		now := time.Now()
		var best *Account
		for _, a := range p.accounts {
			if !a.Enabled || a.Cooling(now) || a.ModelCooling(generator, now) {
				continue
			}
			if exclude != nil && exclude[a.ID] {
				continue
			}
			if a.QuotaExhaustedToday(now, p.dailyLimit) {
				continue
			}
			if best == nil || a.LastUsed < best.LastUsed {
				best = a
			}
		}
		if best == nil {
			p.mu.Unlock()
			if time.Now().After(deadline) {
				return nil, ErrNoAccount
			}
			// 有账号但都在并发占用中 —— 稍等再试
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}
		id := best.ID
		sem := p.sem[id]
		if sem == nil {
			sem = make(chan struct{}, p.maxPerAccount)
			p.sem[id] = sem
		}
		snapshot := *best
		p.mu.Unlock()

		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(500 * time.Millisecond):
			// 抢不到槽位就换下一个号（下一轮会因为 LastUsed 未变而再选中它，
			// 所以先把它在 exclude 里跳过一轮，避免空转）
			if exclude == nil {
				exclude = map[string]bool{}
			}
			exclude[id] = true
			continue
		}

		// 抢到槽位后重新确认账号仍可用
		p.mu.Lock()
		cur := p.findLocked(id)
		if cur == nil || !cur.Enabled || cur.Cooling(time.Now()) || cur.ModelCooling(generator, time.Now()) {
			p.mu.Unlock()
			<-sem
			continue
		}
		cur.LastUsed = time.Now().Unix()
		snapshot = *cur
		p.mu.Unlock()

		return &Lease{Account: &snapshot, pool: p, generator: generator}, nil
	}
}

func (p *Pool) findLocked(id string) *Account {
	for _, a := range p.accounts {
		if a.ID == id {
			return a
		}
	}
	return nil
}

// report 归还槽位并按结果调整账号状态。
func (p *Pool) report(id string, generator string, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if sem := p.sem[id]; sem != nil {
		select {
		case <-sem:
		default:
		}
	}
	a := p.findLocked(id)
	if a == nil {
		return
	}
	now := time.Now()
	if err == nil {
		a.Fails = 0
		a.LastError = ""
		a.CooldownUntil = 0
		a.SuccessCount++
		if a.UsageDay != now.Format("2006-01-02") {
			a.UsageDay = now.Format("2006-01-02")
			a.UsageCount = 0
		}
		a.UsageCount++
		_ = p.save()
		return
	}

	a.FailCount++
	a.LastError = err.Error()
	a.Fails++

	var kind string
	if ae, ok := err.(interface{ ErrorKind() string }); ok {
		kind = ae.ErrorKind()
	}
	switch {
	case kind == "quota":
		// 额度耗尽：只冷却**这个模型**到次日 04:00（上游额度是按模型档位算的，
		// 核心模型满了不该把 flux 一起带走）。generator 为空时退化成整号冷却。
		reset := nextQuotaReset(now).Unix()
		if generator != "" {
			if a.ModelCooldown == nil {
				a.ModelCooldown = map[string]int64{}
			}
			a.ModelCooldown[generator] = reset
		} else {
			a.CooldownUntil = reset
		}
		a.UsageDay = now.Format("2006-01-02")
		limit := p.dailyLimit
		if limit <= 0 {
			limit = 5
		}
		a.UsageCount = maxInt(a.UsageCount, limit)
	case kind == "auth":
		// token 失效：直接禁用，等人处理
		a.Enabled = false
		a.CooldownUntil = now.Add(24 * time.Hour).Unix()
	case kind == "concurrency":
		// 上游并发满：短冷却，让别的号上
		a.CooldownUntil = now.Add(3 * time.Second).Unix()
	case kind == "unsupported":
		// 参数/模型不支持：账号本身没问题，不冷却，只清失败计数
		a.Fails = 0
		a.CooldownUntil = 0
	default:
		cd := p.errCooldown
		if cd <= 0 {
			cd = 90 * time.Second
		}
		// 连续失败指数退避，封顶 30 分钟
		for i := 1; i < a.Fails && i < 6; i++ {
			cd *= 2
		}
		if cd > 30*time.Minute {
			cd = 30 * time.Minute
		}
		a.CooldownUntil = now.Add(cd).Unix()
	}
	_ = p.save()
}

// nextQuotaReset 返回下一个额度重置点（次日 04:00 本地时间）。
func nextQuotaReset(now time.Time) time.Time {
	reset := time.Date(now.Year(), now.Month(), now.Day(), 4, 0, 0, 0, now.Location())
	if !now.Before(reset) {
		reset = reset.Add(24 * time.Hour)
	}
	return reset
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func trim(s string) string {
	for len(s) > 0 && (s[0] == ' ' || s[0] == '\n' || s[0] == '\r' || s[0] == '\t' || s[0] == '"') {
		s = s[1:]
	}
	for len(s) > 0 {
		c := s[len(s)-1]
		if c == ' ' || c == '\n' || c == '\r' || c == '\t' || c == '"' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}
