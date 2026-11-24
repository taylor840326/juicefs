# 需求分析与方案选型

## 1. 现状

`v1.3.0` 中与限速相关的机制只有对象存储侧整形：

- `--upload-limit` / `--download-limit`（Mbps）→ `chunk.Config.UploadLimit/DownloadLimit`（字节/秒），在 `pkg/chunk/cached_store.go` 里对对象存储请求做令牌桶整形，桶容量为速率/10。
- 粒度跟随 block（默认 4 MiB），且 cache 命中、预读、写缓冲合并都会改变“对象流量”与“用户可见吞吐”的比例；因此它不能作为用户带宽/IOPS 配额。
- 无数据 IOPS、元数据 IOPS 控制；参数挂载后不可变。

## 2. 限流位置选型

| 方案 | 覆盖范围 | 结论 |
|---|---|---|
| 对象存储前置代理 | 仅对象流量 | 与既有 `--upload-limit` 语义重复；cache 命中绕过；签名/缓存/成本问题，不采用 |
| 元数据协议代理（PG wire / RESP） | 语句级 QPS | 无法区分逻辑操作（readdir 批量、Lua EVAL 算一条）；代理排队会拉长事务、挤占心跳；单点风险，不采用 |
| FUSE 层（`pkg/fuse`） | 仅 mount 的 FUSE 操作 | 无法覆盖 gateway/webdav；每个 op 都要构造 context；语义分散，不采用 |
| `meta.Meta` 装饰器 | 元数据方法调用 | `readdir` 经 `DirHandler.List` 批量查询会绕过装饰器；数据路径不经过，不采用 |
| **VFS 层（`pkg/vfs`）** | mount 模式全部 FUSE 元数据 + 数据操作 | **采用**：单一收口、有 `Context`、天然覆盖 read/write/copy_file_range、与 FUSE 解耦 |

### 覆盖范围结论

`gateway` / `webdav` / `mdtest` / `object` 使用 `pkg/fs.FileSystem`，它自带 reader/writer 并直接调用 `fs.m`（`meta.Meta`），**不经过 VFS**，因此当前功能只对 `mount` 模式生效。这一点在参数文档中已明确说明；后续若需要覆盖这些模式，需要在 `pkg/fs` 增加同套 `waitMeta` / `waitData` 门控。

## 3. 动态控制通道选型

| 通道 | 优点 | 缺点 | 采用 |
|---|---|---|---|
| `.control` 内部消息（1009） | 复用已有虚拟文件与协议；不需要新端口；`juicefs qos` CLI 天然可用 | 依赖挂载进程；`.control` 权限 0666，本机任意用户可改 | 主通道 |
| `SIGUSR1` + `--qos-config` | 简单，适合 K8s ConfigMap 热更新 | 只能整文件重载；需要进程权限 | 辅助通道 |
| HTTP `/api/qos` | 易与平台/编排集成；带 token | 复用 metrics 端口，若监听 0.0.0.0 需依赖 token；仍无 TLS | 可选通道 |

HTTP 接口挂在 `--metrics` 监听器上（仅 mount 进程注册），未配置 token 时所有请求返回 403，等价于关闭控制面。

## 4. 运行时并发语义

- `VFS.qos` 为 `atomic.Pointer[qos.Controller]`。
- 更新时在 `qosMu` 保护下按 `Config.Apply(Update)` 计算新配置，`qos.New` 重建令牌桶，然后原子替换指针；查询（`Update.Empty()`）不重建。
- 已在等待的操作继续使用旧控制器的桶（其容量与速率固定），新操作立即使用新桶；不存在“半个请求按新限额重算”的不一致。
- 令牌桶 burst 取 `rate/10`，并 clamp 到 `[1, rate]`；单次请求大于桶容量时按容量分块消耗，避免“大请求永远拿不到 token”。

## 5. 风险与约束

1. **按进程计数**：共享 mount pod 中 N 个挂载进程 = N 份额度；不提供跨进程聚合。
2. **突发**：令牌桶允许最多 `rate/10`（且不超过 1 秒的量）的瞬时突发；低速率场景下这是可感知的抖动，但不会突破长期平均。
3. **元数据限流与后台任务**：门控只作用于用户路径（VFS 方法），不改变 `pkg/meta` 内部的心跳、flush、清理循环，因此不会因为限流导致会话过期。
4. **中断安全**：等待期间每 100ms 检查 `Canceled()`，取消返回 `EINTR`；释放类操作（`Release`/`Releasedir`）不门控，避免关闭路径被阻塞或句柄泄漏。
5. **权限**：`.control` 沿用既有 0666；HTTP 用常量时间比较 token；token 建议走环境变量，避免出现在 `ps`。
6. **不改变数据一致性**：限流只延迟操作，不改变读写顺序、close-to-open 语义或缓存行为。
