# grokcli-2api

## 本次更新

- 补充 xAI 代理配置、连通性测试和邮箱注册会话管理接口。
- 优化管理台交互，以及 OpenAI / Anthropic 流式请求在客户端断开时的处理。
- 完善 Anthropic Messages 兼容转换和相关依赖配置。

> **状态说明：注册机暂不可以使用。** 邮箱辅助注册（MoeMail 协议）当前暂不可用，请使用设备码登录或导入已有 `auth.json`；现有账号管理和 API 服务不受影响。

把 **Grok OIDC 登录态** 转成 **OpenAI / Anthropic 兼容 API**，并附带 Web 管理台：多 API Key、**多账号轮询**、设备码 / 导入授权。

**独立运行**：不依赖本地 Grok CLI，不调用 `grok login` / 浏览器 OAuth。凭证保存在项目 `data/` 目录。

**v1.6**：**Anthropic Messages API**（`POST /v1/messages` · 流式 SSE · tools / tool_use · `count_tokens` 估算）。

**v1.5**：完整 **OpenAI Tools / Function Calling**（`tools` / `tool_choice` / `tool_calls` 往返、流式与非流式、`role: tool` 回传）。

**v1.4**：原生 OIDC 设备码、按 user_id 多账号池、`round_robin` 默认轮询、失败自动切换、后台 Token 刷新、auth.json 文件锁。

适合 Cherry Studio、NextChat、OpenAI SDK、Anthropic SDK、Claude Code、Cursor 自定义模型等工具接入。

## 原理

```
客户端 (OpenAI / Anthropic SDK · GUI)
        │  POST /v1/chat/completions   (OpenAI)
        │  POST /v1/messages           (Anthropic)
        │  Authorization: Bearer …  或  x-api-key: …
        ▼
  grokcli-2api  (FastAPI)
        │  管理台 /admin
        │  Anthropic ↔ OpenAI 协议转换
        │  读取 data/auth.json（多账号池）
        │  按策略轮询 + 失败自动切换
        │  附加 X-XAI-Token-Auth / x-grok-client-*
        ▼
  cli-chat-proxy.grok.com
```

直接使用 CLI 同款代理协议，不依赖本地 `grok` 可执行文件。

## 功能

| 功能 | 说明 |
|------|------|
| OpenAI 兼容 | `GET /v1/models` · `POST /v1/chat/completions` · 流式 SSE |
| **Anthropic 兼容** | `POST /v1/messages` · 流式 event SSE · tools/tool_use · `count_tokens` |
| **Tools / 函数调用** | OpenAI `tool_calls` · Anthropic `tool_use` / `tool_result` 往返 |
| 管理台 | `http://127.0.0.1:3000/admin` 账号 / Key / 轮询 / 接入指南 |
| 多 API Key | 创建、停用、删除；哈希存储，明文只显示一次 |
| **多账号轮询** | `round_robin` / `least_used` / `random`（账号对等，无主账号） |
| **对话粘性** | 同一会话固定账号，切号/轮询不中断多轮记忆；失败才 failover |
| **失败切换** | 上游 401/429/5xx 冷却该账号，自动换下一个 |
| **额度查询** | 管理台「查询额度」→ 上游 `/v1/billing`；耗尽自动移出轮询 |
| **Token 自动续期** | 后台维护线程按剩余时间自适应刷新，更新 `expires_at` |
| **单账号模型探测** | 管理台「账号」页逐个测试；后台定时探测报错并自动移出轮询 |
| **授权** | 设备码登录（管理台显示 user_code）· 导入 auth.json / JWT |
| 鉴权策略 | 无 Key 时开发模式开放；有 Key 后自动要求鉴权（`Authorization` 或 `x-api-key`） |

## 前置条件

1. Python 3.10+
2. 可访问 `cli-chat-proxy.grok.com` 与 `auth.x.ai`（设备码 / 刷新 Token）

## 安装与启动

### Windows

```powershell
cd $env:USERPROFILE\Desktop\grokcli-2api
pip install -r requirements.txt
python app.py
# 或
.\start.ps1
```

### Linux 服务器

```bash
cd /opt/grokcli-2api   # 或你的部署目录
python3 -m pip install -r requirements.txt

export GROK2API_HOST=0.0.0.0
export GROK2API_PORT=3000
export GROK2API_OPEN_BROWSER=0
export GROK2API_ADMIN_PASSWORD='your-strong-password'
# 多账号轮询（默认即 round_robin，可不设）
export GROK2API_ACCOUNT_MODE=round_robin

chmod +x start.sh
./start.sh
# 或后台
nohup ./start.sh > grok2api.log 2>&1 &

# 或 systemd
# sudo useradd -r -s /usr/sbin/nologin grok2api
# sudo mkdir -p /opt/grokcli-2api /var/lib/grok2api/data
# sudo cp -r . /opt/grokcli-2api/
# sudo cp deploy/grok2api.service /etc/systemd/system/
# sudo systemctl daemon-reload && sudo systemctl enable --now grok2api
```

默认监听：`http://127.0.0.1:3000`

| 地址 | 说明 |
|------|------|
| http://127.0.0.1:3000/admin | Web 管理台 |
| http://127.0.0.1:3000/docs | Swagger |
| http://127.0.0.1:3000/health | 健康检查 |
| http://127.0.0.1:3000/v1 | OpenAI Base URL |
| http://127.0.0.1:3000/v1/messages | Anthropic Messages API |
| http://127.0.0.1:3000 | Anthropic SDK `base_url`（根地址） |

### Anthropic 接入示例

```bash
# curl
curl http://127.0.0.1:3000/v1/messages \
  -H "x-api-key: sk-g2a-YOUR_KEY" \
  -H "anthropic-version: 2023-06-01" \
  -H "Content-Type: application/json" \
  -d '{"model":"grok-4.5","max_tokens":1024,"messages":[{"role":"user","content":"你好"}]}'
```

```python
from anthropic import Anthropic
client = Anthropic(base_url="http://127.0.0.1:3000", api_key="sk-g2a-YOUR_KEY")
msg = client.messages.create(
    model="grok-4.5",  # 或 claude-sonnet-4 等别名
    max_tokens=1024,
    messages=[{"role": "user", "content": "Hello"}],
)
print(msg.content[0].text)
```

`claude-*` 模型名会自动映射到默认 Grok 模型。流式、system、tools / tool_use / tool_result 均已支持。

## 如何授权

**不依赖本地 Grok CLI**，两种方式：

### 方式 A：设备码登录（推荐）

1. 打开管理台 → **账号 / 轮询**
2. 点 **设备码登录**
3. 页面显示 **user_code** 与验证链接
4. 用手机/本机浏览器打开链接，输入设备码
5. 完成后点刷新，账号会出现在列表中

### 方式 B：导入 JWT / auth.json

管理台 → **导入 Token / auth.json**：

- 粘贴完整 `auth.json` 内容（可勾选「与现有账号合并」实现多账号）
- 或只粘贴 `eyJ...` 访问令牌（会解析 exp / sub）

## 多账号轮询

| 模式 | 行为 |
|------|------|
| `round_robin` | 按顺序轮流使用（默认，推荐） |
| `least_used` | 优先请求次数更少的账号 |
| `random` | 随机（可按 weight） |

> 所有账号对等，**没有主账号**。额度耗尽（billing 或上游错误）时自动禁用该账号，不再参与轮询；可在管理台手动「启用」重新加入。

- 每个账号可单独 **启用 / 禁用**
- 失败后进入冷却（401 约 5 分钟，429/5xx 约 1 分钟），自动跳过
- 可在管理台切换策略，或设环境变量 `GROK2API_ACCOUNT_MODE`

多次设备码登录或合并导入多个 token 即可组成账号池。

## 快速上手

1. 打开管理台 → 设置管理员密码  
2. **账号** 页：设备码登录 / 导入 auth.json  
3. 选择轮询策略（默认 round_robin）  
4. **API Keys** 页创建 `sk-g2a-...`  
5. 客户端：Base URL + Key + 模型 `grok-4.5`

### curl

```bash
curl http://127.0.0.1:3000/v1/models -H "Authorization: Bearer sk-g2a-YOUR_KEY"

curl http://127.0.0.1:3000/v1/chat/completions \
  -H "Authorization: Bearer sk-g2a-YOUR_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"grok-4.5","messages":[{"role":"user","content":"你好"}],"stream":false}'
```

### OpenAI Python SDK

```python
from openai import OpenAI

client = OpenAI(
    base_url="http://127.0.0.1:3000/v1",
    api_key="sk-g2a-YOUR_KEY",
)
r = client.chat.completions.create(
    model="grok-4.5",
    messages=[{"role": "user", "content": "Hello"}],
)
print(r.choices[0].message.content)
```

### Tools / Function Calling

兼容 OpenAI Chat Completions 工具协议（Cherry Studio、OpenAI SDK、Cursor 等可直接用）：

```python
import json
from openai import OpenAI

client = OpenAI(base_url="http://127.0.0.1:3000/v1", api_key="sk-g2a-YOUR_KEY")

tools = [
    {
        "type": "function",
        "function": {
            "name": "get_weather",
            "description": "Get weather for a city",
            "parameters": {
                "type": "object",
                "properties": {
                    "city": {"type": "string"},
                },
                "required": ["city"],
            },
        },
    }
]

messages = [{"role": "user", "content": "北京天气怎么样？"}]
r = client.chat.completions.create(
    model="grok-4.5",
    messages=messages,
    tools=tools,
    tool_choice="auto",
)

msg = r.choices[0].message
if msg.tool_calls:
    messages.append(msg)  # 含 tool_calls 的 assistant 消息
    for tc in msg.tool_calls:
        args = json.loads(tc.function.arguments)
        # 在本地执行工具
        result = {"city": args["city"], "temp_c": 26, "condition": "晴"}
        messages.append(
            {
                "role": "tool",
                "tool_call_id": tc.id,
                "content": json.dumps(result, ensure_ascii=False),
            }
        )
    r2 = client.chat.completions.create(
        model="grok-4.5",
        messages=messages,
        tools=tools,
    )
    print(r2.choices[0].message.content)
else:
    print(msg.content)
```

也支持流式：`stream=True` 时 `delta.tool_calls` 会按 OpenAI 分片格式下发，`finish_reason` 为 `tool_calls`。

扁平工具定义（`name` / `parameters` 在顶层）会自动规范为 OpenAI 嵌套 `function` 结构后再转上游。

## 环境变量

| 变量 | 默认 | 说明 |
|------|------|------|
| `GROK2API_HOST` | `127.0.0.1` | 监听地址（服务器用 `0.0.0.0`） |
| `GROK2API_PORT` | `3000` | 端口 |
| `GROK2API_API_KEY` | 空 | 遗留单 Key |
| `GROK2API_ADMIN_PASSWORD` | 空 | 管理台密码 |
| `GROK2API_ACCOUNT_MODE` | 空（UI 默认 round_robin） | 轮询策略 |
| `GROK2API_CONVERSATION_AFFINITY` | `1` | 对话粘性：同会话固定账号（防记忆中断） |
| `GROK2API_AFFINITY_TTL` | `7200` | 粘性绑定过期（秒） |
| `GROK2API_AFFINITY_MAX` | `5000` | 最多保留的会话绑定数 |
| `GROK2API_REQUIRE_API_KEY` | `auto` | `auto` / `1` / `0` |
| `GROK2API_DEFAULT_MODEL` | `grok-4.5` | 默认模型 |
| `GROK2API_AUTH_FILE` | `./data/auth.json` | 凭证路径 |
| `GROK_CLI_CHAT_PROXY_BASE_URL` | `https://cli-chat-proxy.grok.com/v1` | 上游 |
| `GROK2API_TIMEOUT` | `600` | 超时（秒） |
| `GROK2API_FORCE_STREAM` | `1` | 上游强制 stream |
| `GROK2API_DATA_DIR` | `./data` | keys / settings / auth |
| `GROK2API_OPEN_BROWSER` | Linux 无头默认 `0` | 是否自动开浏览器 |
| `GROK2API_CLI_VERSION` | `0.2.93` | 客户端版本头 |
| `GROK2API_TOKEN_MAINTAIN` | `1` | 后台自动刷新 Token |
| `GROK2API_TOKEN_MAINTAIN_INTERVAL` | `300` | 维护周期（秒） |
| `GROK2API_TOKEN_REFRESH_SKEW` | `120` | 过期前多少秒刷新 |
| `GROK2API_MODEL_HEALTH` | `1` | 后台模型探测开关 |
| `GROK2API_MODEL_HEALTH_INTERVAL` | `600` | 探测周期（秒），`0` 仅手动 |
| `GROK2API_MODEL_HEALTH_AUTO_DISABLE` | `1` | 探测失败时自动屏蔽模型/禁用账号 |
| `GROK2API_PROBE_MODELS` | 默认模型 | 定期探测的模型列表（逗号分隔） |

## 管理 API 摘要

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/admin/api/status` | 公开状态 |
| POST | `/admin/api/setup` | 首次设密码 |
| POST | `/admin/api/login` | 登录拿 token |
| GET | `/admin/api/dashboard` | 仪表盘 |
| CRUD | `/admin/api/keys` | API Key |
| GET/POST/DELETE | `/admin/api/accounts*` | 账号 |
| POST | `/admin/api/accounts/login` | 设备码登录（原生 OIDC） |
| GET | `/admin/api/accounts/login/sessions/{id}` | 设备码会话状态 |
| POST | `/admin/api/accounts/import` | 导入 JSON 体（API） |
| POST | `/admin/api/accounts/import-file` | 上传 JSON 文件导入 |
| GET | `/admin/api/accounts/export` | 导出 auth.json（含 token） |
| PATCH | `/admin/api/accounts/{id}/enabled` | 启用/禁用池账号 |
| POST | `/admin/api/accounts/{id}/probe` | 单账号模型探测 |
| POST | `/admin/api/accounts/probe-all` | 全部账号模型探测 |
| GET | `/admin/api/model-health` | 后台探测状态 |
| GET | `/admin/api/accounts/quota` | 查询全部账号额度（耗尽自动移出轮询） |
| GET | `/admin/api/accounts/{id}/quota` | 查询单账号额度 |
| PUT | `/admin/api/settings/account-mode` | 轮询策略 |

管理请求头：`X-Admin-Token: <token>`。

## 常见问题

### 401 / auth_error

- **客户端 401**：API Key 错误  
- **上游 auth_error**：会话过期，重新设备码登录或导入新 Token  

### 多账号不轮询

确认账号未被「禁用 / 额度禁用」；token 未过期。管理台点「查询额度」可刷新 billing 状态；额度恢复后手动「启用」即可重新加入轮询。

### token 有效期

Session token 会过期（约数小时到数天）。有 `refresh_token` 时后台会自动续期；否则重新设备码登录/导入即可。

## 安全提示

- 默认只绑 `127.0.0.1`；公网务必设管理密码 + API Key，并配合防火墙 / reverse proxy  
- `data/auth.json` 与 `data/keys.json` 含敏感信息，勿分享  
- 管理台 token 存在浏览器 localStorage  

## 目录结构

```
grokcli-2api/
  app.py              # 主服务 + 多账号 failover
  admin_routes.py     # 管理 API
  auth.py             # 读取凭证
  auth_store.py       # auth.json 安全读写（文件锁）
  accounts.py         # 设备码 / 导入（无本地 CLI）
  account_pool.py     # 轮询 / 冷却 / 统计
  oidc_auth.py        # 原生 OIDC 设备码 + refresh
  token_maintainer.py # 后台多账号 Token 维护
  apikeys.py          # 客户端 Key
  settings_store.py   # 管理密码与偏好
  models.py           # 模型列表
  config.py           # 配置
  static/index.html   # 管理台 UI
  start.sh            # Linux 启动
  deploy/grok2api.service  # systemd 单元
  start.ps1 / start.bat
  data/               # 运行时数据（auth / keys / settings）
```

## 协议与免责

仅供个人学习与自用。请遵守 xAI / Grok 服务条款与用量限制。
