# 多租户 QoS：需求落盘与实现方案（讨论稿）

> 状态：讨论稿，供评审。范围不含元数据透明代理（按前期结论暂时搁置）。
> 基线：社区版 `v1.3.0`（tag `30190ca`）+ `qos` 分支现有客户端 QoS 实现。
> 关联：[01-requirements.md](01-requirements.md)、[02-requirements-analysis.md](02-requirements-analysis.md)、[03-implementation.md](03-implementation.md)、[04-evolution-analysis.md](04-evolution-analysis.md)。

本文分两部分：**第 1 部分把已确认的需求正式落盘**（可逐条验收）；**第 2 部分给出可讨论的实现方案**（架构、算法、接口、代码改造、里程碑与待决问题）。

---

# 第 1 部分：需求落盘

## 1.1 背景与目标

平台以“卷”为单位为每个租户提供 JuiceFS。平台需要：

1. 对每个租户卷设置**卷级 QoS 上限**（数据带宽、数据 IOPS、元数据 IOPS）；
2. 允许租户管理员在卷内**按子目录**分配 QoS，并让该卷的**所有挂载客户端共享同一份设定**；
3. 保证租户管理员与最终用户**无法绕过或篡改**配额，变更需走审批；
4. 保证**所有子目录配额之和不超过租户总量**，且子目录之间**严格分区、互不借用**。

现状（`03-implementation.md`）是“单挂载进程内、挂载时固定”的 QoS：多客户端各算各的、租户可自行改参数、没有预算校验，无法满足上述要求。

## 1.2 角色

| 角色 | 典型职责 |
|---|---|
| 平台管理员 | 创建租户卷、设定/调整租户总量、审批提额、强制收缩、查看全局审计 |
| 租户管理员 | 在本租户总量内为子目录设定 QoS、查看本租户用量与预留、提交提额申请 |
| 最终用户 | 挂载/使用卷内目录；**没有任何 QoS 写权限** |

## 1.3 功能需求

| 编号 | 需求 | 验收要点 |
|---|---|---|
| F1 | 平台可对租户卷设置卷级 QoS：`bandwidth`、`readBandwidth`、`writeBandwidth`、`iops`、`readIOPS`、`writeIOPS`、`metaIOPS`，以及元数据分类（`lookup/getattr/create/unlink/readdir/statfs`）可选 | 未设置的维度表示不限；卷级限制跨该卷所有挂载生效 |
| F2 | 租户管理员可为卷内目录设置规则；规则作用于该目录及其后代（最近祖先规则优先） | 规则按 inode 存储，目录 rename 后规则跟随；换路径后仍生效 |
| F3 | 同一租户卷的多个挂载客户端共享同一份规则与同一份额度 | 客户端 A 与客户端 B 的合计流量不超过租户上限 |
| F4 | 最终用户不能修改 QoS；租户管理员的写操作由平台网关鉴权 | 未授权写返回 403；mount pod 不暴露任何控制面 |
| F5 | 新增/调大规则必须通过预算校验：任意节点的直接子规则预留之和 ≤ 该节点额度，顶层规则之和 ≤ 租户总量 | 超出返回 409 `insufficient_budget`；不允许 overcommit |
| F6 | 严格分区：子目录只能使用自己的配额，不得借用兄弟目录或租户空闲额度 | 子目录 A 打满时，空闲额度不能被 B 使用 |
| F7 | 规则变更在目标时延内传播到该卷全部挂载（目标 ≤ 30s）；挂载进程重启后自动拉取最新快照 | 变更后 30s 内新挂载与已挂载客户端行为一致 |
| F8 | 观测：租户管理员可查看每条规则的配置值、当前使用、剩余预算；平台可查看全局 | 提供 API/CLI 查询；数据来自集中计数服务 |
| F9 | 审计：所有写操作记录操作者、时间、变更前后值、请求来源 | 审计不可由租户删除 |
| F10 | 降级语义可配置：控制面/计数服务不可用时，数据面继续服务；是否继续限流可选（默认 fail-open 并告警） | 断连后挂载仍可读写；恢复后自动续租 |
| F11 | 未配置多租户策略时行为与社区版一致，零额外开销 | 无策略快照时所有门控直接放行 |
| F12 | 平台可通过 CLI/HTTP 完成最小管理集：设卷级、增删改规则、查预算/用量 | 见第 2.5 节 API 草案 |

## 1.4 非功能需求

| 编号 | 需求 |
|---|---|
| N1 | 性能：启用限流后单次门控为内存操作 + 低频租约请求；未启用时零开销（nil 快路径） |
| N2 | 一致性：硬上限不可显著突破；允许的超发上界 ≤ 每个维度每挂载一个 lease quantum（第 2.4 节） |
| N3 | 可用性：计数服务故障不阻断数据面；挂载重启可用本地磁盘缓存的上次快照 |
| N4 | 可运维：计数服务支持 HA、按租户分片、可观测（grant/deny/latency） |
| N5 | 安全：租户不接触元数据地址与对象存储凭证；管理 token 不落日志、不随快照下发 |
| N6 | 兼容：老客户端忽略未知字段；策略版本可回滚 |

## 1.5 范围外（本期不做）

- 元数据透明代理/带 token 的元数据连接（m02439 决定搁置）；
- 对象存储前置代理；
- `gateway/webdav/mdtest/object` 模式（走 `pkg/fs`，不经过 VFS）；
- 按 uid/pid 的公平调度、跨租户借用、计费；
- 跨地域/多活。

## 1.6 前提假设

1. 租户只能通过**平台托管的 mount pod** 访问卷；网络策略阻断租户直达元数据库与对象存储。
2. 平台控制面已有租户、卷、鉴权与审计体系；本方案只定义 QoS 相关契约。
3. 元数据引擎以 PostgreSQL 为主；计数服务独立部署（可用 Redis）。
4. 卷由 CSI 或平台编排创建，挂载参数由平台注入。

## 1.7 验收标准（AC）

| 编号 | 场景 | 通过条件 |
|---|---|---|
| AC1 | 租户上限带宽 100MiB/s，两个 mount pod 各跑 80MiB/s 写 | 两者合计 ≤ 100MiB/s（允许 ≤ 2×quantum 瞬时超发） |
| AC2 | 租户上限 100MiB/s，/a 预留 60MiB/s、/b 预留 30MiB/s | 创建 /c 预留 20MiB/s 被拒（409）；/a 实际 60、/b 空闲时 /a 仍 ≤ 60 |
| AC3 | 最终用户调用管理 API | 403；日志有审计记录 |
| AC4 | 租户管理员把 /a 规则调到超出剩余预算 | 409，策略不变 |
| AC5 | 规则变更后 | 30s 内所有挂载点生效 |
| AC6 | 目录 rename | 规则跟随新路径，限流不中断 |
| AC7 | 计数服务宕机 | 按配置 fail-open 继续服务并告警；恢复后额度正确 |
| AC8 | 未配置策略 | 与社区版行为一致，无额外系统调用 |
| AC9 | 多租户隔离 | 租户 A 的规则/用量对租户 B 不可见 |
| AC10 | 审计完整性 | 每次写操作均可在审计中检索到 |

---

# 第 2 部分：实现方案

## 2.1 总体架构

```
                    ┌──────────────────────────────────────────┐
                    │ 平台控制面（QoS Service，新组件）          │
                    │  - 策略树 / 预算账本 / 审批 / 审计          │
                    │  - GET /policy-snapshot（mount pod 拉取）  │
                    └───────────────┬──────────────────────────┘
                                    │ 快照(JSON, version/ETag, 5s 轮询)
        ┌───────────────────────────┼───────────────────────────┐
        │                           │                           │
┌───────▼────────┐         ┌────────▼───────┐          ┌────────▼───────┐
│ mount pod (租户1)│         │ mount pod (租户1)│          │ mount pod (租户2)│
│ 策略快照缓存     │         │ 策略快照缓存     │          │ 策略快照缓存     │
│ scope 解析 + 本地桶│  ────►  │ scope 解析 + 本地桶│  ──────► │ scope 解析 + 本地桶│
│ 租约客户端       │         │ 租约客户端       │          │ 租约客户端       │
└───────┬────────┘         └────────┬───────┘          └────────┬───────┘
        │ Grant(scope,dim,n)         │ Grant                         │
        └───────────┬───────────────┴──────────────┬────────────────┘
                    ▼                              ▼
        ┌───────────────────────┐        ┌──────────────────┐
        │ 计数服务（层级令牌桶）   │        │ 元数据 / 对象存储  │
        │ Redis + Lua 或 Go 服务  │        └──────────────────┘
        └───────────────────────┘
```

- **执行点在 mount pod 内的 VFS 层**：租户只接触 FUSE 挂载点，无法替换限流逻辑。
- **控制面不参与数据路径**：只做策略与预算，挂载侧用快照 + 租约。
- **计数服务**是唯一看到全局用量的组件，负责层级扣减；挂载侧本地桶只做节流，不做权威记账。

## 2.2 策略模型与数据模型

### 2.2.1 策略快照（控制面 → mount pod）

```json
{
  "version": 42,
  "tenant": "t1",
  "volume": "jfs-t1",
  "epoch": 7,
  "cap": {
    "bandwidth": 104857600, "readBandwidth": 0, "writeBandwidth": 0,
    "iops": 10000, "readIOPS": 0, "writeIOPS": 0,
    "metaIOPS": 5000,
    "metaOps": {"lookup": 2000, "readdir": 500}
  },
  "rules": [
    {"id": "r-a", "path": "/run/sczx001", "ino": 12345,
     "limits": {"bandwidth": 52428800, "iops": 500}},
    {"id": "r-b", "path": "/run/sczx002", "ino": 23456,
     "limits": {"bandwidth": 31457280}}
  ]
}
```

规则以 **inode** 为准，`path` 仅用于展示与幂等校验；`ino` 在快照下发前由控制面解析（路径不存在则拒绝创建，返回 404/422）。挂载侧应用快照时再做一次 `Lookup` 校验，`ino` 不匹配则标 `stale` 并告警，不参与限流。

### 2.2.2 有效规则

对 inode `I`，沿父目录链向上，遇到的**第一个**规则即为该 inode 的规则；根规则 `cap` 恒存在。

**计费链**：从 `I` 的最近规则节点起，到根，所有带规则的祖先节点逐级扣桶。因此：

- 严格分区由“准入”保证：任意节点的直接子规则预留之和 ≤ 该节点额度；顶层规则之和 ≤ 租户 cap。
- 嵌套规则产生**逐级限制**：`/a=50M` 且 `/a/b=30M` 时，`/a/b` 的流量同时扣 `/a/b` 与 `/a`，`/a` 下未设规则的 `/a/c` 扣 `/a`。

### 2.2.3 预算账本（控制面）

按维度维护：`cap`、`reserved`（子树直接预留之和）、`available = cap - reserved`。

- 新增/调大规则：`reserved + Δ ≤ parent_limit`（逐级向上校验到 cap），否则 409；
- 调小 `cap` 时若 `cap < reserved`：拒绝，或进入“强制收缩”流程（先缩子规则，再降 cap）；
- 使用 `If-Match: <version>` 乐观锁防并发超卖。

## 2.3 运行时 scope 解析

### 2.3.1 解析算法

```
resolve(ino):
  if cached[ino] valid: return cached[ino]
  chain := []
  cur := ino
  for depth := 0; depth < MaxDepth && cur != 0; depth++ {
      if rule := rules[cur]; rule != nil { chain = prepend(chain, rule) }
      if cur == mountRoot || cur == tenantRoot { break }
      parents := v.Meta.GetParents(ctx, cur)   // map[Ino]int，硬链接可多父
      if len(parents) == 0 { break }
      cur = 选一个父（优先本地挂载路径上的父；否则取任意一个并告警）
  }
  scope := {ino, chain, effective: chain[0] 或 cap}
  cached[ino] = scope
  return scope
```

- `GetParents` 走 `attr.Parent` 快路径；没有 parent 时回退 `doGetParents`（较慢）。因此缓存必须命中率高：目录遍历、`Lookup/Create` 时已知父级，可**自顶向下传播**（父 scope 已知时，子 scope = 自身规则存在则新建链，否则继承父链）。
- **rename 失效**：`VFS.Rename` 成功后，若策略中任意规则存在（或源/目标祖先链上有规则），提升全局 `epoch`，清空 scope 缓存，惰性重建；rename 风暴下允许短暂冷缓存（用 metric 观测）。
- 打开的文件在 `Open/Create` 时把 `scope` 绑定到 `handle`，读写按句柄绑定的 scope 计费（与 RBD image 生命周期一致）；规则变更只影响新 `Open`。

### 2.3.2 元数据操作分类与去重

| 类别 | 映射 |
|---|---|
| lookup | `Lookup` |
| getattr | `GetAttr`（Open 内部触发的 GetAttr 需去重） |
| create | `Create`、`Mknod`、`Mkdir`、`Symlink`、`Link` |
| unlink | `Unlink`、`Rmdir` |
| readdir | `Opendir`、`Readdir`（每次调用计 1 个 token，不按条目数） |
| statfs | `StatFS` |
| 其他 | 只扣 `metaIOPS` 总桶 |

内部递归调用（`Create→Unlink(O_EXCL)`、`SetAttr→Truncate`、`Open→GetAttr`）用 context 标记跳过类别桶，避免同一逻辑操作扣两次；但**逐级规则链**仍按设计扣。

## 2.4 层级令牌桶与租约

### 2.4.1 本地桶

每个 `(scope, dimension)` 一个本地令牌桶；`quantum = clamp(limit/20, 1, 1MiB 或 10 ops)`。

```go
func (m *Manager) Acquire(ctx, scope, dim, n) error {
    for n > 0 {
        got := m.local.take(scope, dim, n)     // 0 表示本地令牌不足
        if got == 0 {
            granted, err := m.lease.Grant(ctx, scope, dim, m.quantum(scope, dim))
            if err != nil { return m.policy.OnLeaseFailure(err) }
            m.local.add(scope, dim, granted)
            continue
        }
        n -= got
    }
    return nil
}
```

### 2.4.2 集中扣减（计数服务）

`Grant(scope, dim, want)` 在计数服务内原子执行：

```
total := want
for level in chainFromScopeToRoot:        // 子规则 → … → 租户根
    total = min(total, level.available)   // 严格分区：任一祖先不足即截断
for level in chainFromScopeToRoot:
    level.available -= total              // 预支令牌
return total
```

- 令牌按各层速率回填（Redis Lua 内按时间差补充，带 burst 上限）。
- **超发上界**：每个 mount pod、每个维度最多持有 1 个 quantum 未消费令牌；合计突破量 ≤ mount 数 × quantum。该上界写入文档并在 AC1 中允许。
- 空闲额度**不可借用**：`available` 只能被自己的 grant 消耗，兄弟节点看不到。

### 2.4.3 失效语义

| 故障 | 行为 |
|---|---|
| 快照拉取失败 | 继续使用磁盘缓存的上次快照；告警；恢复后续用新版本 |
| 租约请求超时/计数服务不可用 | 默认 fail-open：放行并计数 `qos_lease_fail_total`；租户可选 fail-closed（拒绝 EIO/EDQUOT） |
| 快照 `ino` 失配 | 规则标 stale，不生效，告警 |
| 客户端重启 | 读取本地快照文件（mount pod 持久卷），立即恢复策略 |

## 2.5 控制面 API（草案）

鉴权：平台网关 OIDC/JWT；租户管理员用租户级 token；mount pod 用 mTLS 客户端证书。所有写接口用 `If-Match`。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/api/v1/tenants/{t}/qos` | 读策略树 + 预算 + 用量 |
| PUT | `/api/v1/tenants/{t}/qos/cap` | 平台管理员设定租户总量 |
| POST | `/api/v1/tenants/{t}/qos/rules` | 创建子目录规则 `{path, limits}` |
| PATCH | `/api/v1/tenants/{t}/qos/rules/{id}` | 修改规则（预算校验） |
| DELETE | `/api/v1/tenants/{t}/qos/rules/{id}` | 删除规则（不自动删除数据） |
| POST | `/api/v1/tenants/{t}/qos/requests` | 提额申请（审批流） |
| GET | `/api/v1/tenants/{t}/qos/budget` | 预留/可用/使用 |
| GET | `/api/v1/tenants/{t}/qos/usage?scope=` | 按 scope 的实时用量 |
| GET | `/api/v1/mounts/{t}/policy?version=N` | mount pod 拉取快照（mTLS） |
| POST | `/api/v1/leases/grant` | 计数服务内部接口（mTLS） |

错误约定：`400` 参数、`403` 权限、`404` 路径不存在、`409` 预算不足/版本冲突、`422` 校验失败（如维度为负）。

审计事件：`qos.cap.updated`、`qos.rule.created/updated/deleted`、`qos.request.created/approved/rejected`，含 actor、tenant、before/after、ts、request_id。

## 2.6 JuiceFS 代码改造清单

| 位置 | 改动 |
|---|---|
| `pkg/qos/policy.go`（新） | `Policy`/`Rule`/`Limits`/`Dimension`；`ParseSnapshot`、`Validate`、`Effective`；预算校验函数（控制面复用同一份代码） |
| `pkg/qos/lease.go`（新） | `LeaseClient` 接口 + HTTP/gRPC 实现；快照轮询器（ETag/version） |
| `pkg/qos/qos.go` | 保留现有 `Config`/`Controller` 作为**单机调试路径**；新增 `Manager`（policy + local buckets + lease） |
| `pkg/vfs/qos.go` | `waitMeta/waitData/waitOp` 改走 `Manager.Acquire`；scope 解析与缓存；`UpdateQoS` 保留用于本地调试 |
| `pkg/vfs/vfs.go` | `Config` 增 `QoSPolicy *qos.Manager`（或 `QoSSnapshot` 路径）；`VFS.qos` 类型改为 `*qos.Manager`；`Rename` 后失效缓存 |
| `pkg/vfs/handle.go` | `handle` 增 `scope *qos.Scope`；`Open/Create` 时绑定 |
| `pkg/vfs/internal.go` / HTTP | 保留 `.control` 1009 与 `/api/qos`，但仅平台管理员可用；多租户策略不走这里 |
| `cmd/mount.go` / `cmd/flags.go` | 新增 `--qos-policy-url`、`--qos-lease-url`、`--qos-snapshot-cache <file>`、`--qos-tenant-token`（mTLS 时省略）；保留既有 `--qos-*` 作为本地覆盖 |
| `pkg/meta` | 无接口改动；复用 `GetParents`（`interface.go:468`）与 `Attr.Parent` 快路径 |
| 控制面 | 独立仓库/服务；策略树、预算、审计、快照分发；可复用 `pkg/qos/policy.go` 的校验逻辑 |

## 2.7 计数服务选型

| 方案 | 优点 | 缺点 | 结论 |
|---|---|---|---|
| Redis + Lua | 部署简单、原子、社区熟悉；可用现有 Redis | 脚本维护、跨分片需按租户分片 | **一期推荐** |
| 独立 Go gRPC 服务 | 语义清晰、易测、易观测 | 需自建 HA、持久化 | 二期可选 |

Key 设计（每个 `(tenant, ino, dim)` 一个 hash）：`tokens`、`last_refill_ms`、`rate`、`burst`。Lua 脚本完成“按时间回填 + 层级扣减 + 返回 granted”，WATCH 不需要。按 `tenant` 分片到不同 Redis 实例；故障时整个租户进入降级路径。

## 2.8 里程碑（可讨论工作量）

| 阶段 | 内容 | 交付物 | 估算 |
|---|---|---|---|
| P0 | 策略模型 + scope 解析 + 本地层级桶（无租约） | `pkg/qos/policy.go`、解析缓存、单测 | 3–5 人日 |
| P1 | 快照分发 + mount pod 拉取 + 生效时延 | 控制面契约、轮询器、e2e（两个挂载点同策略） | 1–2 周 |
| P2 | 计数服务 + 租约 + 严格分区 + 失效语义 | Redis Lua、grant 接口、多挂载合计上限测试 | 2–3 周 |
| P3 | RBAC/审计/观测/混沌 | API 鉴权、审计事件、指标、故障演练 | 1 周 |

## 2.9 测试计划

- 单测：策略校验、预算树、scope 解析（含硬链接、rename、`--subdir` 截断）、租约上界。
- 集成：两个本地限流器对接一个 fake 计数服务，验证 AC1/AC2/AC6。
- e2e：K8s 两个 mount pod 写入同一租户卷，验证合计上限与传播时延（AC5）。
- 混沌：计数服务宕机/网络分区/快照损坏，验证 AC7 与恢复。

## 2.10 待讨论问题（需要拍板）

| # | 问题 | 选项 | 我的建议 |
|---|---|---|---|
| 1 | 计数服务归属与 SLO | 平台团队 / 存储团队自建 | 平台侧部署，存储侧提供 client 库；SLO 与元数据库同等级 |
| 2 | 租户 cap 是否也写进 Format（便于 `juicefs status` 展示） | 只控制面 / 双写 | 只控制面；`status` 通过控制面 API 展示 |
| 3 | 嵌套规则“逐级扣桶”是否接受双重限制 | 逐级扣 / 只扣最近规则 | 逐级扣（严格分区语义自洽）；若嫌严格可后续加“仅最近规则”开关 |
| 4 | 严格分区的空闲浪费是否接受 | 严格 / 可借用 | 严格（当前需求）；借用作为未来可选模式 |
| 5 | lease quantum 大小与超发上界 | 1MiB/10ops / 更大 | 默认 `limit/20`，上限 1MiB/10ops；可配置 |
| 6 | rename 失效策略 | 全局 epoch / 仅规则子树 | 全局 epoch（实现简单）；rename 风暴观测后再优化 |
| 7 | fail-open vs fail-closed 默认 | 固定 / 租户可选 | 默认 fail-open，租户可开 fail-closed |
| 8 | 元数据类别桶与总桶是否叠加扣 | 叠加 / 只扣类别 | 叠加（当前设计）；文档注明 |
| 9 | 缓存命中的读是否计 IOPS | 计 / 不计 | 计（与当前实现一致，计费语义稳定） |
| 10 | 共享 mount pod 内多 Pod 归因 | 接受不可归因 / per-tenant pod | 平台按租户分配 mount pod；同 pod 多租户场景不支持 |
| 11 | 与 `dirQuotas` 的关系 | 独立 / 联动 | 独立：QoS 限速，quota 限容量，互不扣减 |
| 12 | 鉴权方式 | OIDC / API key | OIDC 为主，API key 供自动化；token 可轮换 |

## 2.11 与现有补丁的兼容

- 现有 `--qos-*` 参数、`.control` 1009、`/api/qos` 保留为**单机调试/平台运维**入口，不参与多租户策略下发；
- 无 `--qos-policy-url` 时行为与当前分支完全一致（AC8）；
- 快照格式带 `version`，老客户端忽略未知字段；
- 发布顺序：先客户端（忽略策略），再控制面，最后打开开关。
