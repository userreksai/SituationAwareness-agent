# SituationAwareness Agent

态势感知拨测节点。Agent 默认监听 `8002`，接收 Master 下发的结构化任务，执行 DNS、TCP、HTTP 探测、TLS 证书读取或网页标题获取后返回结果。所有任务共用原有的 8002 端口和 `/api/v1/tasks` 接口。

Agent **不会执行任意 Shell 命令**。这可以避免公网节点因参数拼接或接口泄漏变成远程命令执行入口。

## 快速启动

```bash
cp .env.example .env
export AGENT_NAME=zy-ctyun.cn-beijing-boce01
export AGENT_SHARED_TOKEN='与 Master 相同的长随机字符串'
go run ./cmd/agent
```

健康检查：

```bash
curl http://127.0.0.1:8002/healthz
```

执行测试任务：

```bash
curl -X POST http://127.0.0.1:8002/api/v1/tasks \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer replace-with-a-long-random-token' \
  -d '{
    "taskId": "manual-001",
    "type": "probe",
    "target": "https://example.com",
    "options": {"timeoutMs": 10000, "ports": [80, 443]}
  }'
```

读取域名证书：

```bash
curl -X POST http://127.0.0.1:8002/api/v1/tasks \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer replace-with-a-long-random-token' \
  -d '{
    "taskId": "certificate-001",
    "type": "certificate",
    "target": "example.com",
    "options": {"timeoutMs": 15000}
  }'
```

获取网页标题：

```bash
curl -X POST http://127.0.0.1:8002/api/v1/tasks \
  -H 'Content-Type: application/json' \
  -H 'Authorization: Bearer replace-with-a-long-random-token' \
  -d '{
    "taskId": "title-001",
    "type": "title",
    "target": "https://seo.chinaz.com/",
    "options": {"timeoutMs": 15000}
  }'
```

## 爱站页面任务（可选）

新增 `type=seo`，用于 SEO 主控无法获得有效爱站页面时，由指定节点读取页面。证书和标题任务行为不变；只升级 Agent 不会自动启用 SEO 主控转发。

```json
{"taskId":"seo-check-001","type":"seo","target":"itgirls.cn","options":{"timeoutMs":25000}}
```

使用原有 `POST /api/v1/tasks` 和 Bearer Token。必须设置 `AGENT_SHARED_TOKEN`；`target` 只接受域名。唯一上游为 `https://www.aizhan.com/cha/{domain}/`，不跟随重定向、不接受任意 URL 或端口，不执行 shell 命令。

结果位于 `result.seo`：`url`、`statusCode`、`contentType`、`body`（原始 HTML 的 base64）、`checkedAt`、`error`、可选 `retryAt`。`result.available=false` 时不可当作有效页面，HTTP 200 空正文也会失败。主控必须继续校验页面域名和关键权重；Agent 不解析权重、不写数据库。

每进程仅一个 SEO 请求，请求完成后至少间隔 10 秒；失败冷却 15 分钟，连续失败翻倍至最多 1 小时，遵守更长的 `Retry-After`。正文最多 3 MiB。调用过频或忙时返回不可用结果及 `retryAt`，不能通过多主控/并发请求绕过限制。SEO 任务同样受 `AGENT_MAX_CONCURRENT` 和 `AGENT_MAX_TIMEOUT` 限制。

健康检查的 `taskTypes` 包含 `seo` 表示支持新版任务。源码部署沿用 `go build` 和已有 systemd 安装路径；Docker 部署需要重新构建/发布并更新镜像，Git 合并本身不会替换已运行容器，也不会自动更新 Docker Hub 镜像。

## 配置参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `AGENT_LISTEN_ADDR` | `:8002` | 监听地址；所有 Agent 统一使用 8002 |
| `AGENT_NAME` | 主机名 | 返回给 Master 的节点名称，建议与注册中心 `agentName` 一致 |
| `AGENT_SHARED_TOKEN` | 空 | Master/Agent 共享令牌；生产环境必须设置 |
| `AGENT_MAX_CONCURRENT` | `8` | 单节点同时执行的最大任务数 |
| `AGENT_DEFAULT_TIMEOUT` | `10s` | 未指定任务超时时使用的默认值 |
| `AGENT_MAX_TIMEOUT` | `60s` | 单次任务允许的最大超时；须不小于 Master 的 `TITLE_AGENT_TIMEOUT` |
| `AGENT_TITLE_MAX_RESPONSE_BYTES` | `2097152` | 标题任务允许读取的最大网页字节数，最大 10 MiB |

未设置共享令牌时 Agent 为便于初次联调会启动，但日志会输出安全警告。正式部署时应在安全组中仅允许 Master IP 访问 8002，并设置共享令牌。

## API 契约

- `GET /healthz`：进程健康状态。
- `POST /api/v1/tasks`：执行任务。支持 `type=probe`、`type=certificate` 和 `type=title`。
- 请求体最大 64 KiB；端口最多 10 个；任务超时不能超过 `AGENT_MAX_TIMEOUT`。
- 合法任务即使目标不可达也返回 HTTP 200，并通过 `result.available=false` 和各步骤的 `error` 描述探测结果。参数错误、未授权或节点繁忙分别返回 400、401、429。
- `certificate` 任务默认读取目标的 443 端口，也可通过 `options.ports` 指定一个测试端口；结果位于 `result.certificate`，包含证书有效期、SAN、域名匹配状态、实际连接地址和错误信息。
- `title` 任务接收域名或 HTTP(S) URL，裸域名保持 HTTPS、HTTPS WWW、HTTP、HTTP WWW 的顺序尝试；每个候选地址独立限制为 15 秒，并同时受任务总超时限制。结果位于 `result.title`，包含标题、最终 URL、HTTP 状态、内容类型和检测时间。
- 标题请求默认使用 `SituationAwareness-Agent/1.0`。仅收到 HTTP 403 时，以 `curl/8.5.0` 为 User-Agent 对同一请求重试一次，其他请求头保持不变，无需安装 curl。重试与首次请求共用当前候选的 15 秒预算，不额外延长 Master 下发的总超时（建议 `TITLE_AGENT_TIMEOUT=60s`）。重试成功后沿用原有标题解析；仍失败则继续下一个候选，失败信息会注明兼容重试的结果。HTTP 403 正文本身不会作为有效标题返回。

## 验证与构建

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o bin/situation-awareness-agent ./cmd/agent
```
