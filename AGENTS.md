# FST AGENTS.md — AI 代码开发规范

面向在本仓库写代码的 AI / 协作者。先读本文件，再改代码。细节以配套文档为准，不要把本文件扩成第二份架构说明书。

配套文档（改行为前按需查阅，不要凭记忆编造协议）：

| 文档 | 用途 |
|---|---|
| [README.md](README.md) | 构建 `bin/fst.exe`、中继打包、代理运行前提 |
| [docs/开发文档.md](docs/开发文档.md) | 技术栈、模块、协议、API/事件、已实现与缺口（开发基准） |
| [docs/项目架构分析报告.md](docs/项目架构分析报告.md) | 启动顺序、目录职责、网络与存储细节 |
| [docs/开发计划.md](docs/开发计划.md) | 演进过程与阶段决策 |
| [docs/代理故障分析与改进报告.md](docs/代理故障分析与改进报告.md) | TUN/DNS/SOCKS5 的已知坑与现行语义 |
| [docs/回归清单.md](docs/回归清单.md) | IPv6 双栈、真实 NAT、受限中继、局域网 SOCKS5、离线队列与群聊的手工回归步骤 |
| [部署.md](部署.md) | fst-relay 部署 |

---

## 1. 项目是什么

FST 是 **无登录、无中心服务器** 的局域网 / 跨网络 P2P 桌面应用。当前产物：**Windows 桌面端**（Wails 嵌入 Vue）+ **Linux 无头中继** `fst-relay`。每个桌面实例既是 UI，也是完整 libp2p 节点。

硬约束（违反即错误实现）：

- **身份 = libp2p PeerID**（Ed25519，`identity.key`），不用 MAC/IP 当主键。
- **单端口 8787**（TCP + QUIC/UDP 同号）。聊天、文件、代理全部走 libp2p stream 多路复用，**禁止为新功能另开端口**（本地回环 SOCKS5 除外，见 §3）。
- **文件字节永不经过 JS**：前端只拿本地路径；I/O、哈希、进度全在 Go。
- 聊天 / 传输 / 设备档案 **只存本机 SQLite**，节点间不同步状态。
- 应用层 **不自研加密**，依赖 libp2p 安全通道（Noise/TLS、QUIC）。
- 用户可见文案与 API `message` 用 **中文**；协议字段、JSON key 用 **snake_case 英文**。

---

## 2. 技术栈

不要引入下表之外的新依赖，除非现有库明显不够且调用方明确要求。

**Go**（`module fst`，go 1.26+）

| 层次 | 选型 |
|---|---|
| 桌面壳 | Wails v3 `v3.0.0-beta.29`（窗口、托盘、对话框、拖放、通知、单实例） |
| P2P | go-libp2p v0.50 + kad-dht v0.42（mDNS、AutoNATv2、DCUtR、Circuit Relay、私有 `/fst` DHT）+ go-libp2p-pubsub（GossipSub 群聊） |
| 代理 | tun2socks v2.6 + gVisor netstack + WireGuard TUN（Windows 需 `wintun.dll`） |
| 存储 | `modernc.org/sqlite`（纯 Go，无 CGO）+ 标准库 `database/sql` |
| 配置 | `go.yaml.in/yaml/v3` |
| HTTP | 标准库 `net/http`（无 gin/echo） |
| 日志 | `log/slog` → 数据目录 `fst.log` |

**前端**（`front/`，Art Design Pro 裁剪）

Vue 3.5 `<script setup>` + TypeScript、Vite 8、Element Plus 2.13、Tailwind CSS v4、Pinia 3、vue-router 5、axios（`src/utils/http`）、`@wailsio/runtime` **只用 Events，不生成 Wails TS 绑定**、pnpm。

**通信**

- UI → Go：同源 REST `/api/fst/*`，信封 `{code, message, data}`。业务错误 = **HTTP 200 + code 400**（模板靠 `message` 提示）。
- Go → UI：`app.Event.Emit`；前端只在 `front/src/store/modules/fst.ts` 的 `init()` 订阅。

---

## 3. 功能地图（已有 vs 不要擅自做）

已实现，改动前先搜现有代码：

- 局域网 mDNS 发现、HELLO peer-exchange、在线/离线、信任开关、手动连接（multiaddr / PeerID+中继 / 仅 PeerID+DHT）
- 点对点聊天：落库、ACK、去重、未读、分页、富文本（≤32000 runes）；消息类型 text/image/file，图片气泡走只读媒体端点（`GET /api/fst/media/{id}`，唯一直接返回原始字节的 API：仅 done 图片、不套信封、不提供写入/Range 之外的能力）、文件卡片复用传输进度；2 分钟内撤回；头像随 HELLO 同步
- 离线消息：对端离线/网络失败落 `queued`（气泡「待发送」，不弹错误），15s 扫描 + 15s→10min 指数退避（20 次转 `failed`），`peer:online` 立即冲刷，`POST /messages/{id}/retry` 手动重投；已撤回消息不入队
- 群聊：`internal/room` 用 GossipSub 复用 8787；局域网房间 + 分组房间（topic 由密钥 SHA-256 派生，不写明文），仅文本、本机 `room_messages` 存档按 ID 去重，无离线补投；局域网房间只接受直连非 circuit 的私网/回环发送者
- 文件传输：多选/拖放、确认弹窗、信任/自动接收、并发 1–8、`.fstpart` 断点续传、整文件 SHA-256、取消/重试/打开位置；全局限速、>1000 文件自动分批、`prehash` 秒跳过已有文件、失败后继续整批、空目录还原
- 跨网络：静态中继 AutoRelay、私有 DHT、中继 Directory 互发现、打洞成功后丢掉 circuit
- 无头中继：Directory、DHT Server、高容量 ConnManager；`-exit` + `-exit-allow` 出口，**空白名单 fail-closed**；`-group` 分组隔离
- 中继分组：终端 `config.yaml: group`（只发给配置中继），中继只把同组终端互相介绍；未分组 = 旧行为；`-group-file` 组表热加载（10s 轮询、原子换表、删组/过期即吊销），终端保存分组密钥即时重连中继并切换群聊房间
- TUN 代理：`split`（CIDR + 域名 fake-IP，DNS `/32` 进 TUN + 未命中域名经出口解析）/ `global`；IPv4 TCP+UDP；局域网/DNS 绕行；本地 `127.0.0.1` SOCKS5 入站（与 TUN 共用出口 Client）；fake-IP 表有 LRU 上限
- 代理加固（默认关闭）：出口 PeerID 白名单 / 令牌 / 分组三层取交集、按组出口槽位；SOCKS5 入站由 `proxy.socks_enable` 开关（缺省开启），可加 RFC1929 + 来源 CIDR + 并发上限；`proxy.ipv6=true` 时 TUN 增加 IPv6 栈、fake AAAA 与 v6 路由，失败自动降级 IPv4
- 文件夹传输：递归目录（单批 ≤1000 文件，超出自动拆批）→ `FileInfo.RelPath` → 接收端 `safeRelPath` 清洗后按目录树落盘，逐文件续传；空目录单独下发
- 工程地基：`user_version` 版本化迁移、离线图标（`@iconify-json/*` 本地注册）、四个业务页 zh/en 词条
- 桌面：托盘、关闭三态、通知、开机自启、单/多实例、配置原子写

明确 **未做**，未经用户点名不要开做：群聊补投/成员名单（群聊本体已做，仅文本 + 本机历史）、多设备消息同步、中继密钥在线轮换（组表热加载/过期吊销已做）、前端模板大瘦身、NSIS/自动更新、剪贴板/屏幕共享。IPv6 代理与出口令牌/分组已实现但默认关闭，属实验能力，不要当作稳定基线。缺口清单见 [docs/开发文档.md](docs/开发文档.md) §8。

---

## 4. 目录与职责（改代码落在对的包）

```
main.go / instance.go / config.go   启动、数据目录、config.yaml
internal/api     UI 适配器，不放业务状态机
internal/node    libp2p Host、发现、HELLO、在线、拨号；不管聊天文本/文件字节
internal/chat    聊天收发 + 落库 + ACK + 离线队列（queued/退避/重试）
internal/room    GossipSub 群聊：自动房间、topic 派生、LAN 校验、本机历史；不做群消息补投
internal/transfer 文件/文件夹 offer、流、续传、校验、进度、safeRelPath
internal/store   SQLite、user_version 迁移（migrate.go）、查询，无协议逻辑
internal/proto   协议 ID、信封、分帧；改协议先改这里
internal/proxy   TUN / 路由 / fake-IP DNS（LRU）/ 出口解析 / SOCKS5 / 出口 Client
internal/e2e     真实 libp2p 多节点测试
cmd/fst-relay    无头中继入口
front/src/api/fst        唯一业务 HTTP 封装
front/src/types/api/fst.d.ts  与 Go JSON 对齐的类型
front/src/store/modules/fst.ts  唯一 Wails 事件订阅点
front/src/views/fst/{device,chat,transfer,settings}
front/src/locales/langs/{zh,en}.json  fst.* 业务词条
```

- 不要在 `front/src/components/core` 里塞 FST 业务（那是模板）。
- 不要为同一件事新增第 N 个 store / 第 N 个 API 文件。
- SQLite 用 `internal/store/migrate.go` 的**版本化迁移**：改 schema 只追加新版本（禁止改已发布的旧版本语句），`Open` 会在单事务内升级到最新 `user_version`。

---

## 5. 代码怎么写

默认是高效而非炫技（YAGNI）：标准库能做就不要新包；能一行就一行；少文件、少抽象。有意的捷径用 `ponytail:` 注释写明上限和升级路径。

**不懒的部分**：信任边界校验、防止数据丢失的错误处理、安全（路径穿越、私网目标、白名单）、代理路由环路、硬件/OS 与文档不一致时的校准、以及用户明确要求的行为。

### 5.1 Go

- 包名短、小写；导出标识用 Go 习惯，JSON 用 `json:"snake_case"`。
- 错误返回给 UI 的用中文完整句子（已有风格：`出口繁忙，请稍后重试`）；内部 `fmt.Errorf` 可中英，但要 `%w` 包装。
- 控制帧：`proto.WriteFrame` / `ReadFrame`（uvarint + JSON）。文件内容与 SHA-256 trailer **不进 JSON**。
- 代理出口禁止环回 / 私网 / 链路本地 / 组播；空白名单拒绝所有人。
- 平台文件用 build tag：`routes_windows.go` / `linux` / `darwin`，不要在通用文件里 `runtime.GOOS` 堆命令。
- 不引入 ORM、不引入第二套 HTTP 框架、不把业务写进 `main.go`（`main` 只装配）。

### 5.2 前端

- `<script setup lang="ts">`；四个业务页的文案走 `t('fst.*')`（`src/locales/langs/{zh,en}.json`），中文为默认；新增文案必须同时补 zh/en。
- 类型放 `front/src/types/api/fst.d.ts` 的 `Fst.*`，改 API 时前后端一起改。
- 列表/分页走模板 `useTable` + 信封，不要另造 axios 实例。
- 文件选择用 Go `pick-files` / 拖放路径，禁止把文件内容 POST 到 `/api`。

### 5.3 测试与构建

- 非平凡逻辑留 **一个** 可跑的检查：`internal/xxx/*_test.go` 或 `internal/e2e`，不要上测试框架/fixtures。
- 代理测试 **不碰真实网卡**；e2e 用进程内 libp2p。
- 改完至少：`go test ./internal/...`；动到 `front/` 再 `cd front && npx vue-tsc --noEmit`。
- 桌面产物：`wails3 build` → `bin/fst.exe`（先停掉正在运行的 `fst`）。不要主动 `git commit` / `push`，除非用户要求。

---

## 6. 函数注释（强制）

**新增或改动到的每个函数 / 方法必须有注释**，包括未导出符号。注释必须写清：作用、参数名与类型、返回值与类型；有副作用、错误语义、超时/并发上限时一并写。

不要写复述代码的废话（禁止 `// i++  // 把 i 加一`）。接口实现要写明实现的是哪个接口。

测试函数 `TestXxx` 至少一行说明测什么；表格驱动用例的字段靠字段名即可。

改到 **没有注释的旧函数** 时，顺手补上，不要为「统一风格」全仓库刷一遍注释。

### 6.1 Go（godoc + 参数/返回块）

放在函数正上方，用中文，godoc 第一句以函数名开头。

```go
// WriteFrame 把 v 编码成一帧「uvarint 长度 + JSON」写入 w。
// 帧大于 MaxFrame 时拒绝，避免控制流被大 payload 拖垮。
//
// 参数:
//   - w: io.Writer — 目标流（libp2p network.Stream 或测试 buffer）
//   - v: any — 可 json.Marshal 的控制结构（Msg / FileHeader / ProxyReq / ProxyResp）
//
// 返回:
//   - error — 序列化失败、超长、或底层 Write 失败；成功为 nil
func WriteFrame(w io.Writer, v any) error

// DialUDP 经出口打开一条 UDP 会话（握手后为 2 字节大端长度前缀数据报）。
//
// 参数:
//   - ctx: context.Context — 取消/截止；握手阶段写入 Stream deadline
//   - address: string — "host:port"，host 为 IPv4 或域名（域名在出口解析）
//
// 返回:
//   - net.PacketConn — 可 ReadFrom/WriteTo 的封装；Close 关闭底层 stream
//   - error — 拨号、开流、对端拒绝（含白名单）或握手失败
func (c *Client) DialUDP(ctx context.Context, address string) (net.PacketConn, error)
```

无参数、只返回 error 的也要写返回块。闭包/函数类型字段在类型注释里写清签名，不必给每个字面量再写一遍。

### 6.2 TypeScript / Vue

用 JSDoc，`@param` 带类型，`@returns` 写清 Promise 结果。

```ts
/**
 * 向指定设备发送一条聊天消息（落库与 ACK 由 Go 完成）。
 * @param peer_id 对方 PeerID（string）
 * @param content 正文（string，后端限制 ≤8000 runes）
 * @returns Promise<{ message: Fst.Message; error?: string }> 已落库的本地消息；对端失败时带 error
 */
export const fetchSendMessage = (peer_id: string, content: string) => { /* ... */ }
```

```ts
/**
 * 保存设置页表单（跨网络与代理写入 config.yaml，重启生效）。
 * @returns Promise<Fst.Settings> 服务端回读后的完整设置
 */
const save = async () => { /* ... */ }
```

Vue 组件内 `function` / 具名 `const fn = () =>` 都要注释；模板里纯展示的 computed 一行说明派生什么即可。

### 6.3 例外（仅这些可以不写满参数块）

- `node_modules/`、Wails/Vite 生成物、`front/src/components/core` 里未改动的模板代码
- 空接口方法体里的 `panic("unimplemented")` 占位（应尽快删掉而不是注释）
- 标准库接口的薄转发（如 `func (a p2pAddr) Network() string`）可用一行：`// Network 实现 net.Addr，固定返回 "libp2p"。`

---

## 7. 改协议 / 配置 / UI 时的同步清单

| 改动 | 必须同时改 |
|---|---|
| `/fst/msg` 字段或新 Type | `internal/proto`、收发两端、e2e 或单测、必要时 `fst.d.ts` |
| `/api/fst/*` | `internal/api`、`front/src/api/fst`、`front/src/types/api/fst.d.ts` |
| Wails 事件名或载荷 | Go `Emit`、`front/src/store/modules/fst.ts` |
| `config.yaml` / proxy 字段 | `config.go`、`main.go` Hooks、settings API、设置页 |
| 出口代理语义 | `internal/proxy` 测试 + [docs/开发文档.md](docs/开发文档.md) 代理小节（行为变了才改文档） |

`config.yaml` 与跨网络开关 **重启才生效**，不要假装热更新 Host。

---

## 8. 禁止事项

- 为代理/聊天/文件再监听一个公网端口
- 把文件内容读进前端再上传
- 出口允许 `127.0.0.1` / RFC1918 / link-local（测试 mock 拨号除外）
- 空 `-exit-allow` 却放行所有 PeerID
- 新增 Wails bind 服务替代 `/api/fst`
- 把中继 Directory 信任来源扩到「任意节点发的 HELLO」
- 用 IP/MAC 替换 PeerID 作为设备主键
- 无关重构、顺手删模板组件、加新依赖「以后可能有用」
- 修改用户未点名的 plan 文件、大段重写未触及的模块
- 提交 git（除非用户明确要求）

---

## 9. 动手前的最短检查

1. 这件事是否已有函数可扩，而不是新包？
2. 是否破坏单端口 / PeerID / 文件不经 JS / 出口 fail-closed？
3. 新函数注释是否含作用、参数类型、返回值？
4. 非平凡逻辑有没有一个会失败的测试？
5. 需要的话：`go test ./internal/...`，前端则 `vue-tsc`。
