# HttpBroker

**基于 HTTP 的轻量级 NAT 穿透和内网穿透解决方案**，让防火墙后、NAT 后或受限网络中的机器通过中间提供者访问资源。仅使用标准 HTTP/HTTPS 流量作为反向代理隧道 — 无需 VPN 客户端，无需修改防火墙配置。

## 核心特性

- 🌐 **NAT 穿透 / 内网穿透** — 访问 NAT、防火墙或私有网络后的服务，无需端口转发或 VPN 配置
- 🔒 **防火墙友好** — 使用看起来像普通 Web API 调用的标准 HTTP/HTTPS 流量 — 绕过严格的企业代理和防火墙
- 🚀 **零基础设施** — 无需 VPN 服务器设置，无需复杂路由，无需 iptables 规则 — 只需三个简单的二进制文件
- 🎯 **SOCKS5 代理** — 适用于任何支持 SOCKS5 的浏览器或应用程序（Chrome、Firefox、curl、SSH 等）
- 🔀 **反向代理隧道** — NAT 后的 Provider 机器可以暴露其网络，无需配置入站端口转发
- 📡 **HTTP 长轮询传输** — 使用 HTTP 长轮询维护持久隧道 — 兼容大多数企业代理服务器
- 🔐 **隐私保护** — DNS 查询在 Provider 端解析，向本地 DNS 隐藏你的浏览目的地
- 🔑 **SSH 隧道支持** — 通过代理隧道传输 SSH 连接，访问 NAT/防火墙后的远程服务器

## 使用场景

- **远程访问私有网络** — 访问运行在 NAT 或企业防火墙后的内部服务（数据库、API、管理面板）
- **SSH 到内部服务器** — 无需端口转发或 VPN 设置即可 SSH 到 NAT/防火墙后的机器
- **绕过网络限制** — 通过限制较少的网络位置的 Provider 路由流量
- **开发与测试** — 针对本地机器上运行的服务测试 Webhook 和外部集成
- **物联网与家庭网络** — 访问家庭网络上的设备，无需将其直接暴露到互联网
- **VPN 替代方案** — 比传统 VPN 更简单的基于代理的流量路由设置

## 概述

HttpBroker 在三台机器之间创建 TCP 隧道：

- **机器 A（Broker）** — 中央中继服务器，可同时被 B 和 C 访问
- **机器 B（Consumer）** — 运行本地 SOCKS5 代理；你的浏览器连接到这里
- **机器 C（Provider）** — 在其本地网络上拨号目标主机并中继数据

```
┌─────────────┐            ┌─────────────┐            ┌─────────────┐
│  机器 B     │            │  机器 A     │            │  机器 C     │
│ (Consumer)  │◄──HTTP/S──►│  (Broker)   │◄──HTTP/S──►│ (Provider)  │
│             │            │             │            │             │
│ SOCKS5 :1080│            │ HTTP/S      │            │ 拨号目标    │
│ 浏览器 ──►  │ ─────────► │ :8080       │ ─────────► │ 主机:端口   │
│             │ ◄───────── │             │ ◄───────── │             │
└─────────────┘            └─────────────┘            └─────────────┘
```

**流量流向：**

```
浏览器 → SOCKS5 (B:1080) → Broker (A:8080) → Provider (C) → 目标网站
```

DNS 在机器 C（Provider）上解析，因此 VPN 内部或私有主机名可以正常工作。

## 工作原理

### 长轮询传输

Consumer 和 Provider 都维护对 Broker 的持续 HTTP POST 请求循环：

1. **POST 请求体**携带上行数据（从客户端到 broker）
2. **响应体**携带下行数据（从 broker 到客户端）
3. Broker 保持响应打开（长轮询），直到有数据可用或超时（30秒）
4. 客户端在收到响应后立即发送下一个 POST

对任何网络观察者来说，这看起来像一个 Web 应用程序发出常规 API 调用 — 没有 WebSocket，没有持久连接，没有特殊协议。

### yamux 多路复用

多个浏览器连接（标签页、并发请求）通过 [hashicorp/yamux](https://github.com/hashicorp/yamux) 在单个逻辑 HTTP 会话上多路复用。每个浏览器连接成为一个 yamux 流，所有流共享同一个轮询循环。

### SOCKS5 代理

Consumer 运行本地 SOCKS5 服务器。当你的浏览器通过 SOCKS5 代理发出请求时，Consumer 打开一个新的 yamux 流，通过隧道发送 CONNECT 请求，Provider 在其网络上拨号目标主机。

### 请求头清理

Provider 可以选择性地从 HTTP 请求中去除显示代理的请求头（`X-Forwarded-For`、`Via`、`Proxy-Authorization` 等）后再转发它们。

## NAT 穿透与内网穿透

HttpBroker 在 **NAT 穿透**和**内网穿透**场景中表现出色，特别适用于传统 VPN 或直连不可行的情况：

### NAT 穿透工作原理

关键在于 **Provider 主动发起所有到 Broker 的连接**。这意味着：

- ✅ **无需在 Provider 网络上配置入站端口转发**
- ✅ **适用于运营商级 NAT (CGNAT)**，即使公共 IP 地址被共享
- ✅ **绕过严格防火墙**，仅需允许出站 HTTP/HTTPS 流量
- ✅ **穿透企业代理**，因为所有流量看起来像标准的 Web API 调用

### 与传统解决方案对比

| 功能 | HttpBroker | 传统 VPN | 端口转发 | 其他隧道 |
|------|------------|----------|----------|----------|
| **NAT 穿透** | ✅ 内置 | ⚠️ 需要配置 | ❌ 需要公网 IP | ✅ 视情况而定 |
| **防火墙友好** | ✅ 仅 HTTP/HTTPS | ❌ 特殊协议 | ❌ 需要入站流量 | ⚠️ 可能被阻止 |
| **设置复杂度** | ✅ 低（3个二进制） | ❌ 高（服务器设置、证书、路由） | ⚠️ 中（路由器配置） | ⚠️ 视情况而定 |
| **企业代理兼容** | ✅ 是 | ❌ 通常被阻止 | ❌ 不适用 | ❌ 通常被阻止 |
| **CGNAT 后工作** | ✅ 是 | ⚠️ 困难 | ❌ 否 | ✅ 通常可以 |
| **本地网络隐私** | ✅ DNS 在 Provider | ✅ 完全加密 | ❌ 本地 DNS | ⚠️ 视情况而定 |

### 实际应用场景

**场景 1：CGNAT 后的家庭网络**
```
问题：ISP 使用 CGNAT，你没有公网 IP 地址
解决方案：在家庭网络上部署 Provider，VPS 上部署 Broker，笔记本上部署 Consumer
结果：从任何地方访问家庭服务（NAS、物联网设备）
```

**场景 2：严格的企业网络**
```
问题：企业防火墙阻止 VPN 协议，仅允许 HTTP/HTTPS
解决方案：在个人服务器上部署 Provider，云端部署 Broker，工作笔记本上部署 Consumer
结果：使用看起来像网页浏览的流量绕过限制
```

**场景 3：开发测试**
```
问题：需要针对 localhost 上运行的服务测试 Webhook
解决方案：在开发机上部署 Provider，VPS 上部署 Broker，测试机上部署 Consumer
结果：外部服务可以通过隧道访问你的本地服务器
```

**场景 4：物联网设备管理**
```
问题：管理客户现场各种 NAT 配置后的物联网设备
解决方案：现场部署 Provider，云端部署 Broker，管理工作站部署 Consumer
结果：无论客户网络拓扑如何，都能获得一致的管理界面
```

**场景 5：SSH 访问家庭服务器**
```
问题：旅行时需要 SSH 到家庭服务器，但 ISP 使用 CGNAT
解决方案：在家庭服务器上部署 Provider，便宜的 VPS 上部署 Broker，笔记本上部署 Consumer
结果：ssh -o ProxyCommand='nc -x 127.0.0.1:1080 %h %p' user@homeserver.local
      从任何地方访问家庭服务器，无需将 SSH 端口暴露到互联网
```

## 快速开始

### 前置要求

- **Go 1.25+**
- 三台机器（或在一台机器上打开三个终端窗口进行测试）

### 构建

```bash
# 为当前平台构建所有三个二进制文件
make build-all

# 为树莓派交叉编译 broker（arm64）
make build-pi

# 为旧版树莓派交叉编译 broker（armv7）
make build-pi-armv7

# 为 linux/amd64 交叉编译所有二进制文件（VPS/服务器）
make build-linux

# 使用版本信息构建
make build-release VERSION=v1.0.0
```

二进制文件放置在 `bin/` 目录中。

### 机器 A — 启动 Broker

```bash
# 使用配置文件（默认：configs/broker.yaml）
./bin/httpbroker-broker --config configs/broker.yaml

# 或使用 CLI 标志
./bin/httpbroker-broker --listen :8080

# 使用 TLS
./bin/httpbroker-broker --listen :8443 --tls-cert server.crt --tls-key server.key
```

broker 默认监听 `:8080` 并等待 Consumer 和 Provider 连接。

**健康检查：**

```bash
curl http://BROKER_IP:8080/status
```

### 机器 B — 启动 Consumer

```bash
# 使用配置文件
./bin/httpbroker-consumer --config configs/consumer.yaml

# 或使用 CLI 标志
./bin/httpbroker-consumer --broker-url http://BROKER_IP:8080 --endpoint vpn1 --socks5-listen :1080
```

这会在 `127.0.0.1:1080` 上启动一个 SOCKS5 代理。将你的浏览器指向此地址（参见下面的[浏览器配置](#浏览器配置)）。

### 机器 C — 启动 Provider

```bash
# 使用配置文件
./bin/httpbroker-provider --config configs/provider.yaml

# 或使用 CLI 标志
./bin/httpbroker-provider --broker-url http://BROKER_IP:8080 --endpoint vpn1 --scrub-headers
```

Provider 连接到 Broker 并等待隧道请求。它在其本地网络上拨号目标主机并通过 Broker 中继流量。

### 使用 curl 测试

一旦所有三个节点都在运行：

```bash
curl --socks5-hostname 127.0.0.1:1080 http://example.com
```

## 浏览器配置

**重要：** 你必须启用远程 DNS 解析，以便域名在 Provider（机器 C）上解析，而不是在你的本地机器上。

### Firefox

1. 打开 **设置** → **常规** → 滚动到 **网络设置** → 点击 **设置…**
2. 选择 **手动代理配置**
3. 设置 **SOCKS 主机**：`127.0.0.1`，**端口**：`1080`
4. 选择 **SOCKS v5**
5. ✅ 勾选 **使用 SOCKS v5 时代理 DNS**
6. 点击 **确定**

### Chrome / Chromium

使用代理标志启动 Chrome：

```bash
google-chrome \
  --proxy-server="socks5://127.0.0.1:1080" \
  --host-resolver-rules="MAP * ~NOTFOUND , EXCLUDE 127.0.0.1"
```

`--host-resolver-rules` 标志强制通过 SOCKS5 代理进行 DNS 解析。

### curl

```bash
# --socks5-hostname 在远程端（Provider）解析 DNS
curl --socks5-hostname 127.0.0.1:1080 http://example.com

# HTTPS 也可以工作
curl --socks5-hostname 127.0.0.1:1080 https://example.com
```

### 系统级代理（macOS）

1. 打开 **系统偏好设置** → **网络** → 选择你的连接 → **高级** → **代理**
2. 启用 **SOCKS 代理**
3. 将服务器设置为 `127.0.0.1`，端口设置为 `1080`

### SSH（远程服务器访问）

你可以通过 SOCKS5 代理隧道传输 SSH 连接，以访问 NAT 或防火墙后的远程服务器：

**方法 1：使用 ProxyCommand 和 netcat**
```bash
# 一次性连接
ssh -o ProxyCommand='nc -x 127.0.0.1:1080 %h %p' user@remote-server.internal

# HTTPS 连接
ssh -o ProxyCommand='nc -X connect -x 127.0.0.1:1080 %h %p' user@remote-server.internal
```

**方法 2：在 ~/.ssh/config 中配置**
```bash
# 编辑 ~/.ssh/config
Host remote-server.internal
    ProxyCommand nc -x 127.0.0.1:1080 %h %p
    User your-username

# 或为所有主机通过此代理
Host *.internal
    ProxyCommand nc -x 127.0.0.1:1080 %h %p

# 然后简单连接
ssh remote-server.internal
```

**方法 3：使用 SSH 原生 SOCKS 支持（OpenSSH 7.6+）**
```bash
# 一次性连接
ssh -o ProxyCommand='ssh -W %h:%p -o ProxyCommand="nc -x 127.0.0.1:1080 localhost 22" jumphost' user@remote-server

# 或使用 socat 更简单
ssh -o ProxyCommand='socat - SOCKS4A:127.0.0.1:%h:%p,socksport=1080' user@remote-server.internal
```

**方法 4：使用 ProxyJump（多跳）**
```bash
# 首先在 ~/.ssh/config 中配置代理
Host proxy-tunnel
    HostName 127.0.0.1
    Port 1080
    ProxyCommand nc -x 127.0.0.1:1080 %h %p

Host internal-server
    HostName remote-server.internal
    User your-username
    ProxyJump proxy-tunnel

# 然后连接
ssh internal-server
```

**SSH 隧道使用场景：**
- 访问 NAT 后的服务器，无需端口转发
- 从外部 SSH 到企业网络上的机器
- 管理防火墙后的物联网设备或嵌入式系统
- 旅行时访问家庭服务器
- 连接到受限网络中的开发机器
- 在企业环境中绕过 SSH 限制

## 配置

每个二进制文件都读取一个 YAML 配置文件并支持 CLI 标志覆盖。CLI 标志优先于配置文件值。

### Broker（`configs/broker.yaml`）

```yaml
server:
  listen: ":8080"          # 监听地址
  tls:
    enabled: false         # 启用 TLS
    cert_file: ""          # TLS 证书路径
    key_file: ""           # TLS 私钥路径

tunnel:
  poll_timeout: "5s"       # 在返回空响应之前保持轮询请求多长时间
  session_timeout: "5m"    # 断开空闲时间超过此值的会话

auth:
  enabled: false           # 身份验证（预留供未来使用）

logging:
  level: "info"            # 日志级别：debug、info、warn、error
```

### Consumer（`configs/consumer.yaml`）

```yaml
broker:
  url: "http://127.0.0.1:8080"   # Broker URL
  endpoint: "default"             # 端点名称（必须与 Provider 匹配）

socks5:
  listen: ":1080"                 # 本地 SOCKS5 监听地址

transport:
  poll_interval: "50ms"           # 轮询请求之间的延迟
  retry_backoff: "5s"             # 出错时重新连接前的等待时间

logging:
  level: "info"                   # 日志级别：debug、info、warn、error
```

### Provider（`configs/provider.yaml`）

```yaml
broker:
  url: "http://127.0.0.1:8080"   # Broker URL
  endpoint: "default"             # 端点名称（必须与 Consumer 匹配）

provider:
  scrub_headers: true             # 清除显示代理的 HTTP 请求头
  dial_timeout: "10s"             # 拨号目标主机时的超时

transport:
  poll_interval: "50ms"           # 轮询请求之间的延迟
  retry_backoff: "5s"             # 出错时重新连接前的等待时间

logging:
  level: "info"                   # 日志级别：debug、info、warn、error
```

### 多个端点

你可以在不同的端点上运行多个 Provider 以访问不同的网络：

```yaml
# Provider 1 — configs/provider-vpn1.yaml
broker:
  url: "http://BROKER_IP:8080"
  endpoint: vpn1

# Provider 2 — configs/provider-office.yaml
broker:
  url: "http://BROKER_IP:8080"
  endpoint: office
```

然后为每个端点启动单独的 Consumer：

```bash
# 连接到 vpn1 的 Consumer
./bin/httpbroker-consumer --broker-url http://BROKER_IP:8080 --endpoint vpn1 --socks5-listen :1080

# 连接到 office 的 Consumer
./bin/httpbroker-consumer --broker-url http://BROKER_IP:8080 --endpoint office --socks5-listen :1081
```

每个 Consumer 获得自己的 SOCKS5 端口，你可以配置不同的浏览器或配置文件使用不同的代理。

## CLI 参考

### httpbroker-broker

```
运行 broker 服务器，在 consumer 和 provider 之间中继流量。

用法：
  httpbroker-broker [flags]

标志：
  -c, --config string    配置文件路径（默认 "configs/broker.yaml"）
      --listen string    覆盖监听地址（例如 :8080）
      --tls-cert string  TLS 证书文件
      --tls-key string   TLS 密钥文件
  -h, --help             httpbroker-broker 的帮助
```

### httpbroker-consumer

```
运行 consumer SOCKS5 代理，通过 broker 隧道传输浏览器流量。

用法：
  httpbroker-consumer [flags]

标志：
  -c, --config string         配置文件路径（默认 "configs/consumer.yaml"）
      --broker-url string     broker URL（例如 http://192.168.1.100:8080）
      --endpoint string       端点名称
      --socks5-listen string  SOCKS5 监听地址（例如 :1080）
  -h, --help                  httpbroker-consumer 的帮助
```

### httpbroker-provider

```
运行 provider，拨号目标主机并通过 broker 返回响应。

用法：
  httpbroker-provider [flags]

标志：
  -c, --config string      配置文件路径（默认 "configs/provider.yaml"）
      --broker-url string  broker URL
      --endpoint string    端点名称
      --scrub-headers      从 HTTP 请求中清除代理请求头（默认 false）
  -h, --help               httpbroker-provider 的帮助
```

## 架构

项目遵循清晰的 Go 包结构：

```
cmd/
  broker/       → httpbroker-broker 二进制文件
  consumer/     → httpbroker-consumer 二进制文件
  provider/     → httpbroker-provider 二进制文件
internal/
  broker/       → Broker 服务器、端点注册、中继逻辑
  consumer/     → SOCKS5 服务器、yamux 客户端、隧道拨号器
  provider/     → Provider 客户端、目标拨号器、请求头清理器
  transport/    → HTTP 长轮询传输、基于管道的会话、httpconn 适配器
  config/       → YAML 配置加载、日志设置
configs/        → 示例 YAML 配置文件
plans/          → 架构文档
```

有关详细的技术设计，请参阅 [plans/architecture.md](plans/architecture.md)。

## 安全说明

- **HTTP vs HTTPS**：默认情况下，节点之间的流量使用纯 HTTP。对于生产使用，在 Broker 上启用 TLS（在 `broker.yaml` 中设置 `tls.enabled: true`）或将其放在带 TLS 终止的反向代理后面。没有 TLS，隧道流量对网络观察者是可见的。

- **请求头清理**：Provider 可以去除显示代理使用的请求头，如 `X-Forwarded-For`、`Via` 和 `Proxy-Authorization`。在 Provider 配置中使用 `scrub_headers: true` 或在 CLI 上使用 `--scrub-headers` 启用。

- **身份验证**：auth 中间件是一个占位符。目前节点之间没有身份验证。在没有添加身份验证或通过 IP 限制访问的情况下，不要将 Broker 暴露到公共互联网。

- **DNS 隐私**：DNS 查询在 Provider（机器 C）上解析。这意味着你的本地 DNS 解析器永远看不到你通过隧道访问的域名，但 Provider 的 DNS 解析器可以看到。

- **端点命名**：任何知道 Broker URL 和端点名称的人都可以作为 Consumer 或 Provider 连接。在实现适当的身份验证之前，将端点名称视为共享密钥。

## 树莓派部署

Broker 设计为在树莓派上作为轻量级中继运行：

```bash
# 为树莓派 3/4/5（64位操作系统）构建
make build-pi

# 为旧版树莓派（32位操作系统）构建
make build-pi-armv7

# 复制到树莓派
scp bin/httpbroker-broker-arm64 pi@raspberrypi:~/httpbroker-broker
scp configs/broker.yaml pi@raspberrypi:~/broker.yaml

# 在树莓派上运行
ssh pi@raspberrypi
./httpbroker-broker --config broker.yaml --listen :8080
```

## 许可证

详情请参阅 [LICENSE](LICENSE)。

