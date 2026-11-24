# 多租户 QoS 客户端实现（P0）

本文记录 `docs/zh_cn/qos/05-multitenant-implementation.md` 中 P0 阶段的落地结果：
客户端侧的策略解析、目录级限流、层级桶、共享计数服务、运行时重载与测试方式。
控制面（策略存储、申请审批、预算准入、审计）仍是 P1，本文只描述客户端如何消费
一份已经生成好的策略快照。

## 1. 已实现的能力

| 需求 | 实现位置 | 说明 |
|---|---|---|
| F1 卷级 QoS | `qos.Manager` + tenant `cap` | 策略里的 `cap` 是最后一道总闸，未命中任何规则的路径都继承它 |
| F2 目录级 QoS | `pkg/vfs/qos_policy.go` `qosScope` | 从操作目标的 inode 向上找最近带规则的祖先，命中谁的规则就用谁的配额 |
| F3 跨客户端共享 | `cmd/qosd.go` + `pkg/qos/lease.go` | 各挂载进程向同一个 `qosd` 按需申请令牌，父子桶一起扣，Σ 子目录 ≤ 租户 cap |
| F4 严格分区 | `Manager.chargeLocal` / `Counter.Grant` | 子规则的桶独立，顺序消耗；兄弟目录剩余额度不会被借用 |
| F5 读写带宽 | `Limits.ReadBandwidth/WriteBandwidth` | 方向桶优先，未设置时回退到共享 `bandwidth` |
| F6 数据 IOPS | `Limits.IOPS/ReadIOPS/WriteIOPS` | 每次数据操作先扣 1 个 IOPS，再按字节扣带宽 |
| F7 元数据 IOPS | `Limits.MetaIOPS` | 所有受控元数据操作共享一个桶 |
| F8 元数据分类 | `Limits.MetaOps` + `MetaDim()` | lookup/getattr/create/unlink/readdir/statfs 各自独立桶，和总桶叠加 |
| F9 运行时热更新 | `SetQoSPolicy` / `SIGUSR1` | 重新加载策略文件并解析路径，epoch 递增使 scope 缓存失效 |
| F10 fail-open | `--qos-fail-open`（默认 true） | 计数服务不可用时放行并告警；设 false 则返回 EINTR/EIO |
| F11 兼容/零开销 | `qosScope` 提前返回 | 未配置策略且没有旧 `--qos-*` 参数时不走任何额外代码 |
| F12 观测 | 日志 + `GetQoSPolicy` | `QoS policy loaded/reloaded`、`lease service unavailable` 等 |

## 2. 数据流

```
策略文件 policy.json
   │  mount: LoadPolicyFile + ResolvePaths(路径→inode)
   ▼
VFS.qosPolicy (atomic)          ── SIGUSR1：重读文件、重建 policy、epoch++
   │
   ├─ 元数据操作：qosWaitMeta(ctx, 目标 inode 或父 inode, 类别)
   │      └─ qosScope(ino)：inode → GetParents → … → 最近规则 + tenant cap
   │             └─ Manager.Charge：逐级扣桶（本地）或向 qosd 申请（租约）
   │
   └─ 数据操作：Open/Create 时把 scope 绑定到 handle
          └─ Read/Write：qosWaitData(ctx, h, 方向, 字节数)
                 └─ Manager.Charge（同一个 Scope，不再解析路径）
```

### 2.1 scope 解析与缓存

- 规则只挂在目录上，解析时从目标 inode 向上，遇到第一个有规则的祖先取它的
  `Limits`，继续向上可命中更多规则，最后追加 `cap`。
- 结果缓存在 `scopeCache`（`ino → scope`），带 `epoch`；策略变更与 rename
  成功后 `epoch++` 并清空缓存，因此移动目录树不会留下旧配额。
- rename 采用保守失效：只要配置了策略，rename 成功就清缓存（实现上在
  `VFS.Rename` 成功后调用 `invalidateQoSScopes`）。
- 文件打开时把 scope 绑到 `handle`，读写路径不重复走 `GetParents`。

### 2.2 层级桶与共享预算

- 本地模式（未配 `--qos-lease-url`）：每个层级一个 token bucket，一次操作
  从最近规则到 cap 逐级扣；父桶被所有子目录共享，因此「子目录不超过父目录」，
  但 Σ 子目录可能小于父桶容量（严格分区）。
- 租约模式：每个维度只扣「最近定义该维度的层级」。该层本地桶没有令牌时，向
  `qosd` 请求一批（`burst = rate/10`）；`qosd` 对该层级及其所有祖先的桶一起
  扣减，只有全部足够才发放。这样多个挂载进程共享同一组桶，Σ 子 ≤ 租户 cap
  由服务端一次性原子扣减保证。
- `qosd` 重启会重置令牌(相当于短暂放宽)，这是 P0 的已知简化，P1 可持久化桶状态。

### 2.3 元数据分类

- 类别：`lookup / getattr / create / unlink / readdir / statfs`，对应
  `meta.*` 维度；未归类的操作只吃 `metaIOPS` 总桶。
- 一次操作同时扣「类别桶 + metaIOPS」；桶的顺序固定，避免死锁。
- 内部递归（如 `Create` 内部检查权限用的 `Lookup`）在 VFS 层不重复走门控——
  门控只在 VFS 方法入口调用一次。

## 3. CLI 与装配

```bash
# 计数服务（每租户/平台一个，默认只监听本机）
juicefs qosd --listen 127.0.0.1:19567 --token "$JFS_QOSD_TOKEN"

# 挂载：策略文件 + 租约 + fail-open
juicefs mount sqlite3:///data/meta.db /mnt/jfs \
  --qos-policy-file /etc/juicefs/policy.json \
  --qos-lease-url http://127.0.0.1:19567 \
  --qos-lease-token "$JFS_QOSD_TOKEN" \
  --qos-fail-open=true
```

- 策略文件路径在**挂载时**读入，`SIGUSR1` 重读；CSI 场景用 ConfigMap 挂载 +
  `kill -USR1` 即可热更新。
- 旧的客户端级参数（`--qos-bandwidth` 等）继续可用，与策略叠加；两者都未配置
  时没有任何额外开销。

### 3.1 独立 qosd 可执行程序

计数服务同时提供独立二进制，适合和挂载进程分开部署（sidecar 或平台共享服务）。
它只依赖令牌桶计数器，不编译 FUSE、元数据引擎和对象存储客户端，体积约 7MB：

```bash
# 单独编译（不依赖 juicefs 主二进制）
make juicefs-qosd
# 等价于：CGO_ENABLED=0 go build -ldflags="-s -w" -o juicefs-qosd ./cmd/qosd

# 交叉编译 Linux 静态版本
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o juicefs-qosd-linux-amd64 ./cmd/qosd
```

参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--listen` | `127.0.0.1:19567` | 监听地址；跨主机部署时用 `0.0.0.0:19567` 并配合 token 与网络隔离 |
| `--token` | env `JFS_QOSD_TOKEN` | 校验 `Authorization: Bearer` / `X-JuiceFS-Token`；为空则跳过鉴权并打警告 |
| `--log-level` | `info` | 日志级别，可选 `info`、`warn`、`debug`；非法值直接报错退出 |
| `--version` | — | 打印版本后退出 |

```bash
juicefs-qosd --listen 0.0.0.0:19567 --token "$JFS_QOSD_TOKEN" --log-level debug
juicefs-qosd --log-level bogus
# ERROR: invalid log level "bogus": must be one of info, warn, debug
```

`juicefs qosd` 子命令仍然保留，行为一致（日志级别用全局 `juicefs --log-level`）。
测试脚本会自动优先使用 `juicefs-qosd`，找不到时回退到 `juicefs qosd`。

### 3.2 qos.sh：策略运维助手

测试包提供 `qos.sh`，用于查看和修改策略文件（`policy.json`），修改后自动向挂载进程发 `SIGUSR1` 完成热加载，无需重新挂载：

```bash
./qos.sh show                     # 打印策略 JSON + 最近一次加载日志
./qos.sh list                     # 每条规则一行（带宽按 MiB/GiB 显示）
./qos.sh cap --bandwidth 100M --iops 5000 --meta-iops 2000
./qos.sh set /proj-a --bandwidth 20M --meta-iops 50
./qos.sh set /proj-b --bandwidth 10M --read-bandwidth 8M
./qos.sh del /proj-b
./qos.sh reload                   # 只发 SIGUSR1 并校验 "QoS policy reloaded"
./qos.sh check                    # 等价于 ./test.sh
```

- 限速参数：`--bandwidth/--read-bandwidth/--write-bandwidth`（速率，支持 `B/K/M/G/T/P` 后缀、1024 进制，裸数字为字节/秒）、`--iops/--read-iops/--write-iops`、`--meta-iops`、`--meta-lookup/--meta-getattr/--meta-create/--meta-unlink/--meta-readdir/--meta-statfs`。
- 编辑采用临时文件 + `os.replace` 原子写入，`version` 自增；只修改本次给出的字段，其余规则保持不变。
- `mount.sh` 在挂载时会把包内的 `policy.json` 覆盖到 `$BASE/policy.json`；重新格式化/重挂后需要重新执行 `qos.sh set` 或直接编辑 `$BASE/policy.json`。
- 依赖可用的 `python3`；脚本探测 `/opt/homebrew/bin/python3`、`/usr/local/bin/python3`，也可用 `PY=/path/to/python3` 指定（系统自带的 `/usr/bin/python3` 在 Xcode 损坏的机器上不可用）。
- **修改立即生效**：`set`/`cap`/`del`/`reload` 都会枚举当前所有 JuiceFS 挂载点、逐个向挂载 daemon 发 `SIGUSR1`，并轮询各挂载日志等待新的 `QoS policy reloaded ... (version=N)` 行；策略重载后 epoch 递增，**已经打开的文件句柄会在下一次读写时自动重解析 scope**（无需重新 open），元数据操作立即按新规则计费。双挂载场景每个挂载点各收到一次信号、各自打一条 reloaded 日志。
- 批量改完建议追加 `./qos.sh reload` 做一次显式校验；脚本会打印每个挂载点的信号与确认行，任一挂载点未确认则返回非零。

### 3.3 qosd 日志

默认 `--log-level=info` 记录状态变化；`debug` 追加每次请求的明细：

| 事件 | 级别 | 示例 |
| --- | --- | --- |
| 启动/监听 | info | `juicefs-qosd ... listening on 127.0.0.1:19567 (log-level=info)` |
| 鉴权开启 | info | `qosd: bearer token authentication enabled` |
| 新建/更新桶 | info | `qosd: created bucket tenant=t1 ino=20 dim=metaIOPS rate=50 burst=5` |
| 周期汇总 | info（每 10s） | `qosd: tenants=1 buckets=6 grants=120 denied=3 granted_tokens=...` |
| 授予令牌 | debug | `qosd: grant tenant=t1 dim=bandwidth want=2097152 granted=2097152 levels=20:20971520,0:104857600` |
| 请求被限流 | debug | `qosd: request throttled ...`（`denied` 计数随心跳汇总） |
| HTTP 错误 | warn | `qosd: rejected POST /v1/grant from ...: bad token` |
| 收到信号/退出 | info | `qosd: received terminated, shutting down` / `qosd: stopped` |

## 4. 测试

`../juicefs-multitenant-qos-test/`（仓库外）提供脚本化验收：
`qosd.sh` → `mount.sh` → `test.sh`，以及双挂载的 `mount2.sh` + `shared-test.sh`。
细节见该目录的 `README.md`。

单元测试：`go test ./pkg/qos/`（策略解析、维度组合、预算校验、本地严格分区、
租约共享、fail-open、计数服务 HTTP 鉴权）。

## 5. 与 P1 的边界

以下内容不在本阶段，属于控制面（05 文档的 P1）：

- 策略存储、版本发布、审批流、预算准入（`ValidateBudget` 已提供函数，未接 API）。
- 租户/目录用量上报与审计、额度借用（P2）。
- 策略快照签名/防篡改（当前依赖 `qosd` 的 token 与网络隔离）。
- 元数据分类之外的更多 op 类别、每个 inode 的例外规则。
- 真实元数据引擎上的多客户端一致性验证（本阶段用 sqlite3 单机两挂载验证共享预算）。

## 6. 验收结果（macOS 实测，2026-11-24）

测试环境：`sqlite3` 元数据 + `file` 对象存储，单机两个挂载（`mnt`、`mnt2`），
`qosd` 监听 `127.0.0.1:19567`；策略见测试包 `policy.json`（租户 cap 100 MiB/s，
`/proj-a` 20 MiB/s + metaIOPS 50，`/proj-b` 10 MiB/s）。

| 检查 | 结果 |
| --- | --- |
| SIGUSR1 后 `QoS policy reloaded` 日志、路径解析为 inode | 通过 |
| `/proj-a` 写 200 MiB | 9.99 s ≈ 20.0 MiB/s |
| `/proj-b` 写 100 MiB（严格分区，不借兄弟额度） | 9.97 s ≈ 10.0 MiB/s |
| `rename /proj-a -> /proj-a-renamed` 后写 100 MiB | 4.95 s ≈ 20.2 MiB/s |
| `/proj-a-renamed` 200 次 `stat`（metaIOPS 50） | 4.14 s ≈ 50 次/s |
| `qosd` 停机时 fail-open 写 256 MiB | 2.03 s，日志含 lease 失败告警 |
| 两个挂载并行各写 100 MiB 到 `/proj-b`（共享预算） | 19.92 s ≈ 10.0 MiB/s（合计） |
| 400 MiB 写进行中 `qos.sh set /proj-a --bandwidth 100M`（已打开句柄热更新） | 5.80 s（前 2 s 走 20 MiB/s，其余走 100 MiB/s；不热更新约需 20 s） |

排障记录：policy 模式最初挂载点报 EIO。根因是 **FUSE 请求上下文的 `Err()` 恒为
`syscall.EINTR`**（`pkg/fuse/context.go` 的设计：取消要通过 `Canceled()` 判断，
且前 1 秒内恒为 false），而 `Manager.Charge` 之前用 `ctx.Err()` 判取消，导致每个
元数据操作都被当成已取消，直接返回 EINTR。改为优先使用 `Canceled()`（普通
`context.Context` 回退到 `Err()`），并去掉对 `ctx.Done()` 的 select（FUSE 上下文
未实现该通道）。修复提交：`597f8ec`。
