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

## 配置参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `AGENT_LISTEN_ADDR` | `:8002` | 监听地址；所有 Agent 统一使用 8002 |
| `AGENT_NAME` | 主机名 | 返回给 Master 的节点名称，建议与注册中心 `agentName` 一致 |
| `AGENT_SHARED_TOKEN` | 空 | Master/Agent 共享令牌；生产环境必须设置 |
| `AGENT_MAX_CONCURRENT` | `8` | 单节点同时执行的最大任务数 |
| `AGENT_DEFAULT_TIMEOUT` | `10s` | 未指定任务超时时使用的默认值 |
| `AGENT_MAX_TIMEOUT` | `30s` | 单次任务允许的最大超时 |
| `AGENT_TITLE_MAX_RESPONSE_BYTES` | `2097152` | 标题任务允许读取的最大网页字节数，最大 10 MiB |

未设置共享令牌时 Agent 为便于初次联调会启动，但日志会输出安全警告。正式部署时应在安全组中仅允许 Master IP 访问 8002，并设置共享令牌。

## API 契约

- `GET /healthz`：进程健康状态。
- `POST /api/v1/tasks`：执行任务。支持 `type=probe`、`type=certificate` 和 `type=title`。
- 请求体最大 64 KiB；端口最多 10 个；任务超时不能超过 `AGENT_MAX_TIMEOUT`。
- 合法任务即使目标不可达也返回 HTTP 200，并通过 `result.available=false` 和各步骤的 `error` 描述探测结果。参数错误、未授权或节点繁忙分别返回 400、401、429。
- `certificate` 任务默认读取目标的 443 端口，也可通过 `options.ports` 指定一个测试端口；结果位于 `result.certificate`，包含证书有效期、SAN、域名匹配状态、实际连接地址和错误信息。
- `title` 任务接收域名或 HTTP(S) URL，依次尝试 HTTPS/HTTP 及适用的 `www` 候选。疑似软 404 标题会对最终 URL 使用浏览器 User-Agent 和禁缓存请求头复查；仍异常时继续下一个候选，全部失败则返回 `result.available=false`，不会把软 404 当成有效标题。
- 标题结果位于 `result.title`，包含标题、最终 URL、HTTP 状态、内容类型、Server 响应头、检测时间和 `attempts` 诊断列表。每次尝试记录请求/最终 URL、User-Agent、状态码、标题、判定结果及失败原因；Agent 名称位于响应顶层的 `agent` 字段。

## 验证与构建

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o bin/situation-awareness-agent ./cmd/agent
```
