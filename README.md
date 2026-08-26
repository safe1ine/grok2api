# grok2api

把 Grok / X 会员订阅额度转成标准 API 的中转站。通过 **xAI 官方 OAuth**（授权码 + PKCE）登录会员账号，对外暴露 **OpenAI** 和 **Anthropic** 两种兼容格式，并带一个 **daisyUI 管理台**。

## 架构

```
客户端 (OpenAI SDK / Anthropic SDK / 各类 agent)
   │  base_url + 你的 API Key (sk-grok2api-...)
   ▼
grok2api 服务 (Go, :30081)
   ├─ /api/open/openai/v1/*      → 透传 api.x.ai（chat/responses/images/audio）
   ├─ /api/open/anthropic/v1/*   → 透传 api.x.ai（messages）
   ├─ /models                    → 聚合所有账号可用模型
   └─ /api/*                     → 管理 API（OAuth 登录/账号/密钥/记录）
   │
   │ Bearer <该账号 access_token>（自动刷新、多账号轮换、429/401 降级）
   ▼
https://api.x.ai/v1
```

## 目录

```
server/   Go 后端（chi + pgx + 账号池 + 透传网关）
web/      React + daisyUI 管理台（仪表盘/账号/密钥/调用记录）
```

## 快速开始

### 1. 准备 PostgreSQL

```bash
docker run -d --name grok2api-pg \
  -e POSTGRES_USER=grok2api -e POSTGRES_PASSWORD=grok2api -e POSTGRES_DB=grok2api \
  -p 5432:5432 postgres:16
```

### 2. 注册 xAI OAuth 应用

1. 打开 https://console.x.ai → 创建 OAuth 应用（或 API Keys → OAuth）
2. 记下 **Client ID**（公开客户端 + PKCE 可不需要 secret）
3. 把回调地址登记为：`http://localhost:30081/api/oauth/callback`
4. 需要的 scope：`openid profile email offline_access grok-cli:access api:access`

### 3. 配置并启动后端

```bash
cd server
cp .env.example .env
# 编辑 .env：填 DATABASE_URL、XAI_CLIENT_ID、ADMIN_PASSWORD
go run ./cmd/grok2api
```

### 4. 启动前端（debug）

```bash
cd web
npm install
npm run dev        # http://localhost:30080，/api 自动代理到 30081
```

打开 http://localhost:30080，用 `.env` 里的 `ADMIN_USERNAME` / `ADMIN_PASSWORD` 登录。

## 登录会员账号（OAuth）

1. 管理台「账号管理」→「添加账号」
2. 弹出 x.ai 登录链接，点击打开并完成授权
3. 浏览器会跳转到 `http://localhost:30081/api/oauth/callback?code=...&state=...`（远端浏览器打不开是正常的）
4. 把地址栏里的**完整 URL** 复制粘贴回管理台 → 「完成登录」
5. 服务端完成 code 换 token，账号加入账号池

> 如果浏览器就在服务器本机，localhost 能打开，`/api/oauth/callback` 会直接显示「登录成功」自动完成。

## 对外 API 用法

在管理台「密钥管理」创建 Key（`sk-grok2api-...`），然后：

**OpenAI SDK：**

```python
from openai import OpenAI
client = OpenAI(
    api_key="sk-grok2api-...",
    base_url="http://<你的服务器>:30081/api/open/openai/v1",
)
client.chat.completions.create(model="grok-4.6", messages=[{"role":"user","content":"hi"}])
```

**Anthropic SDK：**

```python
import anthropic
client = anthropic.Anthropic(
    api_key="sk-grok2api-...",
    base_url="http://<你的服务器>:30081/api/open/anthropic",
)
client.messages.create(model="grok-4.6", max_tokens=1024, messages=[{"role":"user","content":"hi"}])
```

**模型列表：** `GET /api/open/openai/v1/models`

`base_url` 末尾的 `/v1` 可选，两种形式均兼容：

- OpenAI：`/api/open/openai` 或 `/api/open/openai/v1`
- Anthropic：`/api/open/anthropic` 或 `/api/open/anthropic/v1`

服务端会在转发到 xAI 前统一将路径规范为 `/v1/*`。

支持能力：文本对话（chat/responses/messages）、图片理解、生图（images/generations、images/edits）、语音（TTS `/audio/speech`、STT `/audio/transcriptions`）。

## 环境变量（server/.env）

| 变量 | 说明 |
|---|---|
| `PORT` | 服务端口，默认 30081 |
| `DATABASE_URL` | PostgreSQL 连接串 |
| `XAI_CLIENT_ID` | xAI OAuth 应用 Client ID |
| `XAI_CLIENT_SECRET` | 可选，公开客户端 + PKCE 可不填 |
| `XAI_REDIRECT_URI` | 默认 `http://localhost:30081/api/oauth/callback` |
| `XAI_CHAT_PROXY_BASE` | Grok CLI 订阅信息接口，默认 `https://cli-chat-proxy.grok.com/v1` |
| `XAI_GROK_WEB_BASE` | Grok 重置机会 grpc-web 接口，默认 `https://grok.com` |
| `ADMIN_USERNAME` / `ADMIN_PASSWORD` | 管理台登录 |
| `ENCRYPTION_KEY` | refresh_token 落库加密密钥（留空则用 ADMIN_PASSWORD 派生，重启需保持一致） |
| `REQUIRE_AUTH` | 是否要求下游带 Key，默认 true |
| `WEB_DIST` | 可选，指向 `web/dist` 由后端托管前端 |

## 实现要点

- **账号池**：上千账号常驻内存，优先调度未冷却且当前并发最少的账号；同一账号可并发，上游 429 自动冷却换号，401 刷新 token 重试，刷新失败标记 `need_relogin`。
- **Token 管理**：refresh_token 用 AES-GCM 加密落库，access_token 内存缓存，到期自动刷新。
- **订阅周用量**：通过 Grok CLI billing/settings endpoint 获取会员等级、周用量百分比和重置时间，启动时及每 5 分钟刷新并缓存。
- **周限重置**：通过 Grok 主站 `GetRemainingResets` 查询可用重置机会；账号列表可在二次确认后调用 `RedeemReset`，三天内过期的机会会高亮提示。券 ID 仅保存在服务内存中。
- **透传**：流式 SSE 原样转发；TTS/STT 做字段映射（OpenAI 格式 ↔ xAI 格式）。
- **Tool 兼容**：自动清理 `required: null`，将函数参数根级 nullable/非 object 联合类型收窄为 object，并双向转换 OpenAI Responses namespace function 工具。
- **多 Key**：下游 Key 哈希存储，支持 `Authorization: Bearer` 和 `x-api-key`。
- **调用记录分区**：PostgreSQL 按 Asia/Shanghai 自然日写入 `call_logs_YYYYMMDD`，应用自动预建未来 7 天分区；记录输入/缓存命中/输出 Token、首字延迟、总耗时及流式标记。
- **分钟级仪表盘**：按 Asia/Shanghai 分钟聚合调用次数、输入 Token 和估算费用；缓存输入、输出及超过 20 万 Token 的长上下文分别按 xAI 官方模型费率计价。

## 当前阶段范围

- ✅ 已实现：OAuth 登录、账号管理、账号池、透传（文本/生图/图片理解/语音）、API Key、调用记录、分钟级使用量与价格仪表盘
- ⏳ 未实现：调用记录筛选、Key 限额、告警、HTTPS
