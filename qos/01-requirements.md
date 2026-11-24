# 需求说明

## 1. 背景

JuiceFS 社区版 `v1.3.0` 在客户端层面只提供对象存储侧的带宽整形（`--upload-limit` / `--download-limit`，单位 Mbps），没有面向用户可见吞吐、数据 IOPS 和元数据 IOPS 的限流能力，且挂载参数在挂载后不可调整。在多租户训练平台、共享 mount pod 这类环境中，单个作业容易占满挂载进程的吞吐或元数据能力，影响同节点其他作业。

本功能在**客户端**增加 QoS 能力，不修改元数据引擎与对象存储，不依赖外部代理。

## 2. 目标

1. 控制**数据带宽**：总量、读方向、写方向可分别设置。
2. 控制**数据 IOPS**：每秒 read/write/copy 操作数。
3. 控制**元数据 IOPS**：每秒元数据操作数（lookup、getattr、readdir、create、rename 等）。
4. 支持**运行时调整**：不卸载、不重启挂载进程即可生效。
5. 对未启用 QoS 的挂载**零开销**。

## 3. 功能需求

| 编号 | 需求 | 说明 |
|---|---|---|
| F1 | 总带宽限制 | 读写共享一个令牌桶，单位支持 `100M`、`1G` 或裸数字（字节/秒） |
| F2 | 方向带宽限制 | `--qos-read-bandwidth` / `--qos-write-bandwidth` 设置后，该方向改用独立桶，不再消耗总带宽桶 |
| F3 | 数据 IOPS 限制 | 每次 read/write/copy 至少消耗 1 个 IOPS token |
| F4 | 元数据 IOPS 限制 | 每个经过 VFS 的元数据操作消耗 1 个 token |
| F5 | 挂载时配置 | 通过 `--qos-*` 参数传入；`--qos-config` 可从 JSON 文件读取 |
| F6 | 运行时查询/修改 | `juicefs qos <mountpoint>` 通过 `.control` 查询或局部更新；CLI 支持 `--json` |
| F7 | HTTP 控制接口 | 复用 `--metrics` 监听端口，`/api/qos` 支持 GET/PUT/POST；token 为空时禁用 |
| F8 | 配置热重载 | 挂载时带 `--qos-config`，收到 `SIGUSR1` 后重新读取并应用 |
| F9 | 局部更新语义 | JSON 中未出现的字段保持原值；显式 `0` 清除该项限制 |
| F10 | 查询无副作用 | 只读查询不重建令牌桶、不改变限流状态 |

## 4. 非功能需求

| 编号 | 需求 |
|---|---|
| N1 | 关闭（未配置任何限制）时 `qos.Controller` 为 `nil`，数据/元数据路径只多一次指针判空 |
| N2 | 不引入新的第三方依赖（复用已有 `github.com/juju/ratelimit`） |
| N3 | 等待过程可取消：上下文取消/中断时返回 `EINTR`，不让请求无限挂起 |
| N4 | 运行时更新原子生效：新操作立即看到新限额，已在等待的操作继续排空旧桶，不需要全局锁 |
| N5 | 与既有 `--upload-limit` / `--download-limit` 互补：后者限制对象存储流量，本功能限制经过 FUSE 的用户可见流量与操作数 |
| N6 | 不改变元数据引擎、对象存储协议与已有的锁/事务行为 |
| N7 | 单次等待不超过 100ms 就重新检查取消状态，避免长时间不可中断 |

## 5. 验收标准

1. `juicefs mount --qos-*` 参数被接受并在 `juicefs qos` 中可查；非法值（如 `--qos-bandwidth abc`）告警并忽略（或按 CLI 报错处理）。
2. 设置 100 MiB/s 总带宽时，单进程顺序写吞吐落在设定值 ±10% 附近（考虑 1/10 突发容量）。
3. `juicefs qos <mp> --bandwidth 50M --meta-iops 300` 后立即查询，带宽变为 50 MiB/s、元数据 IOPS 变为 300，未给出的 `iops` 保持原值。
4. HTTP：无 token → 403；错误 token → 401；GET 返回当前限额；PUT 局部更新并返回应用后的配置；未知字段 → 400；其他方法 → 405。
5. `--qos-config` 文件修改后 `kill -USR1` 生效，日志出现 `QoS config reloaded from <path>`。
6. `go test ./pkg/qos` 通过；`go vet ./pkg/qos ./pkg/vfs ./cmd` 通过；既有测试不回归。
7. 未设置任何 `--qos-*` 时，挂载与读写行为与 v1.3.0 一致。

## 6. 范围外

- 按 UID / PID / Pod / cgroup 的细粒度公平（当前额度是**每个挂载进程**一份）。
- `gateway`、`webdav`、`mdtest`、`object` 模式：它们使用 `pkg/fs`，不经过 VFS（见 02 文档）。
- `juicefs sync` 与工具类命令。
- 集群级配额（`juicefs quota` 是另一套语义，按目录计费）。
- 对象存储侧的请求级整形（已有 `--upload-limit` / `--download-limit`）。
- 鉴权/多用户隔离：`.control` 沿用 0666 语义，HTTP 仅做单 token 校验。
