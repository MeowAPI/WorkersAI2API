# WorkersAI2API

将 Cloudflare Workers AI 接入常见 API 客户端。客户端使用标准 Chat Completions、Responses、Messages、Images、Audio、Embeddings 接口，模型名统一使用 `kimi-k2.7-code`、`flux-1-schnell`、`aura-1` 等短名称。无需客户端理解 Cloudflare 的 `/run/` 请求或响应格式。

Go 1.25+，无第三方 Go 依赖。Cloudflare `/ai/run/...` 仅作为服务内部上游，不对外提供该路由。

## 启动

```bash
cp .env.example .env
# 编辑 .env，设置 AUTH_TOKEN
go run .
```

程序自动读取**当前工作目录**的 `.env`，无需 `source` 或 `export`；系统环境变量优先（包括显式空值）。支持注释、单双引号、`export KEY=value` 和 CRLF，不执行 shell 命令或变量展开。修改 `.env` 后重启进程。

打开 http://localhost:8080/admin/ ，使用 `AUTH_TOKEN` 登录，在页面添加一个或多个 Cloudflare 账号即可调用 API。根路径 `/` 也会跳转至管理页面。管理页面和所有静态资源嵌入 Go 二进制，不需要 Node.js、CDN 或前端构建。

| 环境变量 | 用途 | 默认值 |
| --- | --- | --- |
| `AUTH_TOKEN` | 客户端 API 密钥，同时用于管理页面登录；持有者有账号管理权限，必填 | — |
| `BIND_ADDRESS` / `PORT` | 监听地址 / 端口 | `0.0.0.0` / `8080` |

也可通过 Docker 启动：

```bash
docker compose up -d --build
```

发布 GitHub Release 后，[Docker 发布工作流](.github/workflows/docker-release.yml) 自动构建 `linux/amd64` 和 `linux/arm64` 镜像并推送到 `ghcr.io/meowapi/workersai2api`。镜像标签保留 Release 的 Git tag（例如 `v1.0.0`）；正式版同时更新 `latest`，预发布版只发布版本标签。工作流使用内置 `GITHUB_TOKEN`，无需额外配置 registry secret。首次发布后，如需匿名拉取，请将 GitHub Packages 中该镜像的可见性设置为 Public。

```bash
docker pull ghcr.io/meowapi/workersai2api:latest
```

Compose 使用命名卷 `workersai-data` 保存账号和请求日志；重建容器会保留这些数据。原生运行时账号保存在当前工作目录的 `data/accounts.json`，文件权限为 `0600`，写入使用原子替换；目录已加入 Git / Docker 忽略规则。备份该文件即可备份账号，文件包含上游密钥。

账号统一通过管理页面添加、修改、停用或删除。`CLOUDFLARE_ACCOUNT_ID` 和 `CLOUDFLARE_API_TOKEN` 已移除，旧环境或 `.env` 中即使保留它们也不会再读取或导入；已有 `data/accounts.json` 完整保留。没有账号时可以正常启动管理页，推理请求返回 `503`。

模型目录在启动后后台自动同步，之后**每小时**检查一次；页面也可手动刷新。请求时限固定为 10 分钟。原有 `REQUEST_TIMEOUT`、`AUTO_MODELS`、`MODEL_REFRESH_INTERVAL`、`MODELS`、`MODEL_ALIASES`、`PUBLIC_BASE_URL` 均已移除，旧文件中的这些配置会被忽略。

图片 URL 根据请求地址自动生成；HTTPS 反向代理应覆盖并传递 `X-Forwarded-Proto`、`X-Forwarded-Host`，无需配置公开地址。`GET /healthz` 用于进程健康检查，不代表账号配额可用。

## 多账号和管理页面

- **账号管理**：新增、修改、停用、启用、删除，保存后立即生效。修改账号时 Token 留空保留原值。页面只显示密钥末尾提示，不回传完整密钥。最多 100 个账号，每个 Account ID 一条。
- **轮询**：所有推理入口共用账号池，按请求依次选择启用且未冷却的账号；同一请求的多张图片使用同一账号。正在进行的请求保留原账号，不受编辑影响。
- **冷却**：上游 `429` 按 `Retry-After` 冷却（缺失时 60 秒，最多 24 小时），`401/403` 冷却 60 秒，网络异常与 `5xx` 冷却 15 秒。后续请求跳过这些账号；全部不可用时返回 `503`，有冷却账号时附 `Retry-After`。不会自动重放已发送的生成请求。
- **账号运行状态**：显示下一个账号、上游请求数、失败数、最近 HTTP 状态和冷却时间。账号页计数以实际上游调用为单位，一次多图请求可能计多次；这些运行计数在进程重启后归零，统计概览和请求日志的数据则会保留。新增账号校验输入格式，实际权限和配额以调用结果为准。
- **模型目录**：短名称搜索、任务筛选、完整上游 ID、可用标准端点和同步结果。启动即使用内置快照，网络失败保留上一份目录。
- **管理登录**：使用 HttpOnly、SameSite=Strict 的 24 小时会话 Cookie，写操作另做 CSRF 校验；HTTPS 下 Cookie 标记 Secure。退出或进程重启使会话失效。客户端推理仍使用 API Key，不接受管理 Cookie。

## 请求日志、统计和主题

管理页新增 **统计概览** 与 **请求日志**。推理请求完成后记录，成功、参数错误、鉴权失败、上游失败、客户端取消与流式中断均包含在内；模型列表查询、健康检查、图片下载和管理接口不计入推理统计。处理中请求单独显示数量。每个请求有唯一 ID，可与响应 `X-Request-ID` 对应。

日志详情包含调用时间、模型短名称与实际上游模型、所选账号、HTTP 状态、结果、请求总耗时、首字节耗时、输入输出字节数、错误原因，以及以下内容：

- 客户端请求与最终响应正文；流式调用保留已捕获的 SSE 内容，不会为了记录日志等待完整响应后才转发。
- 转换后的上游请求、上游响应、请求头与响应头；一次多图生成会列出每次上游调用。
- 上游报告的输入/输出/缓存/推理 Token。上游未提供 Token 字段时显示“未报告”，不估算、不伪造用量。一次请求内多个上游调用的已报告用量相加，SSE 累计用量不重复计数。

`Authorization`、Cookie、API Key、Token 等凭据字段及已知代理/账号密钥脱敏。日志保存对话、提示词和生成内容，仅管理会话可查看或导出。图片/音频二进制、Base64 媒体以类型、大小和上传文件摘要记录，不复制媒体本体；每份正文最多捕获 **8 MiB**，超出时明确标记截断；无法完整解析的 JSON 正文/事件使用占位说明，避免把截断的凭据或媒体原文写入日志。未被读取的无效请求体不额外读取。首字节耗时指代理开始输出响应正文的时间，不等同于模型首 Token 时间。

日志保存于 `data/logs/*.json`，权限 `0600`，采用原子替换。默认保留最近 **7 天**，最多 **10,000 条 / 256 MiB**，达到任一上限就清理最早记录。统计从已保留日志计算，重启可恢复；已淘汰记录不再计入。异常断电前尚未完成的请求不会有完成日志。磁盘写入失败不阻断推理响应，但管理页面会显示存储异常。

支持按最近 24 小时/7 天/全部保留记录、模型、账号、结果筛选；日志还支持请求 ID、模型、账号名称、路径和错误文本搜索、分页、单条 JSON 下载与筛选结果 NDJSON 导出。统计提供成功率、平均/P95 耗时、首字节时间、Token 用量及报告比例、请求趋势、按模型和账号的分组汇总。HTTP 200 的中断流仍标为失败/中断结果。

主题可选择 **浅色 / 深色 / 跟随系统**，登录页和管理页都可切换；浏览器只持久化主题偏好，管理凭据不存入 localStorage。

## 接口

OpenAI SDK 的 `baseURL` 填 `http://localhost:8080/v1`，`apiKey` 填 `AUTH_TOKEN`。Anthropic/Gemini 客户端使用服务根地址。支持 `Authorization: Bearer` 和 `x-api-key`；Gemini 还支持 `x-goog-api-key` / `?key=`。

| 路径 | 客户端协议及用途 |
| --- | --- |
| `POST /v1/chat/completions` | OpenAI Chat Completions，支持 SSE |
| `POST /v1/responses` | OpenAI Responses，支持 SSE、函数调用 |
| `POST /v1/messages` | Anthropic Messages，支持 SSE、函数调用 |
| `POST /v1beta/models/{model}:generateContent` | Gemini GenerateContent |
| `POST /v1beta/models/{model}:streamGenerateContent?alt=sse` | Gemini SSE |
| `POST /v1/embeddings` | OpenAI Embeddings |
| `POST /v1/images/generations` | OpenAI Images：文字生成图片 |
| `POST /v1/images/edits` | OpenAI Images：multipart 图片编辑 |
| `POST /v1/audio/speech` | OpenAI Audio：文本转语音，返回音频文件 |
| `POST /v1/audio/transcriptions` | OpenAI Audio：multipart 文件转写 |
| `POST /v1/audio/translations` | OpenAI Audio：语音翻译为英语，使用 Whisper Turbo |
| `POST /v1/rerank` | 常见 Rerank 格式：`query`、`documents`、`top_n` |
| `GET /v1/models`、`GET /v1/models/{model}` | 短名称、任务类型和 `supported_endpoints` |
| `GET /v1beta/models`、`GET /v1beta/models/{model}` | Gemini 格式模型目录 |
| `GET /v1/files/{id}/content` | 图片 URL 的临时文件内容 |

## SDK 示例

```python
from openai import OpenAI
import base64
import os

client = OpenAI(
    base_url="http://localhost:8080/v1",
    api_key=os.environ["AUTH_TOKEN"],
)

reply = client.chat.completions.create(
    model="kimi-k2.7-code",
    messages=[{"role": "user", "content": "你好"}],
)
print(reply.choices[0].message.content)

vectors = client.embeddings.create(
    model="bge-small-en-v1.5", input=["你好"],
)
print(len(vectors.data[0].embedding))

picture = client.images.generate(
    model="flux-1-schnell", prompt="A blue square on a white background",
    size="1024x1024", response_format="b64_json",
)
with open("picture.jpg", "wb") as f:
    f.write(base64.b64decode(picture.data[0].b64_json))

speech = client.audio.speech.create(
    model="aura-1", input="Hello world.", voice="alloy", response_format="wav",
)
with open("speech.wav", "wb") as f:
    f.write(speech.content)

with open("speech.wav", "rb") as f:
    transcript = client.audio.transcriptions.create(
        model="whisper-large-v3-turbo", file=f, response_format="json",
    )
print(transcript.text)

with open("picture.jpg", "rb") as f:
    edited = client.images.edit(
        model="flux-2-klein-4b", image=f, prompt="Make the square red",
        size="512x512", response_format="b64_json",
    )
```

重排序：

```bash
curl http://localhost:8080/v1/rerank \
  -H "Authorization: Bearer $AUTH_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"bge-reranker-base","query":"What color is the sky?","documents":["The sky is blue.","Cats like fish."],"top_n":1}'
```

## 模型目录和能力

从 [Cloudflare 官方目录](https://developers.cloudflare.com/workers-ai/models/) 拉取全部模型及任务类型，启动后后台刷新一次，之后每小时刷新。完整 ID 的末段自动成为公开名称：`kimi-k2.7-code` → `@cf/moonshotai/kimi-k2.7-code`。目前的内置快照包含 65 个模型。

刷新失败保留上一份有效目录；启动失败使用 2026-09-30 快照。同名冲突会使本次刷新失败，避免误路由。完整 ID 也可直接使用。

`tasks` 是官网原始分类，`supported_endpoints` 表示已实现的兼容入口。目录收录不等于任意模型能调用任意端点，也不保证账户已接受模型许可。

- **图片**：所有目录图片生成模型使用标准 Images 响应；FLUX.2 使用 multipart 上游，其他模型使用各自 JSON 格式。支持 `n=1..10`、`size`、`b64_json` 和 `url`。FLUX.1 Schnell 仅支持固定 1024×1024；inpainting 模型要求使用 edits 并提供 image、mask。编辑支持 FLUX.2 和 Stable Diffusion 系列。`output_format` 支持 PNG/JPEG；透明背景、专属 quality/style 等无法保真的参数明确报错。
- **图片 URL**：由服务缓存并提供可下载的 HTTP URL，最长 15 分钟；缓存总量 64 MiB，容量不足会淘汰旧图片。URL 含不可猜测随机 ID，持有链接即可读取。需要长期保存时下载文件或使用 `b64_json`。
- **语音合成**：Aura 系列支持 MP3/WAV/PCM/FLAC/Opus/AAC，返回实际音频；标准 voice 名称映射到对应 Aura speaker，也可直接使用上游 speaker 名称。MeloTTS 使用默认音色，支持 MP3。当前只支持 `speed=1`，不支持声线克隆或 instructions。
- **语音转写**：Whisper、Whisper Tiny、Whisper Turbo、Nova 3 接受标准 `file` 上传。支持 JSON、纯文本、verbose JSON；SRT/VTT 要求模型返回真实时间戳。Whisper Turbo 支持 language、prompt 和音频翻译。未实现转写 SSE、说话人分离格式或 WebSocket 实时协议。
- **重排序**：BGE Reranker 支持字符串/`{"text":...}` 文档，返回按分数降序排列的 `index`、`relevance_score` 和可选 `document`，保留输入索引。
- **专用文本/视觉任务**：翻译模型、DistilBERT、ResNet、LLaVA 通过 Chat Completions 的单条 user 消息调用，回复为标准 assistant 内容；不支持会话历史、工具调用。分类返回 JSON 文本。翻译可通过 `extra_body={"source_lang":"english","target_lang":"french"}` 指定语言，默认翻译成英语。ResNet/LLaVA 接受标准 `image_url` 内容块中的 Base64 data URL。模型一次性完成后再输出 SSE 内容，不声称上游逐 token 生成。
- **尚无兼容入口的模型**：Flux 实时语音、Smart Turn 以及尚未适配的 Moondream，其 `supported_endpoints` 为空；不会伪装成可用的聊天模型。
- **聊天协议**：保留工具、图片及 reasoning_content 的转换。Responses 无状态，需要完整 input；不支持 previous_response_id、后台任务、内置搜索/执行工具。store/include 作为兼容提示忽略。推理预算只能近似映射，不生成供应商签名。

媒体请求体上限 64 MiB，聊天输入 16 MiB；图片/结构化媒体响应上限 64 MiB。请求取消会取消上游，写入空闲超时 30 秒。错误统一成客户端兼容格式，保留状态码及 Retry-After；不自动重试付费生成请求。

## 验证

```bash
go test -race ./...
go vet ./...
go build -trimpath -o bin/workersai2api .
```

测试覆盖`.env` 加载与环境覆盖、多账号并发轮询/冷却/持久化、管理鉴权/CSRF/增删改、短名称路由、目录同步、聊天转换、Images JSON/multipart/URL、Speech 二进制/Base64、转写格式、Rerank 索引排序、参数校验及旧 `/ai/run/...` 端点返回 404。SDK 端到端验证使用标准 OpenAI JavaScript SDK 调用本地代理。

本次管理功能验证还包括：桌面/手机浏览器登录与账号增删改、停用/启用、模型筛选和真实目录刷新；延迟状态响应不恢复已退出的会话；容器以非 root 用户运行、保存账号，以及删除重建容器后从命名卷恢复账号。多账号轮询与冷却使用模拟上游检查账号 ID、Token 配对和请求顺序。

日志功能另验证了请求/上游内容脱敏、SSE 跨分片和截断后的用量统计、首块流式及时输出、媒体摘要、过滤/分页/导出权限、磁盘失败不影响推理、日志轮转与重启恢复。
