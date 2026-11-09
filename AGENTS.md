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
| [docs/移动端技术选型评估.md](docs/移动端技术选型评估.md) | 移动端路线对比、技术卡点、分期计划（P0–P2） |
| [phone/README.md](phone/README.md) | 移动端架构、Go 核心封装约定、AAR/APK 构建步骤 |
| [部署.md](部署.md) | fst-relay 部署 |

---

## 1. 项目是什么

FST 是 **无登录、无中心服务器** 的局域网 / 跨网络 P2P 应用。当前产物：**Windows 桌面端**（Wails 嵌入 Vue）+ **Linux 无头中继** `fst-relay` + **Android 客户端**（原生 Compose 外壳 + 同一份 Go 核心）。每个实例既是 UI，也是完整 libp2p 节点；`fst serve` 可把同一核心以 Web 形式跑起来。

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

**移动端**（`phone/`）

| 层次 | 选型 |
|---|---|
| 外壳 | Kotlin 2.0.21 + Jetpack Compose（Material 3）、AGP 8.7.3、minSdk 26 / targetSdk 35、arm64-v8a |
| 核心 | `phone/core`（Go 门面）+ `phone/daemon`（本机 HTTP/SSE 守护进程），编译为 `libfstcored.so` 随 APK 分发，**复用 node/chat/transfer 原码** |
| 边界 | 跨语言只传 JSON 字符串与基础类型；\`127.0.0.1\` 回环 + \`X-FST-Token\` 鉴权；事件走 SSE |
| 发现 | 桌面端 `internal/lanmdns`（Go mDNS）；**Android 端由系统 `NsdManager` 负责**（`data/Nsd.kt`），解析出的 multiaddr 经 `POST /discovered` 注入核心。服务实例名统一为 `FST-<完整 PeerID>`：TXT 被 ROM 吞掉时，双方都靠实例名 + SRV + A 记录兜底还原对端（`Nsd.fallbackAddr` / `lanmdns.fallbackInfo`） |
| 常驻 | 前台服务（`dataSync` 类型）+ `MulticastLock`；核心进程放 `jniLibs` 以取得 execve 权限 |

移动端核心的构建有两个**硬约束**（`phone/scripts/build-core.sh` 已内置自检）：

- **必须静态、非 PIE**：`-buildmode=pie` 会让 Go 写入 `PT_INTERP=/lib/ld-linux-aarch64.so.1`（glibc 路径），Android 无此路径 → `execve` 返回 ENOENT → 进程静默不启动（App 能开、8787 无监听、mDNS 零报文）。默认 `buildmode=exe` 产出 ET_EXEC，无 `.interp`，内核直接加载。
- **不要改用 `GOOS=android`**：它给出正确的 `/system/bin/linker64`，但会启用 `wlynxg/anet` 的 Android 分支（`//go:linkname net.zoneCache`），Go 1.27 已移除该符号，链接失败且上游无修复。

移动端还有两个**平台级硬约束**，改动前必读：

- **回环 HTTP 必须配 `network_security_config`**：targetSdk ≥ 28 默认禁明文，且 API 37 之前回环不豁免；漏配时 `/health` 恒失败，界面只显示「核心健康检查超时」。只对 `127.0.0.1/localhost/::1` 放行，不要用 `usesCleartextTraffic="true"` 全量放开。
- **Go 的 `net.Interfaces()` 在 Android 11+ 不可用**（`NETLINK_ROUTE` 被禁，`netlinkrib: permission denied`）。因此移动端核心必须 `DisableMDNS: true`，发现交给 `NsdManager`；不要试图在 `phone/` 里自己 bind 5353（系统 `mdnsd` 占用，且 `zeroconf` 未开 `SO_REUSEADDR`）。
- **本机 IPv4 必须由 Kotlin 传进核心**（同一个限制的另一面）：libp2p 展开监听地址后只剩 `127.0.0.1`，若原样广播，对端按它拨号会连到自己（peer id mismatch），症状是「能发出去、收不到」「手机之间互相发现却连不上」。启动用 `FST_LAN_IP`，运行期用 `POST /lanip` → `core.SetLANIP` → `node.Config.AnnounceIP`，由 `AddrsFactory` 把回环改写成真实地址。桌面端不要设置它。
- **跨语言 JSON 不得出现 `null` 数组**：Kotlin 的 `kotlinx.serialization` 把 `null` 当作类型错误（`Expected start of the array '['`），Go 的 nil 切片默认编码成 `null`。所有返回给 `phone/` 的列表字段要么是 `[]`，要么在 Kotlin 侧有默认值 + `coerceInputValues = true`。

**不要**引入 Wails v3 移动端实验通道，也不要引 Kotlin/Swift 版 libp2p 重写：jvm-libp2p 的 mDNS/relay 仍是原型级，协议会分叉。理由见 [docs/移动端技术选型评估.md](docs/移动端技术选型评估.md)。

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
main.go                          唯一入口：桌面 GUI / fst serve 子命令分发
internal/bootstrap               装配核心运行时（Assemble）与数据目录、config.yaml、日志
internal/api     UI 适配器，不放业务状态机
internal/node    libp2p Host、发现、HELLO、在线、拨号；不管聊天文本/文件字节
internal/lanmdns mDNS 广播/浏览：标准 _fst._tcp + 历史 fst 双注册，发现结果去重
internal/chat    聊天收发 + 落库 + ACK + 离线队列（queued/退避/重试）
internal/room    GossipSub 群聊：自动房间、topic 派生、LAN 校验、本机历史；不做群消息补投
internal/transfer 文件/文件夹 offer、流、续传、校验、进度、safeRelPath
internal/store   SQLite、user_version 迁移（migrate.go）、查询，无协议逻辑
internal/proto   协议 ID、信封、分帧；改协议先改这里
internal/proxy   TUN / 路由 / fake-IP DNS（LRU）/ 出口解析 / SOCKS5 / 出口 Client
internal/serve   fst serve：SSE + 令牌 Cookie 的 Web 外壳
internal/e2e     真实 libp2p 多节点测试
cmd/fst-relay    无头中继入口
phone/core       移动端 Go 门面（JSON 边界，无 TUN/WAN/群聊）
phone/daemon     把 core 暴露成本机回环 HTTP + SSE，编译为随包分发的可执行文件
phone/android    原生 Android 外壳（Kotlin + Compose + 前台服务 + SAF）
front/src/api/fst        唯一业务 HTTP 封装
front/src/types/api/fst.d.ts  与 Go JSON 对齐的类型
front/src/store/modules/fst.ts  唯一 Wails 事件订阅点
front/src/views/fst/{device,chat,transfer,settings}
front/src/locales/langs/{zh,en}.json  fst.* 业务词条
```

- `main.go` 只做「解析参数 + 装配 + 开窗」；新增启动期逻辑放 `internal/bootstrap`，不要在 `main.go` 里堆业务。
- 移动端只允许在 `phone/` 下加代码；跨端共性改动**改 `internal/`**，不要在 `phone/core` 里复制一份逻辑。

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

### 5.3 移动端（Kotlin / Android）

遵循 [Kotlin 官方编码规范](https://kotlinlang.org/docs/coding-conventions.html) 与 Android 平台惯例：

- 命名：包名全小写（`com.fst.phone.data`）；类/接口 `UpperCamelCase`；函数/属性 `lowerCamelCase`；常量 `UPPER_SNAKE_CASE`；Composable 函数用 `UpperCamelCase`。
- 缩进 4 空格，不用 Tab；单行不超过 120 列；import 按字母序、不写通配符。
- **线程**：所有 `CoreClient` 请求与核心进程管理必须在 `Dispatchers.IO` 执行；Composable 里禁止直接发请求，只能读 `StateFlow`。
- **状态**：界面状态一律 `MutableStateFlow`（私有）+ 只读 `StateFlow`（公开），在 `FstRepository` 收口；不要在 Composable 间传可变对象。
- **权限**：新增敏感权限必须三处同步——`AndroidManifest.xml` 声明、`MainActivity.requestRuntimePermissions()` 申请、拒绝后的能力降级说明。禁止只改 Manifest。
- **前台服务**：Android 14+ 必须声明 `android:foregroundServiceType` 并申请对应 `FOREGROUND_SERVICE_*` 权限；`startForeground` 必须在 5 秒内调用。
- **文件**：进出用户文件只能走 SAF / MediaStore（`ContentResolver`、`OpenMultipleDocuments`），禁止硬编码 `/sdcard` 或申请 `MANAGE_EXTERNAL_STORAGE`。
- **文案**：界面文字进 `res/values/strings.xml` 或用中文常量集中定义；不要在布局/Composable 里散落硬编码中文（本仓库首版中文单语，但位置要留对）。
- **不要**在 Kotlin 侧重新实现 mDNS 报文、握手、加密或分帧——那是 `internal/` 的职责，移动端只调用 `phone/core`。唯一例外是**发现**：Android 11+ 禁止应用使用 `NETLINK_ROUTE`，Go 的 mDNS 在手机上不可用，因此允许（并要求）用系统 `NsdManager` 做注册/浏览，把解析出的 `dnsaddr` 通过 `POST /discovered` 注入核心。这是平台 API 调用，不是协议重写。
- **明文策略**：任何回环 HTTP 都必须有 `res/xml/network_security_config.xml` 兜底，且只放行 `127.0.0.1/localhost/::1`；禁止用 `usesCleartextTraffic="true"` 全量放开。

### 5.4 测试与构建

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

### 6.3 Kotlin / Android（KDoc）

用 KDoc（`/** ... */`）放在声明正上方，中文描述；**必须写清：功能、参数（名称 + 类型 + 含义）、返回值类型与语义、可能抛出的异常、以及线程/生命周期副作用**。`@param`/`@return`/`@throws`/`@property` 标签照 KDoc 标准写。

```kotlin
/**
 * 把 SAF 选中的文档复制到私有发件箱，然后发起传输。
 *
 * 参数:
 *  - context: Context — 用于 ContentResolver
 *  - peerId: String — 对方 PeerID
 *  - uris: List<Uri> — SAF 返回的文档 URI
 *
 * 返回:
 *  - Unit；复制失败会跳过该文件并通过 notice 流提示
 *
 * 副作用：在 cacheDir/outbox 写入临时副本（发送期间占用一份磁盘空间）。
 */
fun sendDocuments(context: Context, peerId: String, uris: List<Uri>)
```

```kotlin
/**
 * 单个会话页。
 *
 * @param peerId 对方 PeerID
 * @param title 顶栏显示的对方名称
 * @param onBack 返回回调
 */
@Composable
fun ChatScreen(peerId: String, title: String, onBack: () -> Unit)
```

- 数据类只在字段语义不直观时写 `@property`，字段名自解释可省略但类本身要有注释。
- `override` 的函数写清实现的是哪个接口契约（例如「实现 gomobile 生成的 Listener」）。
- Composable 函数注释说明「渲染什么 + 依赖哪些状态」，不必逐个复述参数名。
- XML 资源（Manifest、布局）用 `<!-- -->` 说明权限用途与平台版本差异，尤其是 Android 13/14 的行为变更。

### 6.4 例外（仅这些可以不写满参数块）

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
| `config.yaml` / proxy 字段 | `internal/bootstrap`、`main.go` Hooks、settings API、设置页 |
| 出口代理语义 | `internal/proxy` 测试 + [docs/开发文档.md](docs/开发文档.md) 代理小节（行为变了才改文档） |
| mDNS 服务名、实例名或 TXT 格式 | `internal/lanmdns`（`StandardService`/`LegacyService`/`InstancePrefix`）、`internal/proto`（`MDNSServiceStandard`/`MDNSService`）、`phone/android` 的 `data/Nsd.kt`（`SERVICE_TYPE`/`TXT_ADDR`/`NAME_PREFIX`）。实例名改动会同时影响 TXT 缺失时的兜底解析（`fallbackInfo`/`fallbackAddr`），必须一起改 |
| `phone/core` 导出方法 | `phone/daemon` 的路由、`phone/android` 的 `CoreClient`/`FstRepository`、`data/Models.kt`（JSON 字段必须与 Go tag 一致）、重跑 `phone/scripts/build-core.sh`。新增路由记得同步 `CoreClient` 与 README 的 IPC 表 |
| 对外广播地址（`node.Config.AnnounceIP`） | `phone/core` 的 `Start`/`SetLANIP`、`phone/daemon` 的 `FST_LAN_IP` 与 `POST /lanip`、`FstProcess` 的环境变量、`FstRepository.applyLanIP` |
| mDNS 代播（`lanmdns.Bridge` / `Unbridge`） | `bridgeTargets` 的「只补到对端没自播的标签」规则、`node.bridgePeer`（连接后补播）/`onDisconnected`（撤销）、`phone/data/Nsd.kt` 的 `announceAgain` 与 `FstRepository.REANNOUNCE_MS`、`phone/README.md` 的互通小节。代播记录必须以 PeerID 为键并随连接/发现刷新，不能按 IP 建表。**收到 `-br` 记录时按归属分流**（`isOwnBridgeRecord`）：PeerID 是本机才丢弃，是别人则当成正常发现派发（`via=bridge`）但不参与代播决策——无条件丢弃会让「只能靠中转机牵线」的设备永远互相看不见（现场 192.168.1.24 ↔ 手机） |
| mDNS 网卡选择（`lanmdns.LANInterfaces` / `Diagnose`） | `probeMulticastIPv4/6`（真实 setsockopt 探测）、`primaryIPv4`（默认路由优先）、`isVirtualIface`/`dropVirtual`（有物理网卡时剔除 vEthernet/VMnet/docker/tun）、单出口收敛（`mdns egress pinned`，多网卡需 `FST_MDNS_IFACE=a,b`）、`buildPayload` 只广播首选网卡、`cmd/netdiag`。改这里必须同步 `docs/局域网发现排查报告.md` 与启动日志 `mdns interfaces` | 
| 启动可观察性（`main.BuildTag`、单实例接管日志） | `main.go` 的 `fst started build=`、`OnSecondInstanceLaunch` 的 handoff 警告；每次发布修改 `BuildTag`，否则现场无法区分新旧二进制 |
| Android 权限或前台服务 | `AndroidManifest.xml`、`MainActivity` 的运行时申请、`phone/README.md` 平台要点表 |
| 新增本地 IPC 路由 | `phone/daemon` 的 `routes()`、`CoreClient`、`FstRepository`（含 token 头）；回环明文策略必须覆盖到 |

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
- 在 Kotlin 侧重写 mDNS / 握手 / 分帧，或让移动端改用另一套协议端口
- 代播时抢占对端自己正在用的服务实例名（同名记录会触发 DNS-SD 重名检测，把手机自己的实例改成 `FST-xxx (2)`）
- 把「周期重播本机服务」当成可选优化删掉：手机 `mdnsd` 收不到组播查询是现场复现过的真实状态，删掉后表现为「手机在线但谁也看不见」
- 移动端把文件内容读进内存再交给 Go（必须走私有目录路径）
- 为移动端单独复制一份 `internal/` 业务逻辑
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
