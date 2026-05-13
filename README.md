# Code Switch

> 用 Web 管理 Claude Code、Codex、Gemini CLI、自定义 CLI 和 GPT 生图供应商。

Code Switch 是一个适合部署在 Linux 服务器上的 Web 管理台 + API relay。浏览器负责配置供应商、代理开关、日志和 MCP；后台进程负责把 CLI/API 请求转发到真实上游，并按优先级、模型映射、健康状态和黑名单策略选择 provider。

## 功能概览

- 管理 Claude Code / Codex / Gemini CLI / 自定义 CLI provider
- 独立管理 `GPT生图` provider，支持 OpenAI Images API 兼容中转
- 支持 provider 优先级、模型白名单、模型映射和自动降级
- 支持 Codex relay key，前端不暴露上游 API Key
- 支持请求日志、Token/成本统计、健康检查和黑名单
- 支持 MCP、提示词、CLI 配置管理
- Web 模式运行，不依赖桌面窗口或系统托盘

## 运行端口和数据

默认启动两个监听地址：

| 组件 | 默认地址 | 用途 |
|------|----------|------|
| Web 管理台 | `0.0.0.0:8080` | 浏览器访问后台 |
| Provider Relay | `0.0.0.0:18100` | CLI/API 请求中转 |

数据默认写入 `~/.code-switch/`，包括：

- `app.db`
- `app.json`
- `claude-code.json`
- `codex.json`
- `gpt-image.json`
- `codex-relay-keys.json`
- `mcp.json`
- `prompts.json`
- `proxy-state/`

更新程序时不要删除或覆盖 `~/.code-switch/`。

## 推荐部署方式

生产环境推荐：

```text
公网 HTTPS
  -> Caddy / Nginx
  -> 127.0.0.1:8080 codeswitch-web

CLI / API 客户端
  -> 127.0.0.1:18100 或内网/SSH 隧道
  -> Provider Relay
```

不要直接把 `8080` 裸露到公网。管理台内置管理员登录、setup token、登录限流、同源校验和安全响应头，但公网访问仍建议走 HTTPS 反向代理。

`18100` 是 relay 端口，已经支持 Codex relay key 校验；如果要暴露公网，仍建议配合防火墙、来源限制或网关。

### 构建

要求：

- Node.js 20+
- Go 1.26+
- Linux amd64 部署建议在同架构机器构建

```bash
cd frontend
npm install
npm run build

cd ..
go build -o codeswitch-web .
```

启动：

```bash
./codeswitch-web
```

健康检查：

```bash
curl http://127.0.0.1:8080/healthz
```

### systemd 示例

创建 `/etc/systemd/system/codeswitch.service`：

```ini
[Unit]
Description=Code Switch Web Service
After=network.target

[Service]
Type=simple
User=your-user
WorkingDirectory=/opt/codeswitch/current
Environment=CODE_SWITCH_WEB_ADDR=127.0.0.1:8080
Environment=CODE_SWITCH_SETUP_TOKEN=replace-with-a-long-random-token
ExecStart=/opt/codeswitch/current/codeswitch-web
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
```

启动：

```bash
sudo systemctl daemon-reload
sudo systemctl enable --now codeswitch
sudo journalctl -u codeswitch -f
```

### 反向代理

仓库内提供示例配置：

- Nginx: `deploy/nginx/code-switch.conf`
- Caddy: `deploy/caddy/Caddyfile`

如果反向代理和服务不在同一网络边界，可能需要设置：

```bash
export CODE_SWITCH_TRUSTED_PROXIES='127.0.0.1/32,::1/128,172.18.0.0/16'
export CODE_SWITCH_PUBLIC_ORIGIN='https://admin.example.com'
```

静态文件目录可通过 `CODE_SWITCH_STATIC_DIR` 覆盖：

```bash
export CODE_SWITCH_STATIC_DIR=/opt/codeswitch/current/frontend/dist
```

没有域名时，推荐用 SSH 隧道或内网组网：

```bash
ssh -L 8080:127.0.0.1:8080 -L 18100:127.0.0.1:18100 your-user@your-server
```

## 使用流程

1. 打开 `http://127.0.0.1:8080` 或反代后的 HTTPS 域名。
2. 首次进入时创建管理员账号；公网初始化需要填写启动日志里的 setup token。
3. 在对应 Tab 添加 provider，填写 `API URL`、`API Key`、模型白名单/映射等配置。
4. Claude Code / Codex / Gemini / 自定义 CLI 需要打开对应“托管”开关，让本机 CLI 配置指向 relay。
5. `GPT生图` 不需要托管开关。图片 API 请求天然由 `/v1/images/*` 路由进入 `GPT生图` provider 池。
6. 切换代理或修改 CLI 配置后，建议重启相关 CLI 进程。
7. 在日志页面确认请求、Token、成本和 provider 切换情况。

## Provider 配置要点

`API URL` 填上游 base URL，例如：

```text
https://api.example.com
```

不要把 endpoint 拼进 `API URL`。路径放到 `API Endpoint`。

常见 endpoint：

| 场景 | API Endpoint |
|------|--------------|
| Claude Anthropic Messages | `/v1/messages` |
| Claude -> OpenAI Responses 兼容 | `/v1/responses` |
| Codex Responses | `/responses` |
| OpenAI Chat Completions | `/v1/chat/completions` |
| GPT 生图 | `/v1/images/generations` |

模型白名单为空时通常表示不限制模型；如果配置了白名单或映射，就会按请求模型筛选 provider。

如果外部请求模型和上游真实模型不同，可以配置模型映射：

```text
gpt-image-2 -> upstream-image-model
claude-* -> gpt-5.4
```

## GPT 生图

`GPT生图` 是独立 provider 池。所有 OpenAI Images API 兼容请求都会走这里：

- `POST /v1/images/generations`
- `POST /v1/images/edits`
- `OPTIONS /v1/images/generations`
- `OPTIONS /v1/images/edits`

配置建议：

- `API URL`: 上游 base URL
- `API Key`: 上游 key
- `API Endpoint`: `/v1/images/generations`
- 认证方式：`Bearer`
- 模型白名单/映射可按需填写

普通 base64 返回：

```bash
curl http://127.0.0.1:18100/v1/images/generations \
  -H "Authorization: Bearer csk_xxx" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "a red apple",
    "size": "1024x1024",
    "response_format": "b64_json"
  }'
```

SSE 流式返回需要请求显式开启：

```bash
curl -N --http1.1 http://127.0.0.1:18100/v1/images/generations \
  -H "Authorization: Bearer csk_xxx" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d '{
    "model": "gpt-image-2",
    "prompt": "生成一张极简红色圆点图",
    "size": "1024x1024",
    "stream": true,
    "partial_images": 1
  }'
```

relay 会原样透传上游 SSE，例如：

```text
event: image_generation.partial_image
data: {"type":"image_generation.partial_image","partial_image_index":0,"b64_json":"..."}
```

如果经过 Nginx，确保流式路径关闭代理缓冲，否则前端可能收不到即时事件。

## Codex Relay Key

Codex/OpenAI 兼容 API 调用需要使用后台生成的 `csk_...` relay key。

在 Web 后台进入 `设置` -> `安全设置` -> `生成 key`。

支持的认证头：

```text
Authorization: Bearer csk_xxx
X-Code-Switch-Key: csk_xxx
X-API-Key: csk_xxx
```

示例：

```bash
curl http://127.0.0.1:18100/responses \
  -H "Authorization: Bearer csk_xxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"gpt-5-codex","input":"hello"}'
```

## 更新部署

建议本地构建、打包，再上传服务器解压：

```bash
npm run build --prefix frontend
GOOS=linux GOARCH=amd64 CGO_ENABLED=1 go build -tags production -trimpath -buildvcs=false -ldflags="-w -s" -o codeswitch-web .
tar -czf codeswitch-deploy-linux-amd64.tar.gz codeswitch-web frontend/dist
```

服务器更新时只替换程序目录，不动 `~/.code-switch/`：

```bash
sudo systemctl stop codeswitch
cd /opt/codeswitch/current
sudo tar -xzf /tmp/codeswitch-deploy-linux-amd64.tar.gz
sudo systemctl start codeswitch
```

更稳的方式是使用版本目录和 `current` 软链，便于回滚。

## 常见问题

### 浏览器关掉后 relay 还在吗？

还在。浏览器只是管理界面，真正服务是 `codeswitch-web` 进程。

### 页面提示找不到前端资源

先构建前端：

```bash
cd frontend
npm install
npm run build
```

后端默认读取 `frontend/dist/index.html`。

### Claude/Codex/Gemini 没有走代理

按顺序检查：

1. 对应平台托管开关是否打开。
2. CLI 是否已重启。
3. `codeswitch-web` 是否运行。
4. `127.0.0.1:18100` 是否可访问。
5. provider 的 URL、Key、模型映射是否正确。

### GPT 生图提示没有可用 provider

检查 `GPT生图` Tab：

1. provider 是否启用。
2. `API URL` 和 `API Key` 是否非空。
3. `API Endpoint` 是否填了 `/v1/images/generations`。
4. 如果配置了白名单/映射，请确认请求模型能匹配。

### 如何备份配置？

备份整个目录：

```bash
~/.code-switch/
```

## 开发

常用命令：

```bash
npm run build --prefix frontend
go test ./...
go build ./...
```

本地运行：

```bash
npm run build --prefix frontend
go run .
```

## 技术栈

| 层级 | 技术 |
|------|------|
| 后端 | Go + Gin + SQLite |
| 前端 | Vue 3 + TypeScript + Vite |
| 通信 | HTTP RPC + SSE |
| 数据目录 | `~/.code-switch/` |

## License

MIT License

问题反馈：<https://github.com/Rogers-F/code-switch-R/issues>
