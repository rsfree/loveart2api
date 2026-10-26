# Lovart 上游 API（图生图链路）

> 取证时间：2026-10-26，账号 uid `2928587`，站点 `https://www.lovart.ai`（Cloudflare 后）。
> 所有结论均来自真实请求，未经推测的条目会显式标注「未验证」。

## 0. 一分钟速览

```
上传：kit/create → kit/asset/presign → PUT OSS(不带 Content-Type) → kit/asset/confirm → oss_url
生成：POST lgw/v1/generator/tasks {generator_name, input_args{prompt,image[]}, project_id} → generator_task_id
轮询：GET  lgw/v1/generator/tasks?task_id=… → status + artifacts[].content
```

鉴权：`token: <cookie usertoken>`；网关额外要求 `X-Send-Timestamp` / `X-Req-Uuid` / `X-Client-Signature`。

## 1. 域名与网关分工

| 域名 | 角色 | 说明 |
|---|---|---|
| `www.lovart.ai/api/canva/**` | 业务后端（Java，Spring 风格错误体） | 项目、会话、素材、生成器目录、模型 schema |
| `www.lovart.ai/api/www/**` | 用户/计费/登录 | 会员、积分、订单 |
| `lgw.lovart.ai/v1/**` | **Kong 网关 → 生成/素材服务** | 真正的生成与上传都在这里 |
| `a.lovart.ai/artifacts/**` | 产物 CDN | 生成的图片/视频 |
| `web-static3.lovart.ai` | 静态资源 + `signature_wasm` | 被墙/被 RST，见 §7 |

> 前端代码里 `lgwHost` 指向 `lgw.lovart.ai`，客户端区分 `lovart-canva` / `lovart-www` / `lovart-lgw` 三类 base。

## 2. 鉴权

- 请求头 `token`，值等于 **cookie `usertoken`**（实测 407 字符；`document.cookie` 可读，非 HttpOnly）。
- 不是 `Authorization: Bearer`。`localStorage.liblibai_userinfo.token`（764 字符）是 LiblibAI 侧凭证，**不能**直接当 `token` 头用。
- 401 判定：`{"code":401,"msg":"未登录"}`（www 域）／`{"code":"INVALID_TOKEN"}`（lgw 域）。
- `lgw.lovart.ai` 还要求 `accept-language`（zh / en），缺失不影响鉴权但影响返回文案。

## 3. `X-Client-Signature`（wasm 签名）

lgw 网关对**所有**请求校验三个头，缺一个就 401 `MISSING_HEADER`：

```
X-Send-Timestamp: <毫秒时间戳字符串>        # String(Date.now())
X-Req-Uuid:       <uuid v4 去掉横线>        # 32 hex
X-Client-Signature: <签名>                  # 形如 "1:<64 hex>"
```

签名实现在 `signature_wasm_bg.*.wasm`（41KB，Rust + `hmac-0.12.1` + `sha2-0.10.9`，**无宿主 import**）：

```
导出：memory / gs / ve / get_api_secret / get_config_secret /
      __wbindgen_add_to_stack_pointer / __wbindgen_export{,2,3}
调用：gs(stackPtr, tsPtr, tsLen, uuidPtr, uuidLen, aPtr, aLen, bPtr, bLen)
      → 结果写回 stackPtr 处的 (ptr,len)；JSON 调用永远是 gs(ts, uuid, "", "")
```

- `get_api_secret()` / `get_config_secret()` 按 wasm-bindgen 约定接收 retptr，返回
  `GoLovart_…` / `Pr0duct10n_K3y_v2_…`。算法本身未复刻（HMAC 输入拼接没对上），**本项目直接跑 wasm**（`internal/sign`，wazero），换算法只需换 wasm。
- 签名结果对 `(ts, uuid)` 是纯函数，**不绑定 URL、不绑定 body** —— 同一签名可用于任意 lgw 端点。
- wasm 地址（会随构建变）：`https://web-static3.lovart.ai/lovart_prd/static/_next/static/media/signature_wasm_bg.<hash>.wasm`

## 4. 图片上传（本地文件 → 公网 URL）

三段式 + OSS 直传。所有 `lgw` 请求都要带 §3 的三个头。

```
1) POST /v1/kit/create        {"name":"x"}                      → {"id":"kit_xxxxxxxx"}
2) POST /v1/kit/asset/presign {"kit_id","asset_type":"image","file_name":"a.png"}
   → {"upload_url","oss_key","callback_token","expires_at","max_size"}
3) PUT  <upload_url>  <bytes>            ← 不带 Content-Type！见下
4) POST /v1/kit/asset/confirm {"callback_token"}
   → {"asset":{"files":[{"oss_url":"https://a.lovart.ai/context-forge/kits/…/x.png"}]}}
```

### 4.1 🔴 PUT 必须不带 `Content-Type`

OSS 的 `StringToSign` 是 `PUT\n\n<content-type>\n<expires>\n/<bucket>/<key>`。
上游签发时把 content-type 留空 ⇒ 客户端**只要带上任何 Content-Type 就会 `403 SignatureDoesNotMatch`**。

实测矩阵：

| 请求形态 | 结果 |
|---|---|
| `PUT` + `Content-Type: image/png` | 403 SignatureDoesNotMatch |
| `PUT` + 无 Content-Type（raw / 解码 `%2F` 都一样） | **200** |

> `upload_url` 的 path 里 `%2F` 编码成 `context-forge%2Fkits%2F…`，解码与否都能 200 —— 不是变量。

### 4.2 参数细节

- `asset_type`：`image` / `video` / …；`file_name` 必填且扩展名要在白名单（`.png` 可以）。
- `filename`、`name`、`file_extension` 这些**都不是**字段名，写错报 `INVALID_FILE_EXTENSION`。
- `max_size` 实测 50MB。
- confirm 返回的 `oss_url` 才是喂给生成接口的地址。

## 5. 生成接口

### 5.1 创建任务

```
POST https://lgw.lovart.ai/v1/generator/tasks
{
  "generator_name": "vertex/anon-bob",
  "input_args": {
    "prompt": "make the circle blue",
    "image": ["https://a.lovart.ai/context-forge/kits/kit_xxx/image/xxx.png"],
    "aspect_ratio": "1:1",
    "resolution": "1K"
  },
  "project_id": "2u02lf599xq8"
}
→ {"code":0,"message":"success","data":{"generator_task_id":"ef38354c-…"}}
```

必填三件套：`generator_name` / `input_args` / `project_id`（缺一个 422，错误消息会点名）。

- `project_id` 是 Lovart 画布项目 id（任意自己账号下的项目都行，产物会落进该项目）。
- `input_args.image` 是 **URL 数组**（不是 asset id、不是 base64）。
- 外部 URL 会被拒：`{"code":2010}`（实测传 wikimedia 图直接 2010）。

### 5.2 查询任务

```
GET https://lgw.lovart.ai/v1/generator/tasks?task_id=<generator_task_id>
→ {"code":0,"data":{
     "generator_task_id":"…","generator_name":"vertex/anon-bob",
     "status":"completed",
     "original_input_args":{…},
     "artifacts":[{"type":"image","content":"https://a.lovart.ai/artifacts/generator/xxx.png",
                   "metadata":{"artifact_id":"…","format":"png","width":1024,"height":1024}}]
   }}
```

🔴 `task_id` 是 **query 参数**：放 body 会报 `required query parameter is missing (query.task_id)`。

### 5.3 已实测成功的调用

| 场景 | 请求要点 | 结果 |
|---|---|---|
| 文生图 | `vertex/anon-bob` + `{prompt, aspect_ratio}` | ✅ `1:1` 1024×1024 |
| 图生图（自家产物当输入） | `image:["https://a.lovart.ai/artifacts/generator/….png"]` | ✅ 换色成功 |
| 图生图（kit 上传图当输入） | `image:["https://a.lovart.ai/context-forge/kits/…"]` | ✅ 创建成功（当天额度已满，见 §6） |

## 6. 限制与错误码

| 现象 | code | 含义 / 处置 |
|---|---|---|
| 并发超限 | `1200000200` | `task rejected`，`running_task_num` 显示在跑的数量；上游并发 = 1 |
| 每日额度 | `1200000409` | 核心模型 `daily_limit=5`；返回体带 `lovart_core_limit_*` 明细 |
| 输入图不可用 | `2010` | 外链/失效 URL |
| 未知 generator | 500 + `no API path found for generator=…, capability=…` | 名字不在网关映射表里（**不扣额度**，可用来探测） |
| 缺签名头 | 401 `MISSING_HEADER` | 依次点名 `X-Req-Uuid` / `X-Send-Timestamp` / `X-Client-Signature` |

> 并发闸门和每日额度都**按账号**计。单账号做图生图 ≈ 5 张/天（核心模型档位），
> 所以本项目按 workbuddy2api 的路子做**多账号池**。

## 7. 网络前提

`www.lovart.ai` / `web-static3.lovart.ai` / `lgw.lovart.ai` 在**中国大陆出口会被 TLS RST**
（TCP 能连、握手即断，直连与本地 http 代理均如此，`colo=LAX`）。因此：

- 服务端必须部署在**境外出口**（或挂境外代理）。
- 浏览器（真人 Chrome，境外出口）可正常访问，逆向因此走浏览器通道取证。
- 阿里云 OSS 桶 `models-online-persist-us.oss-us-east-1.aliyuncs.com` 国内可直连 —— 上传这一步不受影响。

## 8. 模型目录（image 类）

来源：`POST /api/canva/agent/v1/generators/list`（**这个接口在 www 域，只吃 `token` 头，不要签名头**）。

| generator_name | 展示名 | 图生图输入字段 | 备注 |
|---|---|---|---|
| `vertex/anon-bob` | Nano Banana Pro | `image[]` | **已实测跑通**，1K/2K/4K |
| `vertex/nano-banana` | Nano Banana | `image[]` | ⚠️ 网关报 `no API path`（目录里有、网关没有） |
| `openai/gpt-image-1-5` | GPT Image 1.5 | `image[]` | 未逐一验证 |
| `openai/gpt-image-1` | GPT Image | `image[]` | 名字通过网关校验（撞并发），未跑完整任务 |
| `fal/flux-2-pro` / `fal/flux-2-max` | Flux 2 Pro/Max | `image_list[]` | 同上 |
| `seedream/seedream-4-5` / `-4-0` | Seedream 4.5 / 4 | `image[]` | 同上 |
| `seededit/seededit-3-0` | Seededit | `image`（单张） | 纯编辑模型 |
| `recraft/recraft-v3` | Recraft V3 | `image`（单张）+`strength` | |
| `ideogram/ideogram-v3` | Ideogram | `image`（单张）+ `mask` | 多 mode |
| `youchuan/midjourney` | Midjourney | `image_url`（单张） | |

完整入参（含 enum、默认值、x-capabilities.modes）见
`POST /api/canva/agent/v1/generators/schemas`（空 body 即返回整份 OpenAPI，43 个 path、109 个 schema）。

> 未验证的模型在网关侧可能同样报 `no API path`。适配器把「模型 → generator_name + 字段名」
> 做成可配置映射表，探测到不可用时自动回退到默认模型。

## 9. 其他有用端点（www 域，只带 `token`）

| 端点 | 用途 |
|---|---|
| `POST /api/canva/agent/agentThreadList` | 会话列表（`projectId` / `text` / `updateTimestamp`） |
| `POST /api/canva/project/projectList` | 项目列表（含 `projectCoverList` 产物缩略图） |
| `POST /api/canva/agent/v1/generators/list` | 模型目录 |
| `POST /api/canva/agent/v1/generators/schemas` | 整份 OpenAPI（参数真源） |
| `GET  /api/canva/agent/v1/generators/taskInfo?task_id=` | ⚠️ 实测恒返回 `500 服务异常2`，不要用 |
| `POST /api/canva/agent/chat` | agent 对话（SSE，走 Service Worker，纯 HTTP 复现未取证） |
