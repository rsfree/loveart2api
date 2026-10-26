<p align="center">
  <h1 align="center">lovart2api</h1>
  <p align="center">
    把 Lovart 的图像生成能力包成 <b>OpenAI 兼容 API</b> 的单二进制网关 · 附 Web 面板<br>
    文生图 / 图生图 · 13 个模型映射 · 多账号池 · 额度感知 · 冷却与轮转 · wasm 客户端签名
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
| 🧠 **13 个模型映射** | Nano Banana Pro / GPT Image 2·1.5 / Flux 2 Pro·Max / Seedream 4.5·4 / Seededit / Recraft / Ideogram / Midjourney…映射表可配置 |
| 👥 **账号池** | 按 token 哈希去重、最近最少使用轮转、失败换号重试（默认 3 次） |
| 🚦 **额度感知** | 上游核心模型 5 次/天、并发 1/账号；本地计数 + 命中即冷却到次日 04:00，避免反复撞墙 |
| ❄️ **分级冷却** | 额度 → **只冷却该模型**到日切（核心模型满了 flux 照用）；鉴权 → 整号禁用；并发 → 3s；参数错误 → 不冷却（是请求的问题，不是账号的问题） |
| 🔐 **wasm 签名** | 上游用 WebAssembly 算 `X-Client-Signature`，本项目用 wazero 直接跑同一份 wasm，不复刻算法 |
| 📐 **比例兜底** | 任意 `size` 折算到最近的支持比例（`1792x1024` → `16:9`），避免「任务创建成功但执行失败」白烧额度 |
| 🖥 **Web 面板** | 账号增删改测、**批量导入**、积分余额、额度与冷却、请求日志 |
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
| `POST /panel/api/accounts/bulk` | 批量导入 usertoken（多行 / 逗号 / 数组）；支持 `accessToken::refreshToken` 形式 |

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

`config.json → models` 是唯一真源，默认这 13 个（**只有标 ✅ 的是本仓库实测跑通整条链路的**）：

| 对外模型名 | 上游 generator | 图生图 | 实测 |
|---|---|---|---|
| `nano-banana-pro` | `vertex/anon-bob` | ✅ | ✅ 图生图 / 文生图 |
| `flux-2-pro` | `fal/flux-2-pro` | ✅ | ✅ 图生图 / 文生图 / base64 / 16:9 |
| `flux-2-max` | `fal/flux-2-max` | ✅ | 待测 |
| `seedream-4.5` / `seedream-4.0` | `seedream/seedream-4-5` / `-4-0` | ✅ | 待测 |
| `gpt-image-2` | `openai/gpt-image-2` | ✅ | ⚠️ 上游目录/schema 未列，但网关映射已接受（灰度模型），属核心额度池 |
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
>
> **核心额度是三类模型共用一个池**：`NB`（Nano Banana）、`GPT_IMAGE`（GPT Image 系列）、
> `SEEDREAM`（Seedream 系列）合计 **5 次/天**，任一跑满其余全部不可用（三者返回的
> `current_generated_count` 同时为 5 可证）。重置点是**次日 08:00 Asia/Shanghai**。
>
> 而且这 5 次**不是免费的**——每做一次照样扣积分（实测香蕉文生图 24 分、图生图 7 分），
> 积分再充裕也突破不了这个上限。**不受限**的模型：flux-2-pro/max、recraft-v3、
> ideogram-v3、midjourney（只受积分约束）。
>
> 代码里给这些模型打了 `core: true`，跑满后整个核心池进 `core_cooldown`，
> 后续核心请求在本地直接返回 429，不再白打上游。

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

### 用 GHCR 镜像部署（推荐）

CI 在 push 到 main 时自动发版：打 `vX.Y.Z` tag、建 Release、推三 tag 镜像
（`<version>` / `latest` / `sha-<short>`，三 tag digest 一致）。发版前会先在 CI 里
跑容器冒烟（`/healthz` 200 + 无 key 请求 401），不过关不会推。

```bash
docker pull ghcr.io/rsfree/loveart2api:v0.0.1
docker run -d --name lovart2api \
  -p 127.0.0.1:7864:7864 \
  -v $PWD/data:/app/data \
  -v $PWD/config.json:/app/config.json:ro \
  ghcr.io/rsfree/loveart2api:v0.0.1
```

两个必踩的坑：

- 端口**绑回环**，公网入口统一走 nginx + 域名 —— 这是"能烧账号额度的生成出口"。
- 容器内是 uid `10001`：挂载的 `config.json` 与 `data/` 必须 `chown 10001:10001`，
  否则启动直接 `permission denied`。

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

## Token 保活（自动续期）

`accessToken` 只有 **7 天**有效期，`refreshToken` 约 90 天，且**每次刷新都会轮换**。
网关内置续期，不需要手工重登：

- 后台每 6 小时扫一遍，剩余不足 2 天才真正刷新（刷太勤没必要，每次刷新都消耗一次轮换机会）
- 刷新成功会把新 accessToken + 新 refreshToken 一起写回状态文件
- 面板可手动刷：每行「续期」按钮 / 顶部「全部续期」；API 同 `POST /panel/api/accounts/{id}/refresh`
- 🔴 **手动刷新有 1 小时冷却**：上游 WAF 对 refresh 做频控，实测 30 分钟内连刷 3 次就被
  `405 Sorry, your request has been blocked`。冷却期内请求直接返回 `refresh_too_soon`
- 账号按 **userId**（JWT 里的 `userId`）收敛，不会因为 token 轮换而"越刷越多号"

🔴 要续期**必须有 refreshToken**。只有 accessToken 的号 7 天后必然失效，只能重新登录。
批量导入时用 `accessToken::refreshToken` 格式带上，否则面板会标 `无RT`。

```bash
# 带 RT 导入
curl -X POST http://127.0.0.1:7864/panel/api/accounts/bulk \
  -H 'Content-Type: application/json' \
  -d '{"tokens":"<accessToken>::<refreshToken>"}'
```

## 扩容：多账号

单账号核心额度是 5 次/天（全球免费档政策，不随出口 IP 变化——实测美国出口与香港出口返回完全一致的
`region=CN / location=HK`）。所以产能只能靠账号数堆。

账号来源：网页端用**真实邮箱**注册（`mail.xiuvi.cn` 这类临时邮箱域名已被上游拉黑，
返回 `This email provider is not allowed`；且发码前有阿里云验证码挑战，纯 HTTP 过不去）。

拿到 `usertoken` 后两条路导入：

```bash
# 单个
curl -X POST http://127.0.0.1:7864/panel/api/accounts \
  -H 'Content-Type: application/json' -d '{"token":"<usertoken>"}'

# 批量（面板上也有输入框；支持换行/逗号/JSON 数组）
curl -X POST http://127.0.0.1:7864/panel/api/accounts/bulk \
  -H 'Content-Type: application/json' \
  -d '{"tokens":"tok1\ntok2\ntok3"}'
```

批量导入会并发 4 路逐个校验（拉模型目录验证 token 有效），并顺带探测每个号的 `project_id`，
返回逐条结果。重复 token 幂等（`created=false`），失效 token 单独报错不阻塞其他。

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
