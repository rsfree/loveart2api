<p align="center">
  <h1 align="center">lovart2api</h1>
  <p align="center">
    把 Lovart 的图像生成能力包成 <b>OpenAI 兼容 API</b> 的单二进制网关 · 附 Web 面板<br>
    文生图 / 图生图 · 12 个模型映射 · 多账号池 · 额度感知 · 冷却与轮转 · wasm 客户端签名
  </p>
</p>

<p align="center">
  <img alt="Go" src="https://img.shields.io/badge/Go-1.22+-00ADD8?logo=go&logoColor=white&style=flat-square">
  <img alt="API" src="https://img.shields.io/badge/API-OpenAI_Compatible-412991?style=flat-square">
  <img alt="Deploy" src="https://img.shields.io/badge/Deploy-Single_Binary%20%7C%20Docker-2496ED?style=flat-square">
  <img alt="Sign" src="https://img.shields.io/badge/Signature-WebAssembly%20(wazero)-654FF0?style=flat-square">
</p>

---

> [!WARNING]
> **合规与风险**：Lovart 官方没有对外开放的 API。本项目把网页端内部接口适配成 OpenAI 形态，**仅限自用账号、个人 / 私有环境测试**。上游随时可能改协议、改签名、加风控；因使用非官方客户端导致的账号风控由使用者自行承担。请勿用于批量小号、二次售卖或对外提供付费中转。

## 这是什么

一份把 `www.lovart.ai` + `lgw.lovart.ai` 的图像生成链路翻译成 **OpenAI Images API** 的网关：
客户端（OpenAI SDK、new-api、任何支持 `/v1/images/*` 的工具）不改代码即可用 Lovart 的
Nano Banana Pro、GPT Image、Flux 2、Seedream 等模型做文生图和图生图。

```
你的客户端 ──OpenAI 协议──▶ lovart2api ──签名+多账号──▶ lgw.lovart.ai/v1/generator
                                  └─ kit 上传 ─▶ OSS ─▶ a.lovart.ai
```

## 核心能力

| 能力 | 说明 |
|---|---|
| 🖼 **文生图** | `POST /v1/images/generations`，支持 `size` / `aspect_ratio` / `resolution` / `seed` / `negative_prompt` |
| 🎨 **图生图** | `POST /v1/images/edits`，multipart 与 JSON（base64 / data URL / http URL）双形态；`generations` 里带 `image` 也按图生图走 |
| 🔁 **多图参考** | 一次传多张输入图（`image` 数组 / `image[]` / `image_1..9`） |
| 🧠 **12 个模型映射** | Nano Banana Pro / GPT Image 1.5 / Flux 2 Pro·Max / Seedream 4.5·4 / Seededit / Recraft / Ideogram / Midjourney…映射表可配置 |
| 👥 **账号池** | 按 token 哈希去重、最近最少使用轮转、失败换号重试（默认 3 次） |
| 🚦 **额度感知** | 上游核心模型 5 次/天、并发 1/账号；本地计数 + 命中即冷却到次日 04:00，避免反复撞墙 |
| ❄️ **分级冷却** | 额度 → **只冷却该模型**到日切（核心模型满了 flux 照用）；鉴权 → 整号禁用；并发 → 3s；参数错误 → 不冷却（是请求的问题，不是账号的问题） |
| 🔐 **wasm 签名** | 上游用 WebAssembly 算 `X-Client-Signature`，本项目用 wazero 直接跑同一份 wasm，不复刻算法 |
| 📐 **比例兜底** | 任意 `size` 折算到最近的支持比例（`1792x1024` → `16:9`），避免「任务创建成功但执行失败」白烧额度 |
| 🖥 **Web 面板** | 账号增删改测、额度与冷却、请求日志、在线探测 |
| 📦 **单二进制** | 无 CGO、无外部依赖，wasm 与面板都内嵌 |

## 快速开始

```bash
# 1) 起服务（首次会生成 config.json）
go run ./cmd/server -config config.json
# 或
./lovart2api -config config.json

# 2) 拿账号凭证：浏览器登录 lovart.ai，F12 → Application → Cookies
#    复制 usertoken 的值（不是 localStorage 里 liblibai_userinfo 的 token）
#    然后在面板 http://127.0.0.1:7864/panel 里粘贴添加，或直接调接口：
curl -X POST http://127.0.0.1:7864/panel/api/accounts \
  -H 'Content-Type: application/json' \
  -d '{"token":"<usertoken>","email":"me@example.com"}'

# 3) 文生图
curl http://127.0.0.1:7864/v1/images/generations \
  -H 'Authorization: Bearer sk-lovart' -H 'Content-Type: application/json' \
  -d '{"model":"flux-2-pro","prompt":"a red circle on white background","size":"1024x1024"}'

# 4) 图生图
curl http://127.0.0.1:7864/v1/images/edits \
  -H 'Authorization: Bearer sk-lovart' \
  -F 'model=nano-banana-pro' \
  -F 'prompt=change the circle to blue' \
  -F 'image=@./input.png'
```

OpenAI SDK 直接指过来：

```python
from openai import OpenAI
c = OpenAI(base_url="http://127.0.0.1:7864/v1", api_key="sk-lovart")
img = c.images.generate(model="flux-2-pro", prompt="a green triangle", size="1024x1024")
print(img.data[0].url)
```

## API

| 端点 | 说明 |
|---|---|
| `POST /v1/images/generations` | 文生图；带 `image` 字段时自动走图生图 |
| `POST /v1/images/edits` | 图生图；`multipart/form-data` 或 JSON |
| `GET /v1/models` | 模型清单（额外带 `supports_edit` / `upstream_generator`） |
| `GET /healthz` | 健康检查（含可用账号数） |
| `GET /v1/usage` | 账号池用量 |
| `GET /panel` | Web 面板 |

通用参数：

| 字段 | 说明 |
|---|---|
| `model` | 对外模型名（下表）或上游 `generator_name`（如 `vertex/anon-bob`） |
| `prompt` | 必填 |
| `n` | 保留字段，一次请求返回上游给的所有产物 |
| `size` | `1024x1024` 这类，自动折成最近支持比例 |
| `aspect_ratio` | 直接给比例，优先于 `size` |
| `resolution` | `1K` / `2K` / `4K`，模型不支持时自动降级到最近档 |
| `response_format` | `url`（默认）或 `b64_json` |
| `image` | 图生图输入：字符串 / 数组 / base64 / data URL / http URL |

## 模型映射

`config.json → models` 是唯一真源，默认这 12 个（**只有标 ✅ 的是本仓库实测跑通整条链路的**）：

| 对外模型名 | 上游 generator | 图生图 | 实测 |
|---|---|---|---|
| `nano-banana-pro` | `vertex/anon-bob` | ✅ | ✅ 图生图 / 文生图 |
| `flux-2-pro` | `fal/flux-2-pro` | ✅ | ✅ 图生图 / 文生图 / base64 / 16:9 |
| `flux-2-max` | `fal/flux-2-max` | ✅ | 待测 |
| `seedream-4.5` / `seedream-4.0` | `seedream/seedream-4-5` / `-4-0` | ✅ | 待测 |
| `gpt-image-1.5` / `gpt-image-1` | `openai/gpt-image-1-5` / `-1` | ✅ | 待测 |
| `nano-banana` | `vertex/nano-banana` | ✅ | ⚠️ 上游目录有、网关报 `no API path` |
| `seededit-3.0` | `seededit/seededit-3-0` | 单图 | 待测 |
| `recraft-v3` | `recraft/recraft-v3` | 单图 | 待测 |
| `ideogram-v3` | `ideogram/ideogram-v3` | 单图 | 待测 |
| `midjourney` | `youchuan/midjourney` | 单图 | 待测 |

新增模型：照 `POST /api/canva/agent/v1/generators/schemas` 返回的 OpenAPI 填一条
`generator` / `image_field` / `single_image` / `aspect_ratios` 即可。

## 关键设计

### 签名跑 wasm，不逆算法

lgw 网关对每个请求校验 `X-Send-Timestamp` + `X-Req-Uuid` + `X-Client-Signature`，
签名由一份 41KB 的 Rust wasm（hmac + sha2）计算，输出形如 `1:<64hex>`。
本项目内嵌这份 wasm，用 [wazero](https://wazero.io) 直接调用：

```go
sig, _ := signer.Sign(ctx, timestamp, reqUUID)  // → "1:cf82b7f5…"
```

好处：上游换算法 / 换密钥只需替换 `internal/sign/signature.wasm`，不用重写实现。
`internal/sign/sign_test.go` 用浏览器实测样本锁死了调用约定。

### 上传必须不带 Content-Type

上游签发 OSS 预签名时把 `Content-Type` 留空；OSS 的 `StringToSign` 会带上请求里的
content-type，**多带这个头就 403 SignatureDoesNotMatch**。代码里显式 `Del("Content-Type")`。

### 额度与并发都按账号

上游核心模型 `daily_limit=5`、并发 `running_task_num=1`。池的做法：

- 命中 `1200000409` → **该 generator** 冷却到次日 04:00（`model_cooldown`），账号其余模型照常可用；
- 命中 `1200000200` → 3 秒短冷却换号；
- token 失效 → 账号禁用，等人工处理；
- 参数类失败（`VALIDATION_ERROR`）→ **不冷却账号**，直接 400 返回。

> 实测 `flux` 系列不占核心额度（同一个号在 `nano-banana-pro` 报额度满之后，
> 立刻用 `flux-2-pro` 仍能出图），所以按模型隔离冷却是必须的 —— 否则一个模型满了就整号躺平。

## 部署

```bash
# 单二进制
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o lovart2api ./cmd/server
./lovart2api -config config.json

# Docker
docker build -t lovart2api .
docker run -d --name lovart2api -p 7864:7864 \
  -v $PWD/data:/app/data -v $PWD/config.json:/app/config.json lovart2api
```

### ⚠️ 出口必须在境外

`www.lovart.ai` / `lgw.lovart.ai` / `web-static3.lovart.ai` 对中国大陆出口会 TLS RST
（TCP 能连、握手即断）。服务必须部署在**境外出口**（实测香港节点可用）。
阿里云 OSS 桶国内可直连，上传那一步不受影响。

### 配置要点

| 键 | 说明 |
|---|---|
| `api_key` | 对外 Bearer token |
| `panel.auth` | 面板口令；留空时只允许内网/本机访问 |
| `images.download_result` | `true` 时一律回 `b64_json`（网关代下产物，客户端少一次跨境请求） |
| `pool.max_retries` | 一次请求最多换几个账号 |
| `pool.max_concurrency_per_account` | 上游实测为 1，别改大 |

## 已知限制

- **额度**：核心模型 5 次/天/账号。要更多吞吐只能上多账号（面板批量加）。
- **产物格式**：上游按模型返回 png / jpeg，网关不转码，`b64_json` 走原始字节。
- **`n` 参数**：上游一次只产一张（除 seedream 的 `max_images`），网关不做放大重试。
- **agent 对话链路未接**：Lovart 的「对话式设计」走 `/api/canva/agent/chat`（SSE，
  经 Service Worker），纯 HTTP 复现未取证，本仓库只做生成器直连。
- **浏览器取证**：上游是 Cloudflare + Service Worker 结构，抓包需真人 Chrome；
  逆向过程见 `docs/upstream/lovart-api.md`。

## 目录

```
cmd/server        入口（配置 / 池 / HTTP）
cmd/probe         上游诊断工具（验签、探模型、试上传、跑一次生成）
internal/sign     wasm 签名（wazero）+ 一致性测试
internal/lovart   上游客户端（鉴权 / 签名 / 上传 / 任务）
internal/pool     账号池（轮转 / 冷却 / 额度 / 持久化）
internal/server   OpenAI 兼容端点 + 内嵌面板
docs/upstream     上游 API 取证文档
```

## 免责声明

本项目按「原样」提供，不带任何担保。它是**非官方**适配层，与 Lovart / LiblibAI 无关联。
使用者需自行确保用途符合目标平台服务条款与当地法律；作者不对账号封禁、额度损失或
其他后果负责。**请勿购买任何收费版本** —— 本仓库始终免费开源。
