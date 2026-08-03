# 迁移到 pollmux

把 `internal/transport`（698 行）与 `internal/broker/server.go` 里的长轮询端点逻辑，换成外部库
[`github.com/DiamondGo/pollmux`](https://github.com/DiamondGo/pollmux)。

pollmux 是从本项目与 HttpHop 的共同部分抽出来的：**HTTP 长轮询虚拟连接 + yamux 多路复用**。它只管
字节怎么在两台机器之间流动，不解释这些字节，也不知道两端各是什么角色 —— role / endpoint 这类应用
语义一律走不透明的 `meta`。

迁移的收益不是少写代码，而是三件今天做不到的事：

1. **五个已知缺陷被修掉**（A1 客户端无超时、A3 掉线检测要 5 分钟、A5 会话关闭与心跳撞状态码、
   B1 下行吞吐被 64KB 缓冲卡住、以及写侧合并语义）。这些缺陷在本仓库今天都还在。
2. **`EnableKeepAlive=false` 这个正确性前提被封进 API**。今天它靠 4 处注释传递，任何人新建一个
   yamux 会话忘了这一行，链路健康时也会被判定为死连接。
3. **传输参数由服务端下发**，不再两边各配一份。

---

## 零、先看清楚：这是一次破坏性协议变更

**broker、provider、consumer 必须同时升级。** 混合版本不能工作。

| | 今天 | pollmux |
|---|---|---|
| 路由 | `POST /tunnel/connect`、`POST /tunnel/{id}/poll`、`DELETE /tunnel/{id}` | **完全相同** |
| connect 参数 | query：`?role=consumer&endpoint=web` | **JSON body**：`{"protocol_version":1,"meta":{"role":"consumer","endpoint":"web"}}` |
| connect 响应 | `{"session_id":"..."}` | `{"protocol_version":1,"session_id":"...","limits":{...},"meta":{...}}` |
| 会话已关闭 | poll 返回 **204**（与"超时无数据"同码） | poll 返回 **410** |
| 请求体超限 | 静默截断（`MaxBytesReader` 1MB 硬编码） | **413** 并关闭会话 |

好消息是两个方向都**干净失败**，不会静默错配：

- 新客户端 → 旧 broker：旧 broker 读不到 query 参数，回 400 `missing required query params`。
- 旧客户端 → 新 broker：没有 body，`protocol_version` 解析为 0，回 **426**。

所以升级顺序错了会立刻在日志里看见，不会出现"连上了但行为诡异"。

---

## 一、依赖与日志

```bash
go get github.com/DiamondGo/pollmux
go get go.uber.org/zap/exp   # 只为 zapslog，见下
```

pollmux 用 `log/slog`，不是 zap —— 一个共享库不应该把日志实现强加给使用方。本项目继续用 zap，
在边界上桥接一次即可：

```go
import "go.uber.org/zap/exp/zapslog"

// 每个持有 *zap.Logger 的地方，构造一个 slog 视图传给 pollmux。
slogger := slog.New(zapslog.NewHandler(logger.Core()))
```

`zapslog.NewHandler` 签名是 `NewHandler(core zapcore.Core, opts ...HandlerOption) *Handler`
（模块 `go.uber.org/zap/exp`，最新 v0.3.0）。

pollmux 的所有 `Logger` 字段传 `nil` 表示禁用日志，不会 panic。

---

## 二、对照表

| 现有代码 | 去向 |
|---|---|
| `internal/transport/pipe.go`（150 行） | **删除** → `pollmux.BufferedPipe`（另加高水位观测） |
| `internal/transport/session.go`（77 行） | **删除** → `pollmux.Session`（剥掉 Role/Endpoint，改走 meta） |
| `internal/transport/httpconn.go`（354 行） | **删除** → `pollmux.Connector` / `pollmux.Conn` |
| `internal/transport/transport.go`（117 行） | **删除** → 同上 |
| `server.go:handleConnect`（185-226） | `pollmux.ConnectHandler` + `Hooks.Authenticate` / `Hooks.OnConnect` |
| `server.go:handlePoll`（235-306） | `pollmux.PollHandler` |
| `server.go:handleDelete`（309-325） | `pollmux.DeleteHandler` |
| `server.go:generateSessionID`（164-170） | 库内私有，删掉 |
| `server.go:cleanupLoop` / `cleanupExpiredSessions`（368-396） | `pollmux.StartSweeper` |
| `server.go:Stop` 里的关会话循环（151-153） | `pollmux.CloseSession(store, hooks, s, ReasonServerClose)` |
| `provider/client.go:Run` 的重连循环（50-131） | `pollmux.ReconnectLoop` |
| `provider/client.go:acceptStreams`（155-209） | `pollmux.AcceptLoop` |
| `consumer/client.go:Run` 的重连循环（86-216） | `pollmux.ReconnectLoop`（Serve 自己写，见 §六） |
| `EndpointRegistry.sessions`（endpoint.go:32） | **删除** → `pollmux.SessionStore` 是唯一索引（见 §五） |
| 4 处 `yamux.DefaultConfig()` + `EnableKeepAlive=false` | `pollmux.ClientSession` / `pollmux.ServerSession` |

`internal/transport` 整个包删除。`internal/broker/relay.go`、`internal/provider/handler.go`、
`internal/consumer/dialer.go` 的业务逻辑**不动**，只改类型。

---

## 三、Broker：路由与配置

`NewServer` 里，三个路由的 handler 换成 pollmux 的，**中间件包法不变** —— pollmux 的三个 handler
都是裸 `http.Handler`：

```go
store := pollmux.NewSessionStore()
slogger := slog.New(zapslog.NewHandler(logger.Core()))

pcfg := pollmux.ServerConfig{
    PollTimeout:    config.PollTimeout,
    SessionTimeout: config.SessionTimeout,
    CoalesceWindow: config.CoalesceWindow,
    PollBufferSize: config.PollBufferSize, // 新增配置项，默认 256KB
    MaxSendBytes:   config.MaxSendBytes,   // 新增配置项，默认 1MB
    HighWaterWarn:  config.HighWaterWarn,  // 新增，0 表示关闭

    // 本项目用 gorilla/mux，不是 net/http 自带的路由。
    SessionIDFunc: func(r *http.Request) string { return mux.Vars(r)["id"] },

    Logger: slogger,
}

hooks := pollmux.Hooks{
    Authenticate: s.authenticateConnect,
    OnConnect:    s.onConnect,
    OnDisconnect: s.onDisconnect,
}

router.Handle("/tunnel/connect",
    AuthMiddleware(auth, redirEnabled, redirURL, pollmux.ConnectHandler(store, pcfg, hooks))).
    Methods("POST")
router.Handle("/tunnel/{id}/poll",
    AuthMiddleware(auth, redirEnabled, redirURL, pollmux.PollHandler(store, pcfg, hooks))).
    Methods("POST")
router.Handle("/tunnel/{id}",
    AuthMiddleware(auth, redirEnabled, redirURL, pollmux.DeleteHandler(store, pcfg, hooks))).
    Methods("DELETE")
```

> `ServerConfig` 在构造 handler 时会 **panic** 于两种配置错误：`PollMode` 非法，以及
> `SessionTimeout < 2*PollTimeout`。后者是 A3 的要求 —— 会话超时不到两个轮询周期，一个只是
> "正好处在两次 poll 之间"的健康客户端会被当成掉线扫掉。这是启动期失败，不是运行期惊喜。

### Hooks

role / endpoint 从 query 参数变成 `meta`，校验放进 `Authenticate`。注意 **token 校验仍然由
`AuthMiddleware` 做**，`Authenticate` 在这里只负责业务字段：

```go
func (s *Server) authenticateConnect(
    r *http.Request, req pollmux.ConnectRequest,
) (map[string]string, error) {
    role, endpoint := req.Meta["role"], req.Meta["endpoint"]
    if role == "" || endpoint == "" {
        return nil, pollmux.StatusErrorf(http.StatusBadRequest,
            "meta must carry both role and endpoint")
    }
    if role != "consumer" && role != "provider" {
        return nil, pollmux.StatusErrorf(http.StatusBadRequest,
            "role must be 'consumer' or 'provider', got %q", role)
    }
    return nil, nil // 不追加 meta
}
```

`Authenticate` 返回裸 error 时库回 401；需要别的状态码就用 `StatusErrorf` 包一层。

`OnConnect` 取代 `handleConnect` 后半段。**它在会话已经进 store 之后、响应写出之前调用** ——
这正是 `server.go:208-213` 那条注释保护的竞态（poll 早于注册到达导致持续 404），库里已经保证了，
不需要再自己 `RegisterSession`：

```go
type brokerSession struct {
    *pollmux.Session // 提供 ID / Read / Write / Close
    Role     string
    Endpoint string
}

func (s *Server) onConnect(sess *pollmux.Session, meta map[string]string) error {
    bs := &brokerSession{Session: sess, Role: meta["role"], Endpoint: meta["endpoint"]}

    if bs.Role == "provider" {
        // 注意这里不再有 RegisterSession —— 会话已经在 store 里，poll 找得到。
        // 拓扑注册由 HandleProvider 在 yamux 建好后调 SetProvider 完成。
        go s.relay.HandleProvider(bs)
    } else {
        s.registry.AddConsumer(bs.Endpoint, bs)
        go s.relay.HandleConsumer(bs)
    }
    return nil
}
```

`OnDisconnect` 是**唯一**的会话结束出口，三条路径（客户端 DELETE、扫描驱逐、应用主动关）都会走到，
并带上原因：

```go
func (s *Server) onDisconnect(sess *pollmux.Session, reason pollmux.DisconnectReason) {
    meta := sess.Meta() // role / endpoint 从这里取，不用回查 registry
    s.registry.Forget(sess.ID, meta["role"], meta["endpoint"])
    s.logger.Info("session ended",
        zap.String("session_id", sess.ID),
        zap.String("role", meta["role"]),
        zap.String("endpoint", meta["endpoint"]),
        zap.String("reason", reason.String()),
    )
}
```

### 清理与停机

```go
// Start：
stopSweeper := pollmux.StartSweeper(store, pcfg, hooks)   // 取代 cleanupLoop

// Stop：
_ = s.httpSrv.Shutdown(ctx)
for _, sess := range store.All() {
    pollmux.CloseSession(store, hooks, sess, pollmux.ReasonServerClose)
}
stopSweeper()   // 幂等，且会等扫描协程退出
```

`stopSweeper()` 返回后保证不再有 `OnDisconnect` 回调，所以停机顺序是确定的 —— 今天的
`close(s.done)` 没有这个保证。

### relay.go 的改动

只有两处，都是类型：

```go
func (r *Relay) HandleProvider(session *brokerSession) {
    yamuxSess, err := pollmux.ClientSession(session) // 取代 yamux.DefaultConfig() 那四行
    ...
}

func (r *Relay) HandleConsumer(session *brokerSession) {
    yamuxSess, err := pollmux.ServerSession(session)
    ...
}
```

`bridgeStream` 完全不动。

---

## 四、Provider 客户端

`Run`（227 行里的 80 行）塌缩成一个 `ReconnectLoop`。`StreamHandler.Handle` 的签名
（`func(stream net.Conn)`）正好就是 `AcceptLoop` 要的 `handle func(net.Conn)`，直接传：

```go
func (c *Client) Run(ctx context.Context) error {
    slogger := slog.New(zapslog.NewHandler(c.logger.Core()))

    loop := &pollmux.ReconnectLoop{
        Connect: func(ctx context.Context) (pollmux.Conn, error) {
            connector := &pollmux.Connector{
                BaseURL:   c.config.BrokerURL,
                AuthToken: c.config.AuthToken,
                Meta: map[string]string{
                    "role":     "provider",
                    "endpoint": c.config.Endpoint,
                },
                PollInterval:       c.config.PollInterval,
                CoalesceWindow:     c.config.CoalesceWindow,
                InsecureSkipVerify: c.config.InsecureSkipVerify,
                Logger:             slogger,
            }
            return connector.Connect(ctx)
        },
        Serve: func(ctx context.Context, conn pollmux.Conn) pollmux.Outcome {
            sess, err := pollmux.ServerSession(conn)
            if err != nil {
                return pollmux.OutcomeTransportFailed
            }
            defer sess.Close()
            return pollmux.AcceptLoop(ctx, sess, conn, c.handler.Handle)
        },
        InitialBackoff: c.config.RetryBackoff,
        Logger:         slogger,
    }
    return loop.Run(ctx)
}
```

手工构造 `http.Transport` / `http.Client` 的那段（`provider/client.go:66-80`）全部删掉。
pollmux 内部建**两个** client：poll 用长超时（`poll_timeout` + 宽限），send 用 15s 短超时。
今天两者共用一个 `Timeout: 0` 的 client，这就是缺陷 A1 —— broker 所在主机被防火墙黑洞掉时，
poll 会永久挂起，客户端永远不知道自己掉线了。

---

## 五、双索引：EndpointRegistry 怎么处理

迁移后会有两份"session id → session"的索引：pollmux 的 `SessionStore`，和
`EndpointRegistry.sessions`（`endpoint.go:32`）。**必须只留一份，否则两者会漂移。**

建议：**删掉 `EndpointRegistry.sessions`，让 store 成为唯一的会话索引**，registry 退化为纯粹的
拓扑表（哪个 endpoint 有 provider、有哪些 consumer、各自的 yamux 会话）。依赖方向变成单向：
pollmux 管生命周期，registry 是从 `OnDisconnect` 更新的派生视图。

具体删改：

| 现有方法 | 处理 |
|---|---|
| `RegisterSession`（218-225） | 删除。`ConnectHandler` 已经注册过了 |
| `GetSession`（312-319） | 删除。`PollHandler` 直接查 store |
| `AllSessions`（378-388） | 删除，改用 `store.All()` |
| `RemoveSession`（272-310） | 改名 `Forget(id, role, endpoint)`，只摘拓扑，**不再删 sessions map** |
| `RemoveProvider`（173-216） | 删掉其中所有 `delete(r.sessions, ...)`；关 consumer yamux 会话的部分保留 |
| `handleStatus`（335-358） | 拓扑数据仍来自 `r.endpoints`；总会话数改用 `store.Len()` |

顺带修掉一个既有问题：`RemoveProvider` 今天会把所有 consumer 从 `r.sessions` 里删掉
（endpoint.go:196-200），而这些 consumer 的**长轮询连接还活着** —— 它们接下来的 poll 会拿到 404，
而不是一个有意义的响应。删掉这段之后，consumer 靠自己的 yamux 会话被关来感知，路径干净了。

---

## 六、Consumer 客户端 —— 唯一需要仔细读的一节

Consumer 不用 `AcceptLoop`（它是**开流**的一端，不接受流），`Serve` 要自己写那个四路 select。
持久 SOCKS5 监听器留在循环外，跟今天一样：

```go
func (c *Client) Run(ctx context.Context) error {
    listener, err := net.Listen("tcp", c.config.Socks5Listen)
    if err != nil {
        return err
    }
    defer listener.Close()

    connQueue := make(chan net.Conn, 64)
    acceptCtx, cancelAccept := context.WithCancel(ctx)
    defer cancelAccept()
    go c.acceptLoop(acceptCtx, listener, connQueue)

    slogger := slog.New(zapslog.NewHandler(c.logger.Core()))

    loop := &pollmux.ReconnectLoop{
        Connect: func(ctx context.Context) (pollmux.Conn, error) { /* 同 provider，role=consumer */ },

        Serve: func(ctx context.Context, conn pollmux.Conn) pollmux.Outcome {
            sess, err := pollmux.ClientSession(conn)
            if err != nil {
                return pollmux.OutcomeTransportFailed
            }
            defer sess.Close()

            dialer := NewTunnelDialer(sess, c.logger)
            socksServer := socks5.NewServer(
                socks5.WithDial(dialer.Dial),
                socks5.WithResolver(&NoopResolver{}),
            )
            serveCtx, cancelServe := context.WithCancel(ctx)
            defer cancelServe()
            go c.serveLoop(serveCtx, socksServer, connQueue)

            select {
            case <-ctx.Done():
                return pollmux.OutcomeShutdown
            case <-conn.TransportFailed():
                return pollmux.OutcomeTransportFailed
            case <-sess.CloseChan():
                // 必须再查一次。yamux 关闭本身分不出"链路断了"和"broker 主动关掉了
                // 我的会话（因为 provider 走了）"，而这两者要相反的反应。
                select {
                case <-conn.TransportFailed():
                    return pollmux.OutcomeTransportFailed
                default:
                    return pollmux.OutcomePeerClosed
                }
            }
        },

        InitialBackoff:  c.config.RetryBackoff,
        PeerClosedPause: 500 * time.Millisecond, // 对应今天 provider 断开时的短暂停顿
        Logger:          slogger,
    }
    return loop.Run(ctx)
}
```

### 退避语义：看起来变了，其实没变

今天 consumer 的重置条件是 `lastFailWasBroker`（`consumer/client.go:108-112`），provider 则是
无条件重置（`provider/client.go:107-108`）。`ReconnectLoop` 采用后者 —— **每次连接成功都重置退避**。

这两者在本项目里**行为完全等价**：backoff 只在把 `lastFailWasBroker` 置为 true 的那些分支里推进
（`consumer/client.go:103-104`、`132-133`，以及末尾 `providerDisconnected==false` 的 else 分支），
所以不存在"backoff 已推进但下次成功连接时 `lastFailWasBroker` 为 false"的路径。可以放心简化。

它的代价要知道：**连上就立刻断的抖动链路会稳定在 `InitialBackoff` 反复重试，不会升级。**
这是为了真实故障恢复后能立刻恢复速度而做的取舍 —— 升级需要一条"连接活多久才算恢复"的规则，
而这个判断循环本身没有依据做。

### ⚠️ provider 离开时，consumer 会看到 TransportFailed 而不是 PeerClosed

这是迁移中**唯一一处真正的行为变化**，需要专门确认。

链路是这样的：broker 的 `RemoveProvider` 通过 `ys.Close()` 关掉每个 consumer 的 yamux 会话
（endpoint.go:213-215）。而 `yamux.Session.Close()` **会连底层 conn 一起关**
（`yamux@v0.1.2/session.go:289`）—— 这里的底层 conn 就是 consumer 的 pollmux `Session`，
也就是那条隧道本身。隧道一关，consumer 挂着的那个 poll 就拿到 **410**，于是
`TransportFailed()` 触发。

- **今天**：broker 的 poll 在 EOF 时回 **204**（这就是缺陷 A5），consumer 看不出隧道已死，
  只看到 yamux CloseChan，再查 `TransportFailed()` 没触发 → 判定"provider 走了" → 500ms 重连。
- **迁移后**：EOF 正确地回 **410** → `TransportFailed()` 触发 → 判定 `OutcomeTransportFailed`
  → 走退避而不是 500ms 停顿。

**这不是 pollmux 的 bug，而是 A5 修复暴露出来的既有事实**：那条隧道确实已经被 broker 关掉了。

处理办法，按推荐顺序：

1. **接受它，并把 consumer 的 `InitialBackoff` 设成 500ms。** 因为每次成功连接都会重置退避，
   provider 每次离开的代价就是恒定 500ms，永不升级 —— 与今天的时序完全一致。**推荐这个。**
2. 让 broker 不要连带关掉 consumer 的隧道。`GoAway()` 不关底层 conn，但对端只在下一次 `Open()`
   时才会看到 `ErrRemoteGoAway`（`session.go:165`），`CloseChan()` 不触发 —— 所以它**不是**现成的
   替代品，还得再加一层应用层信令。成本远大于收益。
3. 什么都不做：provider 抖动时 consumer 重连从 500ms 变成 1s（`DefaultInitialBackoff`），
   同样不会升级。功能无碍，只是慢一点。

`OutcomePeerClosed` 那条分支在这个拓扑里因此基本不会触发。**保留它** —— 它是正确的，只是这个
拓扑刚好不产生这个事件；换成 broker 用别的方式驱逐 consumer 时它就有用了。

---

## 七、配置文件

新增（都有默认值，不填也能跑）：

```yaml
# broker.yaml
tunnel:
  poll_timeout: 30s
  session_timeout: 60s      # ← 从 5m 改成 60s，见下
  poll_buffer_size: 262144  # 新增，256KB。旧代码硬编码 64KB
  max_send_bytes: 1048576   # 新增，1MB。旧代码硬编码在 server.go:252
  high_water_warn: 33554432 # 新增，0 关闭
```

**`session_timeout` 从 5 分钟改成 60 秒**是 A3 的核心。今天 5 分钟是因为不敢调小 —— 一个正好处在
两次 poll 之间的健康客户端会被误杀。pollmux 加了 `PollInFlight()` 判据：有 poll 挂着就绝不驱逐
（挂着的 poll 意味着客户端此刻正握着一条 TCP 连接）。有了这个判据，超时可以压到两个轮询周期，
最坏检测时延从 5 分钟降到约一个轮询周期。

客户端侧的 `poll_interval`、`session_timeout` 等**不再需要在两边各配一份** —— 服务端在 connect
时下发权威值，客户端只能更保守。客户端还会自检：如果 `poll_timeout + poll_interval >=
session_timeout`，直接在 connect 时报错退出，而不是带着这个隐患跑起来。

---

## 八、现有测试的影响

- `internal/broker/server_test.go:221-237`（"Authorized request works normally"）**会失败**：
  它用 query 参数、空 body 发 connect 并断言 200，迁移后得到 426。改成发 JSON body：
  ```go
  body := strings.NewReader(`{"protocol_version":1,"meta":{"role":"consumer","endpoint":"test"}}`)
  ```
- 同文件的 401 / 302 用例**不受影响** —— 它们在 `AuthMiddleware` 就被拦下，从没走到 handler。
- 没有任何测试文件直接 import `internal/transport`，删包不会引发连锁修改。

---

## 九、迁移中会顺带修掉或暴露的既有问题

| | 位置 | 说明 |
|---|---|---|
| A1 | `provider/client.go:66-80`、`consumer/client.go:221-234` | `Timeout: 0` + `ResponseHeaderTimeout: 0`：broker 被黑洞时 poll 永久挂起，客户端不知道自己掉线 |
| A3 | `server.go:369`、`386` | 清理每分钟一跑、超时 5 分钟，且没有"有 poll 挂着就别驱逐"的判据 |
| A5 | `server.go:298-302` | 会话已关闭（EOF）与超时无数据共用 204 |
| B1 | `server.go:285` | poll 读缓冲硬编码 64KB，是下行吞吐的瓶颈 |
| — | `server.go:252` | 请求体上限硬编码 1MB，且超限静默截断而非报错 |
| — | `endpoint.go:196-200` | `RemoveProvider` 把还活着的 consumer 从索引里删掉，它们的下一次 poll 得到 404 |
| — | `relay.go:234`、`243` | `interface{ CloseWrite() error }` 类型断言是**死代码** —— yamux v0.1.2 的 `*yamux.Stream` 没有 `CloseWrite`，半关闭就是 `Close()`。断言永远失败，所以那两个方向从来没有正确地发过 EOF 信号 |

最后一条值得单独查证：`stream.go` 里 `*Stream` 的方法只有 `Read`/`Write`/`Close`/`SetDeadline`
等，没有 `CloseWrite`。迁移时顺手把那两处死代码改掉或删掉。

---

## 十、验证清单

迁移完成后，除了跑通现有测试，这几条要专门验证：

1. **A1**：给 broker 主机加一条 `iptables -j DROP`，断言客户端在 `poll_timeout + 宽限`（约 40s）
   内检测到传输失败并开始重连 —— 而不是永久挂起。
2. **A3**：拔掉客户端的网线，断言 broker 在约 60s（而非 5 分钟）内驱逐会话，且
   `OnDisconnect` 带 `ReasonEvicted`。
3. **A5**：`DELETE /tunnel/{id}` 掉一个 provider 会话，断言对端在秒级重连，而不是等到自己的超时。
4. **B1**：`tc netem` 注入 100ms RTT，对比 `poll_buffer_size` 64KB 与 256KB 的下行吞吐，
   预期约 ×4。这个数字将来做上下行流式化时是基线。
5. **停机**：`Stop(ctx)` 之后断言所有 `OnDisconnect` 都带 `ReasonServerClose`，且
   `stopSweeper()` 返回后不再有回调。
6. **版本协商**：用旧版客户端连新 broker，断言得到 426 且日志里能一眼看出是版本不匹配。

---

## 附：pollmux 侧已有的对应测试

pollmux 仓库里 `application_test.go` 与 `relay_topology_test.go` 已经按本项目的用法建了模型
（三角色校验、endpoint 注册表、每种请求都过鉴权中间件、provider 离开后 consumer 的恢复时序、
TLS 拓扑），共 132 个测试。迁移中如果撞到 API 不顺手的地方，先去看这两个文件里有没有对应形状 ——
上面 §三 到 §六 的每段代码都能在那里找到可运行的原型。
