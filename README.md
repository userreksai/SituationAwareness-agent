# SituationAwareness Agent

态势感知拨测节点。Agent 不监听任何 TCP 端口，启动后主动连接 SituationAwareness Master 的控制端口，接收结构化 DNS、TCP、HTTP 或 TLS 证书检测任务，并通过同一条 WebSocket 长连接返回结果。

Agent 不执行任意 Shell 命令。节点管理记录的不可变 `id` 是调度身份；IP 仅作为节点资料。每台 Agent 使用独立 Token，Master 在长连接握手时通过节点管理服务完成认证和 `agentId` 绑定。

## 注册顺序

1. 为 Agent 生成至少 32 字符的随机 Token，并配置 `AGENT_NAME`。
2. 在 `node_registry_manager` 中新增节点，填写相同的 Agent 名称和 Token。端口字段属于旧版兼容资料，不再用于任务下发。
3. 配置 Master IP，例如 `ws://10.0.0.10:9910/api/v1/agent/connect`。
4. 启动 Agent。Master 返回的 `agentId` 来源于节点管理记录，不依赖 Agent IP。

也可以先启动 Agent、再登记节点。登记完成前 Agent 会收到 401 并自动退避重连，登记后无需再次安装。

## 源码部署测试

```bash
git clone https://github.com/userreksai/SituationAwareness-agent.git
cd SituationAwareness-agent
cp .env.example .env
```

生成节点独立 Token：

```bash
openssl rand -hex 32
```

配置环境变量后启动：

```bash
export AGENT_MASTER_URL='ws://MASTER_IP:9910/api/v1/agent/connect'
export AGENT_NAME='node-source-01'
export AGENT_SHARED_TOKEN='与节点管理记录完全相同的节点Token'
go run ./cmd/agent
```

成功日志应包含 `registered by Master as agent_id=...`。Agent 主机上不应出现本进程的监听端口。

生产源码部署可编译后使用 `deploy/situation-awareness-agent.service`。环境文件默认位置为 `/etc/situation-awareness-agent.env`，权限应设置为 `0600`。

## Docker 部署测试

Docker 方案不使用 `--publish` 或 `-p`：

```bash
sudo env \
  MASTER_HOST=10.0.0.10 \
  AGENT_NAME=node-docker-01 \
  bash deploy/install-agent-docker.sh
```

脚本会拉取 `beiou/situationawareness-agent:1.1.0`、创建只读容器、生成或复用节点 Token，并把配置保存到 `/etc/situation-awareness-agent/agent.env`。重复部署会复用已有 Token，避免节点管理凭据失效。

如果原容器存在，脚本会保留为 `situation-awareness-agent-rollback`。新容器启动失败时自动恢复；成功后也保留旧容器，便于人工回滚。

如果节点记录已经配置好，可要求脚本等待注册：

```bash
sudo env MASTER_HOST=10.0.0.10 AGENT_NAME=node-docker-01 WAIT_FOR_REGISTRATION=true \
  bash deploy/install-agent-docker.sh
```

使用 WSS 和私有 CA 时，额外传入宿主机 CA 文件；脚本会只读挂载到容器：

```bash
sudo env \
  AGENT_MASTER_URL='wss://MASTER_IP:9910/api/v1/agent/connect' \
  MASTER_CA_FILE='/root/master-ca.pem' \
  AGENT_NAME=node-docker-01 \
  bash deploy/install-agent-docker.sh
```

检查：

```bash
docker ps --filter name=situation-awareness-agent
docker logs --tail 100 situation-awareness-agent
docker port situation-awareness-agent
```

最后一条命令应为空，表示没有映射宿主机端口。

## 配置参数

| 参数 | 默认值 | 说明 |
| --- | --- | --- |
| `AGENT_MASTER_URL` | `ws://127.0.0.1:9910/api/v1/agent/connect` | Master 长连接地址；实际部署使用 Master IP |
| `AGENT_NAME` | 主机名 | 应与节点管理中的 `agentName` 一致 |
| `AGENT_SHARED_TOKEN` | 无 | 每节点独立 Token；必填且至少 32 字符 |
| `AGENT_MAX_CONCURRENT` | `8` | 同时执行任务的上限 |
| `AGENT_DEFAULT_TIMEOUT` | `10s` | 默认任务超时 |
| `AGENT_MAX_TIMEOUT` | `30s` | 最大任务超时 |
| `AGENT_RECONNECT_MIN` | `1s` | 初始重连等待时间 |
| `AGENT_RECONNECT_MAX` | `30s` | 最大重连等待时间 |
| `AGENT_HEARTBEAT_INTERVAL` | `20s` | 长连接心跳周期 |
| `AGENT_TLS_CA_FILE` | 无 | 使用 `wss://` 时可指定私有 CA 文件 |
| `AGENT_TLS_SERVER_NAME` | 无 | TLS 证书校验名称；IP SAN 证书通常不需要覆盖 |

## 网络与安全

- Agent 只需要出站访问 Master 的 TCP 9910，以及执行检测所需的 DNS/HTTP/HTTPS/指定 TCP 端口。
- Master 的 9910 应只对白名单 Agent IP 开放；即使有网络白名单，仍必须保留每节点 Token 认证。
- `ws://` 会以明文传输 Token、任务和结果，只适合受控专网。跨不可信网络使用 `wss://`，证书可以包含 Master IP 的 IP SAN，不要求配置域名。
- 不要在 URL、日志或命令行参数中放置节点 Token；使用权限为 `0600` 的环境文件。
- 节点 Token 更换后，Master 会断开旧连接；更新 Agent 配置后自动重新认证。

## 验证与构建

```bash
go test ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o bin/situation-awareness-agent ./cmd/agent
```

## 恢复旧架构

本次改造前的本地恢复分支是 `codex/backup-agent-before-persistent-connect-20260806`。切换前应先保存当前工作区改动；该分支仍是 Agent 监听 8002、由 Master 主动调用的架构。
