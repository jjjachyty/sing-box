# sing-box Fork 修改说明

本仓库是 [sagernet/sing-box](https://github.com/sagernet/sing-box) 的定制 fork，服务于机场面板（airport_vpn）节点端。

- 上游基线：`2ee20d0`（"Bump version"，2026-06-23，`origin/testing`）
- 工作分支：`vpn-speedlimit`（推送到 `vpn-speedlimit` remote，即 github.com/jjjachyty/sing-box）
- 构建 tag：`with_quic,with_dhcp,with_wireguard,with_utls,with_clash_api`

```bash
CGO_ENABLED=0 go build \
    -ldflags "-s -w" \
    -tags "with_quic,with_dhcp,with_wireguard,with_utls,with_clash_api" \
    -o sing-box ./cmd/sing-box
```

## 改动清单

### 1. 按用户限速（commit 67a1978）

| 文件 | 说明 |
|---|---|
| `common/ratelimiter/manager.go` | **新增**。按用户名（= 用户 UUID）的令牌桶限速器，`SetLimit(name, bytesPerSec)`（≤0 删除）、`WrapConn`/`WrapPacketConn` |
| `experimental/clashapi/speedlimit.go` | **新增**。`GET/POST /speedlimit`、`DELETE /speedlimit/{name}` |
| `experimental/clashapi/server.go` | Server 增加 `rateLimiter` 字段，创建时 `service.MustRegister[*ratelimiter.Manager]` 注册进 ctx |
| `protocol/{vmess,vless,trojan}/inbound.go`、`protocol/shadowsocks/inbound_multi.go`、`protocol/hysteria2/inbound.go` | 认证拿到 user 后 `rateLimiter.WrapConn(conn, user)`（注意：**UDP 路径未包**，限速只对 TCP 生效） |

限速 key 是 sing-box 的 user name，节点侧把 user name 设为用户 UUID，实现真正的按用户限速（多连接共享一个桶）。

### 2. 运行时用户管理（commit 3785ac6）

| 文件 | 说明 |
|---|---|
| `adapter/user_manager.go` | **新增**。`UserEntry{Name, UUID, Password, AlterId, Flow}` 和 `UserUpdatableInbound` 接口 |
| `experimental/clashapi/users.go` | **新增**。`POST /users`（全量替换某 inbound 用户）、`POST /users/close`（按用户踢连接） |
| `protocol/{vmess,vless,trojan,hysteria2}/inbound.go`、`protocol/shadowsocks/inbound_multi.go` | 实现 `UpdateUsers([]adapter.UserEntry)`（hysteria1 / tuic 未实现） |
| `adapter/ssm.go`、`service/ssmapi/user.go` | `ManagedSSMServer.UpdateUsers` 签名由 `(users, uPSKs []string)` 改为 `([]UserEntry)` |

⚠️ 已知 bug：`users.go` 的 `/users/close` 用 `service.FromContext[adapter.ConnectionTracker]` 取 tracker，但 traffic manager 是按具体类型注册的，**永远取到 nil**。内置 agent 直接调 `trafficManager.CloseConnectionsByUser` 不经过此接口，暂不受影响。

### 3. 按用户流量统计（commit 8725f92 等）

| 文件 | 说明 |
|---|---|
| `common/trafficcontrol/manager.go` | 新增 `userTraffic` 累加器；`UserTraffic()`（含在途）、`OnlineUsers()`、`CloseConnectionsByUser(user)` |
| `experimental/clashapi/traffic.go` | 新增 `GET /traffic/users` → `{users: {uuid: [up, down]}, online: n}` |

### 4. ConnectionTracker 接口扩展

| 文件 | 说明 |
|---|---|
| `adapter/router.go` | `ConnectionTracker` 接口新增 `CloseConnectionsByUser(user string) (int, error)` |
| `experimental/v2rayapi/stats.go` | 空实现（接口适配） |

### 5. 连接数 / 设备数限制（连接建立时强制）

| 文件 | 说明 |
|---|---|
| `common/trafficcontrol/manager.go` | `UserLimit{MaxConnections, MaxDevices}`；`SetUserLimits(map[string]UserLimit)` 全量替换；`allowConnection(user, sourceIP)`；join/leave 维护每用户连接数（`userConns`）和每用户源 IP 计数（`userDevices`） |
| `common/trafficcontrol/tracker.go` | `RoutedConnection` / `RoutedPacketConnection` 在 join 前检查，超限直接 `conn.Close()` 并返回（不 join，不产生假记录） |

设备数 = 同一用户活跃连接的去重源 IP 数，IP 最后一条连接关闭即释放名额。限制数据由内置 agent 从面板 `/internal/node/config` 周期拉取后推入。

### 6. 内置节点 agent（`agent/` 包，取代外部 agent 进程）

| 文件 | 说明 |
|---|---|
| `agent/api/types.go`、`agent/api/client.go` | 与中心面板的全部 HTTP 交互（register / heartbeat / blacklist / node config / traffic report），只放在这个目录 |
| `agent/config.go` | node.yaml 加载 |
| `agent/certs.go` | Reality X25519 密钥对、自签 RSA 证书 |
| `agent/options.go` | 由节点配置内存构造 `option.Options`（`json.UnmarshalExtendedContext` 解码，与 cmd_run.go 同路径）。`experimental.clash_api` 保留但 `external_controller` 为空——触发 trafficManager/ratelimiter 创建但不监听端口 |
| `agent/sysinfo.go` | /proc CPU/内存采集 |
| `agent/agent.go` | `Run()`：注册节点 → 拉配置 → `box.New` → 四个循环（heartbeat 30s、blacklist 30s、config 60s、traffic report 60s），全部进程内调用（UpdateUsers / CloseConnectionsByUser / SetLimit / SetUserLimits / UserTraffic） |
| `cmd/sing-box/cmd_agent.go` | cobra 子命令：`sing-box agent -c node.yaml` |

节点部署只需这一个二进制 + node.yaml，不再需要 config.json、SIGHUP 重载、clash API 端口和独立 agent 进程。

## 合并上游更新流程

```bash
cd sing-box
git fetch origin                    # origin = github.com/sagernet/sing-box
git checkout vpn-speedlimit
git merge origin/testing            # 或 rebase，merge 更省心
# 解决冲突后：
go build -tags "with_quic,with_dhcp,with_wireguard,with_utls,with_clash_api" ./...
```

### 冲突热点（按概率排序）

1. **`protocol/{vmess,vless,trojan,hysteria2}/inbound.go`、`protocol/shadowsocks/inbound_multi.go`** — 上游最常动的文件。fork 在每个 inbound 加了 `UpdateUsers` 和 `WrapConn` 钩子，合并时逐函数核对，确保认证后 `metadata.User = user` 的赋值和 WrapConn 不被冲掉。
2. **`common/trafficcontrol/manager.go` / `tracker.go`** — fork 加了用户流量、限制执行、踢线。上游若重构 tracker 生命周期（join/leave），fork 的计数挂载点要跟着挪。
3. **`adapter/router.go` / `adapter/ssm.go` / `adapter/user_manager.go`** — 接口变更点。上游若也改了 `ConnectionTracker` 或 shadowsocks 用户管理签名，需要手工合并接口定义。
4. **`experimental/clashapi/server.go`** — fork 加了 rateLimiter 字段和注册。上游改 Server 结构或路由注册时易冲突。
5. **`service/ssmapi/user.go`、`experimental/v2rayapi/stats.go`** — 纯接口适配，冲突时按新接口重新适配即可。

fork 独有的文件（`common/ratelimiter/`、`agent/`、`experimental/clashapi/{users,speedlimit}.go`、`adapter/user_manager.go`、`cmd/sing-box/cmd_agent.go`）不会冲突，但它们依赖的上游接口若被删改，编译会立刻报错——**合并后必须全量编译**，接口破坏基本都能在编译期发现。

### 合并后验收清单

- [ ] `go build -tags "..." ./...` 通过
- [ ] `sing-box agent -c node.yaml` 能注册节点、拉起 inbound
- [ ] 限速生效（ratelimiter 仍在 ctx 注册、协议 inbound 的 WrapConn 还在）
- [ ] 热更新用户生效（UpdateUsers 断言路径还在）
- [ ] 连接数/设备数限制生效（tracker.go 拦截未被重构掉）

## 已知遗留

- UDP 不限速（各协议 `newPacketConnectionEx` 未包 `WrapPacketConn`）。
- 心跳 `reload` 指令只做热应用（用户/限速/限制），面板改端口或协议类型需重启进程。
- `/users/close` HTTP 接口的 ConnectionTracker 类型 bug（见上文第 2 节）。
- 中心 API 的 `/internal/*` 端点服务端无认证（面板侧问题，不在本仓库）。
