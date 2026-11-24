# 实现说明

## 1. 总体结构

```
FUSE 请求
   │
   ▼
pkg/vfs  ── waitMeta()/waitData()/waitOp() ──▶ pkg/qos.Controller（atomic.Pointer 持有）
   │                                              ├─ bw  总带宽桶
   │                                              ├─ rbw 读带宽桶
   │                                              ├─ wbw 写带宽桶
   │                                              ├─ iops 数据操作桶
   │                                              └─ meta 元数据操作桶
   ├──▶ pkg/meta（元数据引擎）
   └──▶ pkg/vfs.reader / writer（对象存储）
```

运行时更新有 3 个入口，全部汇入 `VFS.UpdateQoS`：

1. `.control` 消息 `meta.QoS`（1009），`juicefs qos` CLI 使用；
2. `SIGUSR1` → 重新读取 `--qos-config` 文件；
3. HTTP `PUT/POST /api/qos`（复用 metrics 端口，Bearer token）。

## 2. 模块与代码位置

### 2.1 `pkg/qos`（新增）

| 文件 | 内容 |
|---|---|
| `qos.go` | `Config`（五个 `uint64` 限额，0=不限）；`Update`（五个 `*uint64`，nil=保持、指向 0=清除）；`Config.Apply(Update)`、`Update.Empty()`；`Controller` 持有 5 个 `juju/ratelimit.Bucket`；`New`（全 0 返回 nil）；`WaitData`/`WaitMeta`/`WaitOp`/`wait`/`sleep`；`Conf()` |
| `parse.go` | `ParseRate`（`100M`/`1.5G` → 字节/秒，裸数字按字节/秒）；`Update.UnmarshalJSON`（带宽字段接受数字或带单位字符串，拒绝未知字段） |
| `http.go` | `Updater` interface（`GetQoS`/`UpdateQoS`）；`HTTPHandler(get func() Updater, token string)`；`checkToken`（`Authorization: Bearer` 或 `X-JuiceFS-Token`，`crypto/subtle` 常量时间比较） |

令牌桶细节：

- `newBucket(rate, burst)`：`burst = rate/10`，clamp 到 `[1, rate]`；
- `wait`：单次请求大于桶容量时按容量分块，逐块 `Take` 并睡眠；
- `sleep`：每 100ms 检查一次 `Canceled()`，取消返回 `EINTR`；
- `WaitData`：先扣 1 个 IOPS token；读方向选择 `rbw`（若设置）否则 `bw`，写方向选择 `wbw`（若设置）否则 `bw`；
- `WaitMeta`：只扣 `meta` 桶；`WaitOp`：只扣 `iops` 桶（服务端复制用）。

### 2.2 `pkg/vfs`

- `vfs.go`：`Config` 新增 `QoS *qos.Config`；`VFS` 新增 `qos atomic.Pointer[qos.Controller]` 与 `qosMu sync.Mutex`；`NewVFS` 里 `v.qos.Store(qos.New(conf.QoS))`。
- `qos.go`（新增）：`waitMeta(ctx)`、`waitData(ctx, read, n)`、`waitOp(ctx)`（nil 控制器直接放行）；`UpdateQoS(qos.Update) qos.Config`（加锁 → `Apply` → 查询直接返回 → 重建并 atomic Store → 打日志）；`GetQoS() qos.Config`。
- `internal.go`：`handleInternalMsg` 新增 `case meta.QoS`：解析 JSON → `UpdateQoS` → 回 `CDATA + JSON + errno` 帧。
- `vfs_unix.go`：平台相关方法门控。

### 2.3 `cmd`

| 文件 | 内容 |
|---|---|
| `flags.go` | `qosFlags()`：`--qos-bandwidth`、`--qos-read-bandwidth`、`--qos-write-bandwidth`、`--qos-iops`、`--qos-meta-iops`、`--qos-config`、`--qos-http-token`；加入 `clientFlags()` |
| `mount.go` | `getQoSConf`（flag + `--qos-config` 文件合并）；包级 `qosConfigFile`、`qosHTTPBackend atomic.Pointer[vfs.VFS]`、`qosHTTPOnce`；`NewVFS` 后注册 `http.Handle("/api/qos", ...)`（仅 mount 进程） |
| `qos.go` | `juicefs qos <mountpoint>`：打开 `.jfs.control` / `.control`，写 `[u32 1009][u32 len][json]`，读 `CDATA` 响应并打印；支持 `--json` |
| `mount_unix.go` | `installHandler` 的 `signal.Notify` 新增 `SIGUSR1`；收到后调用 `reloadQoS(v)` 并 `continue`，不影响优雅退出逻辑 |
| `main.go` | 注册 `cmdQoS()` |

## 3. 门控点与计费规则

### 元数据（每次 1 token，`--qos-meta-iops`）

`vfs.go`：`Lookup`、`GetAttr`、`Mknod`、`Unlink`、`Mkdir`、`Rmdir`、`Symlink`、`Readlink`、`Rename`、`Link`、`Opendir`、`Readdir`、`Create`、`Open`、`Truncate`、`Flush`、`Fsync`、`SetXattr`、`GetXattr`、`ListXattr`、`RemoveXattr`、`Fallocate`；
`vfs_unix.go`：`StatFS`、`Access`、`SetAttr`、`Getlk`、`Setlk`、`Flock`、`Ioctl`。

**不门控**：`Release`（无返回值）、`Releasedir`（返回 int），避免取消/阻塞关闭路径导致句柄泄漏；`ReleaseHandler`、`UpdateReaddirOffset` 同理。

### 数据

- `Read`：读取成功后按实际返回字节 `n` 计费（`waitData(ctx, true, n)`），失败或 EOF 不计费；
- `Write`：在 `h.Wlock` 之后、`h.writer.Write` 之前按 `len(buf)` 计费，失败仍会消耗已取的 token；
- `CopyFileRange`：服务端复制，只扣 1 个 IOPS token（`waitOp`），不扣带宽；
- `Fallocate`：按元数据操作计 1 个 token。

已知的重复计费：`Create` 在 `O_EXCL` 冲突时会内部调用 `Unlink`；`SetAttr` 截断时会调用 `Truncate`。两者都会多计 1 个元数据 token，这是两次真实引擎操作的代价，可接受。

### 计费公式

单次操作的等待时间近似为：

```
read:  max(iops_wait,  bytes / read_bandwidth)       （未设 read_bandwidth 时用 bandwidth）
write: max(iops_wait,  bytes / write_bandwidth)      （未设 write_bandwidth 时用 bandwidth）
```

桶容量为 `rate/10`，因此短时突发最多约 100ms 的量；小请求（如 4 KiB）通常被 burst 吸收，不会逐请求串行化。

## 4. 运行时接口

### 4.1 `.control` 协议

请求帧：`u32 cmd=1009` + `u32 size` + JSON（`Update`）。响应帧沿用内部协议：

```
[0xFF CDATA][u32 BE len][JSON(Config)][u8 errno]
```

`juicefs qos` 复用 `cmd/rmr.go` 的 `openController`、`cmd/warmup.go` 的 `readControl`/`readProgress` 逻辑，保证与其他工具命令一致。JSON 解析失败返回 `EINVAL`，CLI 打印 `invalid QoS update`。

### 4.2 配置文件（`--qos-config`）

```json
{"bandwidth": "100M", "readBandwidth": "200M", "writeBandwidth": "50M",
 "iops": 5000, "metaIOPS": 1000}
```

- 挂载时读取并与命令行参数合并（文件中的字段覆盖命令行）；
- `SIGUSR1` 时重新读取并 `UpdateQoS`，日志：`QoS config reloaded from <path>`；
- 未设置 `--qos-config` 时收到 `SIGUSR1` 只打一条 Warn 并忽略。

### 4.3 HTTP

- 地址 = `--metrics` 的监听地址；未显式设置 `--metrics` 时不会注册（或监听随机端口，见 `exposeMetrics` 行为），因此默认不暴露。
- token 来源：`--qos-http-token` 优先，其次环境变量 `JFS_QOS_HTTP_TOKEN`；都为空 → 403。
- `GET /api/qos` → `Config` JSON；`PUT|POST` → 局部 `Update`，返回应用后的 `Config`；未知字段 → 400（`DisallowUnknownFields`）；方法不支持 → 405 + `Allow`；客户端未就绪 → 503。
- 安全：常量时间比较；建议 token 走环境变量并在内网/本机使用；如需 TLS/鉴权体系，应在反向代理层终止。

## 5. 验证

| 项 | 命令/结果 |
|---|---|
| 格式 | `gofmt -l` 对改动文件无输出 |
| 静态检查 | `CC=<clang> CGO_ENABLED=1 go vet ./pkg/qos ./pkg/vfs ./cmd` → exit 0 |
| 单元测试 | `go test ./pkg/qos -count=1` → ok（覆盖带宽速率、读写方向分离、IOPS、元数据 IOPS、大请求分块、取消、非正尺寸、Update.Apply/Unmarshal、HTTP 403/401/GET/PUT/400/405/503） |
| VFS 集成测试 | `TestVFSQoSUpdate` 已编写；本机因 Xcode/clang 链接问题无法运行，需在 Linux + cgo 环境验证 |
| 端到端 | 在 v1.3.0 基础上构建的二进制做挂载测试：100 MiB/s 下 64 MiB 写入约 2.0s；`juicefs qos` 局部更新、HTTP PUT、SIGUSR1 重载均生效 |

## 6. 性能与开销

- 未启用：每个门控点只做一次 `atomic.Pointer.Load`（nil → 返回），无额外分配。
- 启用：每次操作 1~2 次 `ratelimit.Bucket.Take`（内部有 mutex），字节桶按请求大小扣减；无锁竞争时开销在纳秒~微秒级。
- 限流等待是“阻塞调用线程”，不会额外起 goroutine；FUSE 并发由内核与 go-fuse 调度保证。
- 更新时重建 5 个桶，一次性分配；旧桶由仍在等待的操作自然排空。

## 7. 限制与后续工作

1. **仅 mount 模式**：`gateway`/`webdav`/`mdtest`/`object` 走 `pkg/fs`，不经过 VFS。后续可在 `pkg/fs` 的 reader/writer 与元数据调用处加同套门控。
2. **按进程额度**：不提供跨挂载进程的聚合/公平；共享 mount pod 中每个进程各一份，需要平台侧按进程数折算。
3. **无按用户/Pod 归因**：如需 per-UID/cgroup 公平，需要在 VFS 门控中引入 `ctx.Uid()`/cgroup 维度并做分层桶。
4. **HTTP 无 TLS、单 token**：适合内网；公网暴露需反向代理与更完整的鉴权。
5. **可观测性**：当前只有更新日志，没有“因限流等待了多久”的 Prometheus 指标；后续可增加 throttle 计数与等待时延直方图。
6. **突发**：`burst = rate/10` 对低速率（例如 1 MiB/s）意味着约 100 KiB 的突发，若需要更平滑可考虑可配置 burst。
