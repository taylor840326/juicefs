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
| F3 跨客户端共享 | `cmd/qosd/main.go` + `pkg/qos/lease.go` | 各挂载进程向同一个 `qosd` 按需申请令牌，父子桶一起扣，Σ 子目录 ≤ 租户 cap |
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
  `qosd` 请求一批（`quantum = rate/2`，字节维度上限 16 MiB、ops 维度上限 2000）；
  `qosd` 对该层级及其所有祖先的桶一起扣减，只有全部足够才发放。这样多个挂载进程
  共享同一组桶，Σ 子 ≤ 租户 cap 由服务端一次性原子扣减保证。
- **租约流量控制**（避免 qosd 被逐操作请求打满）：
  - 一次请求拿半秒的量，正常情况下每个活跃维度每 1–2 秒才请求一次，而不是每
    次 I/O 或每次 `stat` 都请求；
  - 同一 `(inode, 维度)` 的并发等待做 single-flight 合并：一个请求在途时，其他
    协程等结果而不是各发一个 HTTP；
  - 服务端只发放了部分令牌时，客户端按缺口补速（`(quantum-granted)/rate`，
    上限 200ms）后再请求；完全拿不到令牌时按 50ms→250ms 退避重试。
  - 实测：200 MiB 写入（20 MiB/s 规则）只产生 **53 次** grant 请求，`qosd` CPU
    约 0.03s；优化前同样的写入是 16 万+ 次请求（约 5.5k req/s）。
- **已打开句柄与阻塞中的请求**：句柄记录解析时的 epoch，下一次 I/O 发现 epoch
  变化会重新解析 scope；此外 `Manager.ChargeWatch` 在每次等待迭代检查 epoch，
  一旦策略变化就返回 `ErrStale`，VFS 立刻用新 scope 重试。因此即使是**正卡在
  令牌等待里的请求**，也会在 ≤100–200ms 内切换到新规则，而不是把旧桶排空。
- `qosd` 重启会重置令牌(相当于短暂放宽)，这是 P0 的已知简化，P1 可持久化桶状态。

### 2.3 元数据分类

- 类别：`lookup / getattr / create / unlink / readdir / statfs`，对应
  `meta.*` 维度；未归类的操作只吃 `metaIOPS` 总桶。
- 一次操作同时扣「类别桶 + metaIOPS」；桶的顺序固定，避免死锁。
- 内部递归（如 `Create` 内部检查权限用的 `Lookup`）在 VFS 层不重复走门控——
  门控只在 VFS 方法入口调用一次。

## 3. CLI 与装配

qosd 是平台侧的 QoS 控制服务：策略存在 Redis，按 volume 提供增删改查接口，同时继续提供令牌租约（共享预算）。挂载只与 qosd 通信，不再需要本地策略文件。

```bash
# 1) 控制服务（独立二进制；策略存 Redis）
juicefs-qosd --listen 0.0.0.0:19567 --redis redis://127.0.0.1:6379/0 \
  --token "$JFS_QOSD_TOKEN"

# 2) 挂载：从 qosd 拉取本卷策略（每 3s），租约走同一服务
juicefs mount sqlite3:///data/meta.db /mnt/jfs \
  --qos-server http://qosd.internal:19567 \
  --qos-server-token "$JFS_QOSD_TOKEN" \
  --qos-lease-url http://qosd.internal:19567 \
  --qos-lease-token "$JFS_QOSD_TOKEN" \
  --qos-fail-open=true
```

- 卷名取自元数据中的 `Format.Name`，客户端请求 `/v1/policies/{volume}`；`404` 表示该卷未配置策略（不限速），之后按返回的 JSON 生效。
- 拉取周期 3 秒；`version` 未变不重载、不递增 epoch。`SIGUSR1` 触发立即拉取（运维提速用）。
- 本地文件模式仍保留（`--qos-policy-file` / `--qos-config`）；同时配置 `--qos-server` 时以服务端为准。
- 策略 JSON 与本地文件同构：`tenant`、`volume`、`version`、`cap`、`rules[]`。路径规则在挂载端解析为 inode（`--subdir` 挂载以挂载根为界）。
- 客户端级 `--qos-bandwidth` 等旧参数继续可用，与策略叠加。

### 3.1 qosd：多租户策略服务

**存储**（Redis）：

| key | 内容 |
|---|---|
| `jfs:qos:policy:{volume}` | 策略 JSON（与本地 `policy.json` 同构） |
| `jfs:qos:version:{volume}` | 版本计数器（服务端分配，写入时自增） |
| `jfs:qos:volumes` | 卷索引（列表查询） |

**HTTP API**（`/v1/healthz` 免鉴权，其余需要 token；错误返回 `{"error": "..."}`）：

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/healthz` | 健康检查 |
| GET | `/v1/policies` | 列出策略；`?tenant=t1` 过滤 |
| GET | `/v1/policies/{volume}` | 取某卷策略；不存在返回 404 |
| PUT | `/v1/policies/{volume}` | 创建/整体替换策略（version 由服务端递增） |
| DELETE | `/v1/policies/{volume}` | 删除策略 |
| GET | `/v1/policies/{volume}/rules` | 规则列表 |
| POST | `/v1/policies/{volume}/rules` | 新增规则（`path` 或 `ino` + `limits`，`id` 可省略） |
| PUT | `/v1/policies/{volume}/rules/{id}` | 更新规则 |
| DELETE | `/v1/policies/{volume}/rules/{id}` | 删除规则 |
| POST | `/v1/grant` | 令牌租约（挂载进程使用） |

```bash
TOKEN=demo-token
curl -X PUT -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  --data @policy.json http://127.0.0.1:19567/v1/policies/mt-demo
curl -H "Authorization: Bearer $TOKEN" http://127.0.0.1:19567/v1/policies/mt-demo
curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"path":"proj-c","limits":{"bandwidth":10485760}}' \
  http://127.0.0.1:19567/v1/policies/mt-demo/rules
```

写入方只需提供 `tenant`、`cap`、`rules`；`volume` 由路径填充，`version` 由服务端分配。同一 `path`/`ino` 重复返回 409。预算准入校验（`ValidateBudget`）尚未接到 API，属于 P1。

**独立二进制参数**：

| 参数 | 默认 | 说明 |
|---|---|---|
| `--listen` | `127.0.0.1:19567` | 监听地址；跨主机部署时配合 token 与网络隔离 |
| `--redis` | env `JFS_QOSD_REDIS` | Redis DSN（`redis://host:port/db` 或 `host:port`），必填 |
| `--token` | env `JFS_QOSD_TOKEN` | Bearer token；为空则跳过鉴权并打警告 |
| `--log-level` | `info` | `info`/`warn`/`debug`，非法值退出码 2 |
| `--version` | — | 打印版本后退出 |

```bash
make juicefs-qosd
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o juicefs-qosd-linux-amd64 ./cmd/qosd
```

**注意**：`juicefs qosd` 子命令已移除，qosd 只作为独立二进制发布。

### 3.2 qos.sh：策略运维助手

`qos.sh` 直接操作 qosd 的 HTTP API（读 → 局部修改 → PUT → 等客户端加载），不再编辑本地文件：

```bash
./qos.sh show                     # 当前策略 JSON + 各挂载最近一次加载
./qos.sh list                     # cap + 每条规则一行
./qos.sh cap --bandwidth 100M --iops 5000 --meta-iops 2000
./qos.sh set /proj-a --bandwidth 20M --meta-iops 50
./qos.sh del /proj-b
./qos.sh reload                   # 立即拉取并等待所有挂载确认
./qos.sh check                    # 等价于 ./test.sh
```

- 限速参数：`--bandwidth/--read-bandwidth/--write-bandwidth`（支持 `B/K/M/G/T/P` 后缀、1024 进制，裸数字为字节/秒）、`--iops/--read-iops/--write-iops`、`--meta-iops`、`--meta-lookup/--meta-getattr/--meta-create/--meta-unlink/--meta-readdir/--meta-statfs`。
- 修改后先 PUT（version+1），再对所有挂载发 `SIGUSR1`，轮询日志等待 `QoS policy loaded from ... version=N`；最多 6 秒，失败返回非零。
- 不依赖本地 `policy.json`；挂载点通过 mount 表枚举（macOS 上 `ps`/`pgrep` 会截断 argv），日志路径用 `lsof` 映射。

### 3.3 qosd 日志

默认 `--log-level=info` 记录状态变化；`debug` 追加每次请求明细：

| 事件 | 级别 | 示例 |
| --- | --- | --- |
| 启动/监听 | info | `juicefs-qosd 1.3.0 listening on 127.0.0.1:19567, redis=127.0.0.1:6379 (log-level=info)` |
| 鉴权开启 | info | `qosd: bearer token authentication enabled` |
| 策略写入 | info | `qosd: policy mt-demo stored version=3 tenant=t1 rules=2` |
| 策略删除 | info | `qosd: policy mt-demo deleted` |
| 规则新增/更新/删除 | info | `qosd: policy mt-demo rule rule-proj-a added (version=4)` |
| 新建/更新桶 | info | `qosd: created bucket tenant=t1 ino=20 dim=metaIOPS rate=50 burst=5` |
| 周期汇总 | info（每 10s） | `qosd: tenants=1 buckets=6 grants=120 denied=3 granted_tokens=...` |
| 授予令牌 | debug | `qosd: grant tenant=t1 dim=bandwidth want=10485760 granted=10485760 levels=20:20971520,0:104857600` |
| 请求被限流 | debug | `qosd: request throttled ...`（`denied` 计数随心跳汇总） |
| 鉴权/请求错误 | warn | `qosd: rejected POST /v1/grant from ...: bad token` |
| 收到信号/退出 | info | `qosd: received terminated, shutting down` / `qosd: stopped` |

## 4. 测试

`../juicefs-multitenant-qos-test/`（仓库外）提供脚本化验收：
`qosd.sh`（Redis + qosd + 播种策略）→ `mount.sh` → `test.sh`，以及双挂载的 `mount2.sh` + `shared-test.sh`。
细节见该目录的 `README.md`。

单元测试：`go test ./pkg/qos/`（策略解析、维度组合、预算校验、本地严格分区、
租约共享、fail-open、计数服务 HTTP 鉴权、策略文件轮询/远程拉取）。

## 5. 与 P1 的边界

以下内容不在本阶段，属于控制面（05 文档的 P1）：

- 预算准入 API（`ValidateBudget` 已提供函数，未接 HTTP）、审批流。
- 租户/目录用量上报与审计、额度借用（P2）。
- 策略快照签名/防篡改（当前依赖 `qosd` 的 token 与网络隔离）。
- 元数据分类之外的更多 op 类别、每个 inode 的例外规则。
- 真实元数据引擎上的多客户端一致性验证（本阶段用 sqlite3 单机两挂载验证共享预算）。

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
| 不发送 SIGUSR1，直接修改 `policy.json`（version+1）后 3 s 内生效 | 通过（`test.sh` 第 6 项） |
| `/proj-a` 写 200 MiB | 9.99 s ≈ 20.0 MiB/s |
| `/proj-b` 写 100 MiB（严格分区，不借兄弟额度） | 9.97 s ≈ 10.0 MiB/s |
| `rename /proj-a -> /proj-a-renamed` 后写 100 MiB | 4.95 s ≈ 20.2 MiB/s |
| `/proj-a-renamed` 200 次 `stat`（metaIOPS 50） | 4.14 s ≈ 50 次/s |
| `qosd` 停机时 fail-open 写 256 MiB | 2.03 s，日志含 lease 失败告警 |
| 两个挂载并行各写 100 MiB 到 `/proj-b`（共享预算） | 19.92 s ≈ 10.0 MiB/s（合计） |
| 400 MiB 写进行中 `qos.sh set /proj-a --bandwidth 100M`（已打开句柄热更新） | 5.80 s（前 2 s 走 20 MiB/s，其余走 100 MiB/s；不热更新约需 20 s） |
| 200 MiB 写（20 MiB/s）产生的租约请求 | 53 次，`qosd` 累计 CPU ≈0.03 s（逐操作请求的旧实现为 16 万+ 次） |

排障记录：policy 模式最初挂载点报 EIO。根因是 **FUSE 请求上下文的 `Err()` 恒为
`syscall.EINTR`**（`pkg/fuse/context.go` 的设计：取消要通过 `Canceled()` 判断，
且前 1 秒内恒为 false），而 `Manager.Charge` 之前用 `ctx.Err()` 判取消，导致每个
元数据操作都被当成已取消，直接返回 EINTR。改为优先使用 `Canceled()`（普通
`context.Context` 回退到 `Err()`），并去掉对 `ctx.Done()` 的 select（FUSE 上下文
未实现该通道）。修复提交：`597f8ec`。
