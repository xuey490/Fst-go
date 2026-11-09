# FST 移动端（Android）

`phone/` 是 FST 的 Android 客户端。核心原则：**协议与桌面端完全一致，复用同一份 Go 核心，只换 UI 外壳**。

```
┌────────────────────────────┐        ┌──────────────────────────────┐
│ desktop (Wails + Vue)      │        │ phone/android (Compose)      │
│ fst serve (浏览器)          │        │ 前台服务 + 3 个页面           │
└─────────────┬──────────────┘        └───────────────┬──────────────┘
              │                                       │ 本机回环 HTTP + SSE
              │                                       ▼
     ┌────────┴────────────────────────────────────────────┐
     │  internal/  （同一份 Go 代码，无分支）                │
     │  node（libp2p + lanmdns） chat transfer store proto   │
     └────────┬────────────────────────────────────────────┘
              │ 被两种外壳复用
              ▼
     phone/daemon（fstcored）← 纯 Go 静态可执行文件，随 APK 分发
```

- 桌面、手机使用**相同 PeerID 机制、单端口 8787、multistream 协议**，因此可互相发现、聊天、传文件。
- 移动端不含 TUN 代理、跨网络中继（WAN）、群聊；这三块仍归桌面端。

## 为什么是「独立进程 + 本机 HTTP」而不是 gomobile 静态库

进程内调用看起来更简单，但 gomobile 必须经 cgo + NDK 的 clang 生成 JNI 胶水。本仓库实测：Windows 上 `gomobile bind` 会卡在 cgo 的 clang 探针（进程 0 CPU 挂死，无报错），整条发布链直接断。

改成独立进程后换来四个实际好处：

| 维度 | 独立进程（现方案） | gomobile AAR |
|---|---|---|
| 构建依赖 | 只需 Go，纯交叉编译 60s 级 | 需 NDK + clang，卡住即无法发布 |
| 崩溃隔离 | 核心挂了 UI 还能提示重启 | 一起崩 |
| 协议一致性 | 与桌面 `internal/` 同一份代码 | 同左 |
| iOS 复用 | 同一份守护进程逻辑 | 需另写绑定 |

代价：多一次本机 HTTP 往返（回环，微秒级，对聊天/文件流无感），以及核心二进制要随包分发（34MB）。

## 目录

```
phone/
├── core/                       Go 封装层（Core/Listener），跨语言只传 JSON
├── daemon/                     fstcored：把 core 暴露成本机 HTTP + SSE
├── android/
│   ├── app/src/main/
│   │   ├── AndroidManifest.xml 权限、前台服务类型、网络安全配置
│   │   ├── res/xml/network_security_config.xml  仅对回环放行明文 HTTP
│   │   ├── jniLibs/arm64-v8a/libfstcored.so   Go 核心（构建产物）
│   │   └── java/com/fst/phone/
│   │       ├── MainActivity.kt        权限申请 + Compose 挂载
│   │       ├── data/FstProcess.kt     拉起/关闭核心进程，读 LISTEN 端口
│   │       ├── data/CoreClient.kt     回环 HTTP + SSE 客户端
│   │       ├── data/Nsd.kt            系统 DNS-SD：注册/浏览/解析，注入对端地址
│   │       ├── data/FstRepository.kt  Go ↔ 界面唯一桥 + MulticastLock
│   │       ├── data/Models.kt         kotlinx.serialization 数据类
│   │       ├── data/Export.kt         MediaStore 导出到「下载/FST」
│   │       ├── service/FstService.kt  前台服务（常驻 + 通知）
│   │       └── ui/                    Compose 界面（首页四宫格 + 设备/会话/传输/设置/聊天）
│   └── local.properties       本机 SDK 路径（不提交）
└── scripts/
    ├── build-core.sh          交叉编译守护进程 → jniLibs
    ├── build-apk.sh           Gradle assembleDebug/Release
    └── e2e-ipc.sh             双进程端到端联调（发现/消息/文件+SHA256）
```

## 构建

```bash
# 1) 编译 Go 核心（不需要 Android SDK/NDK）
phone/scripts/build-core.sh

# 2) 编译 APK（需要 Android SDK 35 + JDK 17/21 + Gradle 8.9+）
ANDROID_HOME=/c/Android \
GRADLE=/c/gradle/gradle-8.13/bin/gradle \
  phone/scripts/build-apk.sh
# 产物：phone/android/app/build/outputs/apk/debug/app-debug.apk

# 3) 安装
adb install -r phone/android/app/build/outputs/apk/debug/app-debug.apk
```

`local.properties` 里的 `sdk.dir` 属于本机配置，不要提交。

## 本机联调（不装手机也能验证核心）

`phone/scripts/e2e-ipc.sh` 会拉起两个核心进程，验证：mDNS 互相发现 → 发文本 → 传 1MB 文件 → 比对 SHA-256。
先把 `daemon` 编成本机可执行文件：

```bash
go build -o /tmp/fstcored.exe ./phone/daemon
bash phone/scripts/e2e-ipc.sh
```

## 与桌面端互通

1. 电脑与手机连**同一个 Wi-Fi**，路由器未开「AP 隔离 / 客户端隔离」。
2. 两端启动 FST：电脑端出现手机名（默认「品牌 机型」），手机「设备」页出现电脑。
3. 点设备进会话发消息；点「附件」选文件发送。

## 界面结构

首页是四个大图标入口（设备 / 会话 / 传输 / 设置），点进去各自是独立页，系统返回键回首页。

| 页面 | 能力 |
|---|---|
| 设备 | 在线 / 离线分成两段；**离线段按设备名去重**，同名只保留最近在线的一条（桌面端重装会换 PeerID，旧身份不再重复占位）；在线设备可进入会话 |
| 会话 | 与首版一致：最近消息、未读数、头像与在线小圆点 |
| 传输 | 顶部搜索框按文件名 / 相对路径 / 设备名 / 状态过滤；顶栏「清理」删除已结束记录 |
| 设置 | 设备昵称、头像（本地压到 ≤12KB 后随 HELLO 同步）、PeerID 与监听地址复制、核心状态与日志、并发文件数、传输限速、发送前预校验、自动接收、受信设备开关、重启核心 |

设置页与桌面端 `settings` 表同源：并发 / 限速 / 预校验 / 自动接收由 transfer
服务每次使用时重新读取，保存即生效。桌面壳专属项（开机自启、关闭按钮行为、
只允许单实例、跨网络中继与代理）在移动端没有对应实现，页尾已明确说明，
不作为「缺功能」处理。

对应的移动端 IPC 接口（`phone/daemon`）：`GET/POST /settings`、`POST /avatar`、
`POST /trust`、`POST /peers/remove`、`POST /transfers/clear`。
4. 手机收到文件落在应用私有目录，列表里点「保存到下载」导出到系统「下载/FST」。

mDNS 服务类型：`_fst._tcp.local.`（标准，移动端可浏览）+ `fst.local.`（历史裸标签，仅为兼容旧装机）。桌面端由 `internal/lanmdns` 双向注册与浏览；**手机端由系统 DNS-SD 代劳**，原因见下节。

### 广播要主动「重播」，桌面端要会「代播」

真机现场出现过的两种「单向可见」，都不是握手问题，而是发现层：

| 现象 | 原因 | 处理 |
|---|---|---|
| 手机在线，但所有电脑都不显示它 | 部分 ROM 的 `mdnsd` 在息屏/省电后**收不到组播查询**，于是永远不应答；电脑的浏览只能靠「回应查询」拿到手机 | 手机端每 3 分钟主动重播一次服务（`Nsd.announceAgain`，`FstRepository.REANNOUNCE_MS`）。重播是主动组播，不依赖收包能力 |
| 旧版桌面上看不到手机（新版桌面端看得到） | 手机只能注册 `_fst._tcp`，P0 之前的桌面端只认 `fst.local.`，两代标签对不上 | 新版桌面端在发现/连上手机后，代它在 `fst.local.` 上补播一条记录（`lanmdns.Bridge`）。只要局域网里有一台新版桌面端，全场都能互相看见，不必逐台升级 |

界面状态条会显示「广播：已注册 / 注册失败（code=N）」——这一行就是「别人能不能看见我」的唯一判据；失败时会自动重试（15s 一次）。

## 为什么手机端不用 Go 侧 mDNS

真机联调发现手机端「发现不到任何设备」，日志里只有一条 `failed to resolve local interface addresses`。根因是 Android 的系统级限制，不是代码 bug：

| 事实 | 后果 |
|---|---|
| Android 11+ 禁止普通应用 bind `NETLINK_ROUTE`、禁止 `RTM_GETLINK`（golang/go#40569） | Go 的 `net.Interfaces()` 恒失败（`netlinkrib: permission denied`），libp2p 的地址退化为只有回环，`zeroconf` 无法枚举网卡 |
| 系统 `mdnsd` 常驻占用 5353 | 第三方若自行 bind 5353 会 `EADDRINUSE`；`zeroconf` 用的 `net.ListenUDP` 没开 `SO_REUSEADDR`，更绑不上 |

因此移动端把**发现**交给系统 `NsdManager`（实现见 `data/Nsd.kt`）：

1. 用 `/self` 的 `peer_id` + `port` 与本机 IPv4（取自 `ConnectivityManager.LinkProperties`，不经 netlink）拼出 `/ip4/<ip>/tcp/<port>/p2p/<peerID>`，作为 TXT `dnsaddr=` 注册 `_fst._tcp`；
2. 浏览 `_fst._tcp`，`resolveService` 成功后在主线程回调，把 TXT 里的 multiaddr 通过 `POST /discovered` 注入 Go 核心；
3. 核心内 `node.HandlePeerFound` 走原有的拨号路径，**握手、加密、分帧、文件传输仍全部由 Go 完成**。

代价与边界：Android 侧只多了一段「发现」代码，协议没有任何分叉；`node.Config.DisableMDNS` 在移动端置 true，避免无意义的 bind 与回环广播。`POST /discovered` 只接受带 PeerID 的完整 multiaddr，格式非法会返回 `地址格式错误`。

**手机之间不需要电脑中转。** 两端都在同一 Wi-Fi 时，DNS-SD 直接互见，libp2p 直接建连；电脑只有在「开启跨网络」「配置中继」时才承担中转角色，局域网内既不是必需也不是默认。

**实例名必须带完整 PeerID**（`FST-<PeerID>`，56 字节，在 DNS-SD 单标签 63 字节上限内）。原因：部分 ROM 的 `mdnsd` 在 resolve 时会把 TXT 丢掉，此时唯一还能拿到 PeerID 的地方就是实例名。`Nsd.fallbackAddr`（手机侧）与 `lanmdns.fallbackInfo`（桌面侧）会用它配合 SRV 端口 + A 记录拼出可拨号地址；不做这一步的表现是「服务看得见、永远连不上」，而且没有任何报错。

**跨语言 JSON 不得输出 `null` 数组。** Kotlin 的 `kotlinx.serialization` 把 `null` 当作类型错误（`Expected start of the array '['`），而 Go 的 nil 切片默认编码成 `null`。历史上 `store.Message.Attach` 为 nil 时输出 `"attach":null`，导致手机端**整个会话**解析失败、界面永远「还没有消息」。现在 Go 侧统一输出 `[]`，Kotlin 侧同时开启 `coerceInputValues = true` 兜底。

## Android 平台要点

| 问题 | 处理 |
|---|---|
| 收不到组播 | 持 `WifiManager.MulticastLock`（设备级锁，对独立进程同样生效）；缺 `CHANGE_WIFI_MULTICAST_STATE` 会静默失败 |
| 后台被杀 | 前台服务（`dataSync`）+ `START_STICKY` |
| Android 13+ 本地网络 | `NEARBY_WIFI_DEVICES`（`neverForLocation`）；Android 17 起归入 `NEARBY_DEVICES` 组，Play 2027-08-31 后强制 |
| Android 14 前台服务 | 必须声明 `foregroundServiceType` 并申请 `FOREGROUND_SERVICE_DATA_SYNC` |
| **回环 HTTP 被明文策略拦截** | targetSdk ≥ 28 默认禁止明文，**API 37 之前回环也不例外**。不配 `networkSecurityConfig` 时每次 `/health` 都抛 `Cleartext HTTP traffic to 127.0.0.1 not permitted`，界面表现为「核心健康检查超时」。已用 `res/xml/network_security_config.xml` 只对 `127.0.0.1/localhost/::1` 放行 |
| **Go 的 `net.Interfaces()` 不可用** | Android 11+ 禁 `NETLINK_ROUTE`；不要让移动端核心走 Go 侧 mDNS（`DisableMDNS: true`），发现交给 `NsdManager` |
| **必须由 Kotlin 提供本机 IPv4** | 同一个限制的另一面：libp2p 展开监听地址后只剩 `127.0.0.1`。若不改写，`/self` 里是回环、HELLO/identify 广播出去的也是回环，对端按它拨号会连到自己（peer id mismatch）。Kotlin 从 `ConnectivityManager.LinkProperties` 取地址，启动时通过 `FST_LAN_IP` 传入，运行期用 `POST /lanip` 热更新（换 Wi-Fi / DHCP 续租），Go 侧 `node.Config.AnnounceIP` 用 `AddrsFactory` 把回环改写成该地址 |
| 执行 Go 二进制 | 放进 `jniLibs/arm64-v8a/lib*.so` + `useLegacyPackaging = true` → 解压到 `nativeLibraryDir`（Android 10+ 唯一允许 execve 的位置） |
| **必须静态、非 PIE** | 加 `-buildmode=pie` 会让 Go 写入 `PT_INTERP = /lib/ld-linux-aarch64.so.1`（glibc 路径），Android 上该路径不存在，`execve` 返回 ENOENT，进程静默不死不活：App 能开、但 8787 无监听、mDNS 零报文。默认 `buildmode=exe` 产出 ET_EXEC 静态文件（无 `.interp`、无动态段），内核直接加载。`build-core.sh` 已内置自检。 |
| 不能改用 `GOOS=android` | 它才给出正确的 `/system/bin/linker64`，但会启用 `wlynxg/anet` 的 Android 分支（`//go:linkname zoneCache net.zoneCache`），Go 1.27 已移除该内部符号，链接失败；anet 最新即 v0.0.5，上游无修复。 |
| 文件进出 | 发送走 SAF（`OpenMultipleDocuments`）复制到 `cacheDir/outbox`；接收写私有目录，导出用 MediaStore |

## 已知限制（首版有意为之）

- **发送时多一份临时副本**：传输引擎以本地路径为接口且需要 seek（断点续传），SAF 的 URI 不是路径。传输结束后由系统缓存回收。
- **核心体积** 30MB（libp2p + SQLite + QUIC），仅 arm64-v8a；如需模拟器请改 `build-core.sh` 增加 `GOARCH=amd64`（同样保持静态非 PIE；`GOOS=android` 的 linkname 问题不适用于 `GOOS=linux`）。
- **后台长传**：Doze 下会变慢，已支持断点续传，回前台继续。
- **不支持**：TUN 代理、跨网络中继、群聊、多设备消息同步。
- **未在真机验证**：本仓库的构建环境没有 Android 设备/模拟器，APK 只验证了「能编译、能签名、能被系统解包」，运行时行为需在真机确认。
