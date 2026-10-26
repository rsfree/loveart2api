// Package config 定义网关配置。默认值优先，配置文件只写要覆盖的部分。
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Config 是网关全量配置。
type Config struct {
	Listen    string `json:"listen"`
	APIKey    string `json:"api_key"`
	StateFile string `json:"state_file"`

	Upstream Upstream `json:"upstream"`
	Images   Images   `json:"images"`
	Pool     Pool     `json:"pool"`
	Panel    Panel    `json:"panel"`
	Logging  Logging  `json:"logging"`

	// Models 是「对外模型名 → 上游 generator」的映射表。
	Models map[string]ModelSpec `json:"models"`
}

// Upstream 上游地址与语言。
type Upstream struct {
	WWWBase string `json:"www_base"`
	LGWBase string `json:"lgw_base"`
	Lang    string `json:"lang"`
}

// Images 图像接口行为。
type Images struct {
	DefaultModel     string `json:"default_model"`
	DefaultEditModel string `json:"default_edit_model"`
	PollInterval     string `json:"poll_interval"`
	PollMax          string `json:"poll_max"`
	Timeout          string `json:"timeout"`
	// DownloadResult 为 true 时网关把产物落成本地字节再回 b64（省客户端一次外网请求）。
	DownloadResult bool `json:"download_result"`
}

// Pool 账号池策略。
type Pool struct {
	// MaxConcurrencyPerAccount 上游实测并发闸门为 1，默认就按 1 排队。
	MaxConcurrencyPerAccount int `json:"max_concurrency_per_account"`
	// QuotaCooldown 命中「每日额度用尽」后的冷却时长（到点自动重试）。
	QuotaCooldown string `json:"quota_cooldown"`
	// ErrorCooldown 普通上游错误的冷却时长。
	ErrorCooldown string `json:"error_cooldown"`
	// MaxRetries 一次请求最多换几个账号。
	MaxRetries int `json:"max_retries"`
	// DailyLimit 本地额度估算：达到后本地提前摘号。0 = 不拦（只靠上游 1200000409 兜底）。
	// 上游「5 次/天」只针对核心模型，flux 等走另一档，写死 5 会误杀。
	DailyLimit int `json:"daily_limit"`
}

// Panel 管理面板。
type Panel struct {
	// Auth 为面板访问口令；留空则只允许本机访问。
	Auth string `json:"auth"`
}

// Logging 请求日志。
type Logging struct {
	Enabled        bool   `json:"enabled"`
	MaxEntries     int    `json:"max_entries"`
	RetentionHours int    `json:"retention_hours"`
}

// ModelSpec 描述一个对外模型如何映射到上游 generator。
type ModelSpec struct {
	// Generator 上游 generator_name（如 vertex/anon-bob）。
	Generator string `json:"generator"`
	// Display 展示名，出现在 /v1/models。
	Display string `json:"display"`
	// Type image / image-modify。
	Type string `json:"type"`
	// ImageField 图生图时输入图数组的字段名：image / image_list。
	ImageField string `json:"image_field"`
	// SingleImage 输入图为单个字符串（recraft / ideogram / seededit / midjourney）。
	SingleImage bool `json:"single_image"`
	// SupportsEdit 是否支持图生图。
	SupportsEdit bool `json:"supports_edit"`
	// Resolutions 可选分辨率枚举（上游 enum），空表示不传。
	Resolutions []string `json:"resolutions,omitempty"`
	// AspectRatios 可选比例枚举，空表示不传。
	AspectRatios []string `json:"aspect_ratios,omitempty"`
	// Extra 固定附加到 input_args 的参数（如 resolution 默认值）。
	Extra map[string]any `json:"extra,omitempty"`
}

// Default 返回最小可用配置。
func Default() *Config {
	return &Config{
		Listen:    ":7864",
		APIKey:    "sk-lovart",
		StateFile: "./data/state.json",
		Upstream: Upstream{
			WWWBase: "https://www.lovart.ai",
			LGWBase: "https://lgw.lovart.ai",
			Lang:    "zh",
		},
		Images: Images{
			DefaultModel:     "nano-banana-pro",
			DefaultEditModel: "nano-banana-pro",
			PollInterval:     "1200ms",
			PollMax:          "5s",
			Timeout:          "15m",
			DownloadResult:   false,
		},
		Pool: Pool{
			MaxConcurrencyPerAccount: 1,
			QuotaCooldown:            "6h",
			ErrorCooldown:            "90s",
			MaxRetries:               3,
		},
		Panel: Panel{},
		Logging: Logging{
			Enabled:        true,
			MaxEntries:     2000,
			RetentionHours: 72,
		},
		Models: defaultModels(),
	}
}

// defaultModels 只放已经实测跑通的映射；其余模型用户可以按 schema 自己加。
func defaultModels() map[string]ModelSpec {
	return map[string]ModelSpec{
		"nano-banana-pro": {
			Generator:    "vertex/anon-bob",
			Display:      "Nano Banana Pro",
			Type:         "image",
			ImageField:   "image",
			SupportsEdit: true,
			Resolutions:  []string{"1K", "2K", "4K"},
			AspectRatios: []string{"21:9", "16:9", "4:3", "3:2", "1:1", "9:16", "3:4", "2:3", "5:4", "4:5"},
			Extra:        map[string]any{"resolution": "1K"},
		},
		"nano-banana": {
			Generator:    "vertex/nano-banana",
			Display:      "Nano Banana",
			Type:         "image",
			ImageField:   "image",
			SupportsEdit: true,
			AspectRatios: []string{"21:9", "16:9", "4:3", "3:2", "1:1", "9:16", "3:4", "2:3", "5:4", "4:5"},
		},
		"gpt-image-1.5": {
			Generator:    "openai/gpt-image-1-5",
			Display:      "GPT Image 1.5",
			Type:         "image",
			ImageField:   "image",
			SupportsEdit: true,
		},
		"gpt-image-1": {
			Generator:    "openai/gpt-image-1",
			Display:      "GPT Image",
			Type:         "image",
			ImageField:   "image",
			SupportsEdit: true,
		},
		"seedream-4.5": {
			Generator:    "seedream/seedream-4-5",
			Display:      "Seedream 4.5",
			Type:         "image",
			ImageField:   "image",
			SupportsEdit: true,
		},
		"seedream-4.0": {
			Generator:    "seedream/seedream-4-0",
			Display:      "Seedream 4",
			Type:         "image",
			ImageField:   "image",
			SupportsEdit: true,
		},
		"flux-2-pro": {
			Generator:    "fal/flux-2-pro",
			Display:      "Flux 2 Pro",
			Type:         "image",
			ImageField:   "image_list",
			SupportsEdit: true,
			Resolutions:  []string{"1K", "2K"},
			AspectRatios: []string{"21:9", "16:9", "4:3", "3:2", "1:1", "2:3", "3:4", "9:16", "9:21"},
		},
		"flux-2-max": {
			Generator:    "fal/flux-2-max",
			Display:      "Flux 2 Max",
			Type:         "image",
			ImageField:   "image_list",
			SupportsEdit: true,
			Resolutions:  []string{"1K", "2K"},
			AspectRatios: []string{"21:9", "16:9", "4:3", "3:2", "1:1", "2:3", "3:4", "9:16", "9:21"},
		},
		"seededit-3.0": {
			Generator:    "seededit/seededit-3-0",
			Display:      "Seededit",
			Type:         "image",
			ImageField:   "image",
			SingleImage:  true,
			SupportsEdit: true,
		},
		"recraft-v3": {
			Generator:    "recraft/recraft-v3",
			Display:      "Recraft V3",
			Type:         "image",
			ImageField:   "image",
			SingleImage:  true,
			SupportsEdit: true,
		},
		"ideogram-v3": {
			Generator:    "ideogram/ideogram-v3",
			Display:      "Ideogram",
			Type:         "image",
			ImageField:   "image",
			SingleImage:  true,
			SupportsEdit: true,
		},
		"midjourney": {
			Generator:    "youchuan/midjourney",
			Display:      "Midjourney",
			Type:         "image",
			ImageField:   "image_url",
			SingleImage:  true,
			SupportsEdit: true,
			AspectRatios: []string{"16:9", "4:3", "1:1", "3:4", "9:16", "2:3", "3:2"},
		},
	}
}

// Load 读配置；文件不存在时返回默认配置（调用方可选择落盘示例）。
func Load(path string) (*Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	// 用 json.Unmarshal 到已有默认值上：用户没写的字段保留默认，map 整体替换。
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("解析 %s: %w", path, err)
	}
	if len(cfg.Models) == 0 {
		cfg.Models = defaultModels()
	}
	cfg.normalize()
	return cfg, nil
}

// normalize 把明显不合法的值掰回默认，避免运行期才炸。
func (c *Config) normalize() {
	if c.Listen == "" {
		c.Listen = ":7864"
	}
	if c.StateFile == "" {
		c.StateFile = "./data/state.json"
	}
	if c.Upstream.WWWBase == "" {
		c.Upstream.WWWBase = "https://www.lovart.ai"
	}
	if c.Upstream.LGWBase == "" {
		c.Upstream.LGWBase = "https://lgw.lovart.ai"
	}
	if c.Upstream.Lang == "" {
		c.Upstream.Lang = "zh"
	}
	if c.Images.DefaultModel == "" {
		c.Images.DefaultModel = "nano-banana-pro"
	}
	if c.Images.DefaultEditModel == "" {
		c.Images.DefaultEditModel = c.Images.DefaultModel
	}
	if c.Pool.MaxConcurrencyPerAccount <= 0 {
		c.Pool.MaxConcurrencyPerAccount = 1
	}
	if c.Pool.MaxRetries <= 0 {
		c.Pool.MaxRetries = 3
	}
	if c.Logging.MaxEntries <= 0 {
		c.Logging.MaxEntries = 2000
	}
}

// Duration 解析时间字段，解析失败回退默认值。
func Duration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// EnsureDir 保证状态文件所在目录存在。
func EnsureDir(stateFile string) error {
	dir := filepath.Dir(stateFile)
	if dir == "" || dir == "." {
		return nil
	}
	return os.MkdirAll(dir, 0o750)
}
