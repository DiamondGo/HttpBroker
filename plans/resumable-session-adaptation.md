# HttpBroker 适配:pollmux 跨重连会话恢复（Resumable Session）

> 前置:本文对应 pollmux 侧的设计
> `~/source/pollmux/plans/pollmux-resumable-session-design.md`（跨重连会话恢复）。
> 那份文档描述 pollmux 内部如何在传输断开重连后保留 yamux 会话；**本文只记录 pollmux 升级
> 到带该能力的版本后,HttpBroker 需要做的适配**。在 pollmux 未发布该能力前,本文不产生任何
> 代码改动,仅作待办记录。

> **状态（2026-09-02）：已落地，基于 pollmux v0.2.0。** pollmux 最终发布的接口与 §4 的期望一致：
> `Session.Resumable()` / `Session.ResumeDeadline()`、`ServerConfig.EnableResume/ResumeGrace`、
> `Connector.PreferResume`，另需挂载 `pollmux.ResumeHandler` 到 `POST /tunnel/{id}/resume`（与
> `/poll`、`/ws`、`DELETE` 同一鉴权中间件之后）。一点与 §3.3 预期不同：v0.2.0 的
> `CloseSessionIfNoPollInFlight` 本身已把「脱离中但仍在宽限期内的可恢复会话」视为忙碌而拒绝关闭，
> 所以 fast reaper 的放行不再是正确性所必需，但仍按 §3.3 实现（`markBrokenPoll` 与
> `sweepBrokenPolls` 都跳过 `Resumable()` 会话），避免与 pollmux 的 sweeper 竞争并消除误导性的
> 淘汰日志。回归测试见 `internal/broker/reaper_test.go`（`*Resumable*`）与 `integration_test.go`
> （`TestIntegration_ResumableStream` / `TestIntegration_ResumableWebSocket`）。§7 中需要真实
> Cloudflare/SSH 环境的验证项仍待人工执行。

## 1. 背景（为什么要做这件事）

当前链路:consumer 与 provider 各有一条传输经过 Caddy/Cloudflare 连到 broker。任意一条被中间层
的「最大连接存活时间」按时(实测约一小时)掐断,pollmux 客户端就会重连并**重建一个全新
session**,`ClientSession(conn)` 随之重建全新 yamux 会话,旧会话里的所有 stream(包括正在用的
SSH)被切断。「有数据也断」正是因为这类掉线由外部存活上限触发,与流量无关,pollmux 自身的秒级
心跳/滚动/空闲淘汰都保护不了它。

pollmux 的 Resumable 能力让传输断开重连后 yamux 会话**原样存活**(序号+ACK+重放补齐接缝丢失的
字节,`/resume` 端点重接),从根本上让 SSH 之类的长流扛住这类掉线。

## 2. 适配总原则

**绝大部分工程量在 pollmux;HttpBroker 侧只有小而局部的改动。** 关键只有一处(fast reaper),
其余是配置透传与接线。中继逻辑完全不动。

前提约束:**consumer↔broker 与 provider↔broker 两跳都必须开启 resume**。任一跳不可恢复,那跳
断裂仍会杀掉流,SSH 照断。

## 3. 需要改动的位置

### 3.1 配置字段 — `internal/config/config.go`（机械改动）

- `TunnelConfig`(broker)新增:
  ```go
  // EnableResume 开启后,broker 允许为请求恢复的会话在传输断开后保留至 ResumeGrace,
  // 期间接受 /resume 重接,使 yamux 会话及其 stream 跨重连存活。默认 false,纯附加。
  EnableResume bool          `mapstructure:"enable_resume"`
  // ResumeGrace 是传输脱离后保留可恢复会话、等待 /resume 的最长时间。0/未设用 pollmux 默认。
  // 需有上限(抵御内存放大,见 pollmux 设计 §12)。
  ResumeGrace  time.Duration `mapstructure:"resume_grace"`
  ```
- `TransportConfig`(consumer/provider 共用)新增:
  ```go
  // PreferResume 请求 broker 为本会话协商跨重连恢复。仅在 broker 也 EnableResume 时生效,
  // 协商是「client 请求 && server 支持」,对老 broker 留 true 也安全(静默回退到今天的行为)。
  PreferResume bool `mapstructure:"prefer_resume"`
  ```
- 映射目标:`pollmux.ServerConfig.EnableResume/ResumeGrace`、`pollmux.Connector.PreferResume`
  （字段名以 pollmux 最终发布为准）。

写法与现有 `EnableWebSocket` / `PreferWebSocket` 完全一致,照抄即可。

### 3.2 接线 — `cmd/broker|consumer|provider/main.go`（机械改动）

把 3.1 的配置字段填进 `broker.Config` / `consumer.Config` / `provider.Config`,再进
`pollmux.ServerConfig` / `pollmux.Connector`。同样照 `EnableWebSocket`/`PreferWebSocket` 的现有
接线抄。

`internal/broker/server.go` 的 `Config` 结构、`internal/consumer/client.go` 与
`internal/provider/client.go` 的 `Config` 结构各加对应字段并透传给 pollmux。

### 3.3 Fast reaper —— 唯一有实质性的改动 — `internal/broker/server.go`

这是必须、也是唯一需要动脑的地方。

**现状**:一次 poll 因 TCP 掉线结束时 `markBrokenPoll` 记时间戳;`sweepBrokenPolls` 每
`fastReaperInterval`(2s)扫描,对超过 `brokenPollGrace`(5s)且无 poll 在途的会话调用
`CloseSessionIfNoPollInFlight` 淘汰。这个 5s 快速淘汰是为了让死掉的 provider 尽快让位给替补
provider,避免 60s 隧道黑洞。

**冲突**:对**可恢复**会话,传输掉线是正常现象——我们要把它**保留 `ResumeGrace`**(如 30s)
等 `/resume`。若 fast reaper 在 5s 就淘汰,`5s~ResumeGrace` 之间的恢复必然失败,会话被误杀,
SSH 照断,等于没做。

**改法**:让 fast reaper **对可恢复会话放行**,交给 pollmux 自己的 resume-aware sweeper 按
`ResumeGrace` 处理。

```go
// markBrokenPoll:可恢复会话的 poll 掉线是预期内的,不记为 broken。
func (s *Server) markBrokenPoll(r *http.Request) {
    id := s.pcfg.SessionIDFunc(r)
    sess, ok := s.store.Get(id)
    if !ok || sess.IsClosed() {
        return
    }
    if sess.Resumable() { // pollmux 需暴露的小接口,见 §4
        return
    }
    s.brokenPollsMu.Lock()
    s.brokenPolls[sess.ID] = time.Now()
    s.brokenPollsMu.Unlock()
}

// sweepBrokenPolls:遍历候选时,对可恢复会话直接跳过(不 evict、并清掉候选记录)。
// 即便 markBrokenPoll 已放行,这里再兜一层,防止会话在协商成 Resumable 前后有竞态残留。
```

- 淘汰仍复用既有原子条件关闭 `CloseSessionIfNoPollInFlight`,不新增 TOCTOU 窗口。
- **非可恢复会话行为完全不变**,仍 5s 快速淘汰,保留今天替补 provider 的快速接管特性。
- 该分支需要一个「这个会话是不是可恢复」的判断,见 §4 对 pollmux 的接口期望。

## 4. 对 pollmux 发布接口的期望

HttpBroker 的 fast reaper 需要一个极小的表面来识别可恢复会话,期望 pollmux 提供其一:

- `func (s *Session) Resumable() bool`,或
- 在 `Session.Meta()` 里带一个约定键(如 `"__resumable"`)。

推荐前者(显式、无需约定魔法键)。除此之外 HttpBroker 不依赖 pollmux 恢复实现的任何内部细节。

`ResumeGrace` 的实际保留/淘汰逻辑期望在 **pollmux 的 sweeper** 内实现(resume-aware),HttpBroker
的 fast reaper 只负责「跳过可恢复会话」这一件事。正确性收在 pollmux,HttpBroker 侧最小。

## 5. 明确**不需要**改动的位置

- **`internal/broker/relay.go`** — `bridgeStream` / `bridgeConns` 的双向 `io.Copy` 完全不动。
  续传能力全在两跳各自的 pollmux 内部,中继依旧是哑巴转发。
- **`relay.go` 的 `HandleProvider` / `HandleConsumer`** — 不动。它们仍
  `ClientSession/ServerSession(session)` 建 yamux,再 `<-yamuxSess.CloseChan()` 阻塞到真正关闭。
- **关键认知**:服务端的 `*Session` 今天就已经能扛住瞬断——一次 poll 结束并不关闭 Session,
  pipe 不关,yamux 只是 stall(读 `toServer` 空、写 `toClient` 缓冲)。今天在瞬断时真正杀掉服务端
  yamux 的,**正是那个 5s fast reaper**(及 60s sweeper)。因此 §3.3 让 fast reaper 放行之后,
  服务端 yamux 自然存活,`HandleProvider/HandleConsumer` 无需任何改动。
- **`EndpointRegistry`**(`internal/broker/endpoint.go`)— 不动。可恢复会话在恢复窗口内保持注册,
  正是我们想要的(provider 短暂脱离期间不驱逐其 endpoint 注册,恢复后隧道续上,consumer 无感)。
  这一点由「fast reaper 不淘汰可恢复会话」自然达成,无需额外改动。

## 6. 配置示例（升级后）

`local/consumer-global.yaml` 与 provider 侧对应文件的 `transport` 段:

```yaml
transport:
  poll_mode: "stream"
  upload_stream_preference: "stream"
  prefer_websocket: true
  prefer_resume: true          # 新增:请求跨重连会话恢复
```

broker 配置(`configs/broker.yaml`)的 `tunnel` 段:

```yaml
tunnel:
  enable_websocket: true
  enable_resume: true          # 新增:允许协商恢复
  resume_grace: "30s"          # 新增:传输脱离后保留会话等待 /resume 的最长时间
```

两跳(consumer 与 provider)都要开 `prefer_resume`,否则未开的那跳断裂仍会杀流。

## 7. 验证清单（升级并适配后）

- consumer 侧人为切断底层传输(如临时阻断到 broker 的连接数秒)后,已建立的 SSH 会话**存活并继续**,
  日志出现 `/resume` 成功而非「重建新 session」。
- provider 侧同样切断,consumer 的 SSH 会话存活。
- 复现原始故障:长时间(>1 小时)保持一个 `top` 之类持续输出的 SSH,验证不再于约一小时处掉线。
- `enable_resume: false` 或对端未开时,行为与今天逐字节一致(回归)。
- 非可恢复会话(如老客户端)仍按 5s fast reaper 快速淘汰,替补 provider 接管速度不退化。
- broker 保留可恢复会话不超过 `resume_grace`;死客户端不会无限占用会话与重放缓冲。

## 8. 落地顺序

1. 等 pollmux 发布带 Resumable 能力(及 `Session.Resumable()` 或等价接口)的版本,`go.mod` 升级。
2. §3.1 / §3.2 配置与接线(机械)。
3. §3.3 fast reaper 放行可恢复会话(核心,约十几行)。
4. §7 验证清单,重点回归非可恢复路径。
5. 更新 `configs/*.yaml` 示例与 `README` / `MIGRATION_POLLMUX.md` 的说明。

## 9. 一句话总结

HttpBroker 的适配 = **配置 + main 接线(纯机械)** + **fast reaper 对可恢复会话放行(唯一实质
改动)**;中继、relay、endpoint registry 全部零改动。真正的工程量都在 pollmux。
