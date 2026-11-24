# 多租户 QoS 演进设计

## 0. 文档状态与范围

- 基线：社区版 `v1.3.0`（tag `30190ca`）+ 本仓库 `qos` 分支的客户端 QoS 实现（见 [03-implementation.md](03-implementation.md)）。
- 本文目标：把“单挂载进程内的 QoS”演进为**多租户、目录级、跨挂载客户端共享**的 QoS 能力。
- 范围外（暂不做）：元数据透明代理、对象存储前置代理、跨地域调度。本文假定租户 Pod 只能通过平台托管的 mount pod 访问卷，租户不持有元数据地址与对象存储凭证。

## 1. 需求与硬约束

| 编号 | 需求 | 推导出的硬约束 |
|---|---|---|
| R1 | 平台为每个租户分配一个卷级 JuiceFS，并设定卷级 QoS（带宽、数据 IOPS、元数据 IOPS） | 卷级 quota 是全局上限，跨所有挂载客户端共享 |
| R2 | 租户管理员可以把卷挂载到多个客户端，并为子目录设定 QoS；所有客户端共享同一份子目录设定 | 额度记账必须是集中式的，不能按进程各算各的 |
| R3 | 最终用户不能修改 QoS；租户管理员只能在自己的租户额度内分配子目录，不能修改租户总量 | 控制面必须有角色与预算校验，且客户端不可绕过 |
| R4 | 所有子目录的 QoS 之和不能超过租户总量；每个子目录只能使用自己的配额，不能借用 | 严格分区（strict partition）：子目录额度是预留，不随其他目录空闲而增加 |

由 R2/R3 可推出：**执行点必须在平台控制的一侧**。客户端二进制如果由租户掌控，任何限流都可以被替换或忽略；因此执行点放在平台托管的 mount pod 内（VFS 层），租户只接触 FUSE 挂载点。

## 2. 角色与权限矩阵

| 角色 | 可读 | 可写 | 明确不可写 |
|---|---|---|---|
| 平台管理员 | 全部租户策略、预留、用量、审计 | 租户卷级 QoS（租户总量）、审批提额、强制收缩 | — |
| 租户管理员 | 本租户的卷级额度、已预留/剩余、子目录规则与用量 | 本租户内的子目录 QoS 规则（不超过剩余预算）；提交提额申请 | 租户总量、其他租户、mount pod 控制面、元数据凭证 |
| 最终用户 | （可选）自己目录的 effective QoS | 无 | 所有 QoS 写操作 |

租户管理员的写操作由平台网关鉴权（OIDC/JWT 或租户专属 API key），mount pod 不向租户暴露任何控制接口。

## 3. 策略模型

### 3.1 策略树

```
tenant（= 一个卷）
  └─ 根规则：卷级 quota（平台管理员设定）
       ├─ 目录规则 A（inode 123）：bandwidth=50M, iops=500
       ├─ 目录规则 B（inode 456）：bandwidth=100M
       └─ …（可嵌套）
```

- 目录规则**按 inode 存储**，设置时把路径解析为 inode；`rename` 后规则跟随目录，不会产生“规则孤儿”。
- 运行时按“最近祖先规则”匹配；根规则即租户总量。
- 规则变更对已打开的数据句柄在下次 `Open` 生效；元数据操作按 inode 实时匹配（带缓存，rename 失效）。

### 3.2 受限维度

| 维度 | 说明 |
|---|---|
| `bandwidth` | 数据带宽总量（字节/秒） |
| `readBandwidth` / `writeBandwidth` | 方向覆盖；设置后该方向使用独立额度，不再消耗总桶 |
| `iops` | 数据 IOPS（每次 read/write/copy 至少 1 个 token） |
| `readIOPS` / `writeIOPS` | 方向覆盖 |
| `metaIOPS` | 元数据操作总量 |
| `metaOps` | 元数据分类桶：`lookup`、`getattr`、`create`、`unlink`、`readdir`、`statfs`、`other` |

分类桶与总桶**叠加扣减**：一次操作同时需要总桶与对应类别桶的 token，保证“总量不超”且“某类不饿死其他类”。内部递归调用（如 `Create` 因 `O_EXCL` 触发 `Unlink`、`SetAttr` 触发 `Truncate`、`Open` 内部 `GetAttr`）通过 context 标记跳过类别桶，避免一次用户操作重复计费。

### 3.3 预算记账（满足 R4 的“准入”部分）

```
tenant.cap         = { bandwidth, iops, metaIOPS, ... }
tenant.reserved    = Σ 子目录规则（每个维度分别求和）
tenant.available   = cap − reserved
```

- 租户管理员新增/调大子目录规则时校验 `reserved + Δ ≤ cap`；超出返回 `409 insufficient_budget`，并提示“需向平台申请提额”。
- 不允许超额预留（overcommit）：`reserved ≤ cap` 恒成立。
- 删除/调小规则释放预算；调小 `cap` 时若 `cap < reserved`，必须先收缩子规则，或由平台执行“强制收缩”流程（逐条确认、审计）。
- 并发编辑使用版本号与 `If-Match` 乐观锁，避免两个租户管理员同时分配导致超卖。

准入校验只是“早失败”；**运行时由层级令牌桶保证**（见 4.2），即使配置校验被绕过也不会超总量。

### 3.4 严格分区

- 子目录额度是**预留**，不是“上限 + 可借额度”；子目录空闲的额度不会转移给其他子目录。
- 因此“上限”是可承诺的：租户总量 = 各子目录预留之和（≤ cap）。
- 代价是利用率：预留但未使用的额度闲置。这是本设计接受的产品取舍（R4 明确要求严格区分）。

## 4. 执行架构

```
                    ┌─────────────────────────────┐
                    │ 平台控制面                    │
                    │ 策略树 / 预算 / 审批 / 审计    │
                    └──────────────┬──────────────┘
                                   │ 策略快照（版本号 + 签名）
                                   ▼
租户 Pod ──FUSE──▶ 平台托管 mount pod（juicefs）
                    │  VFS 门控：waitMeta / waitData / waitOp
                    │  本地令牌缓存（只缓存“租到的”额度）
                    │        │ refill（scope, dim, n）
                    │        ▼
                    │  集中计数服务（层级令牌桶）
                    │   ├─ 租户根桶（跨 mount pod 共享）
                    │   └─ 目录桶（refill 时从父桶扣减）
                    └─────────────────────────────┘
```

### 4.1 执行点

- 沿用现有 VFS 门控点（元数据方法 + 数据 Read/Write/CopyFileRange），由平台托管的 mount pod 执行。
- 仅覆盖 `mount` 模式；`gateway`/`webdav`/`mdtest`/`object` 走 `pkg/fs`，不经过 VFS，多租户平台不应把它们作为租户入口（或后续在 `pkg/fs` 增加同一套门控）。

### 4.2 层级令牌桶与严格分区

- 每个维度一棵树：租户根桶 → 目录桶。
- **子桶 refill 必须从父桶扣减**：`grant_child = min(request, child_available, parent_tokens)`，并原子地扣减父桶。由此构造性保证 `Σ 子目录实际用量 ≤ 租户总量`，与 3.3 的准入校验互为双保险。
- 严格分区：子桶的额度只来自自己的预留；refill 时不会使用兄弟目录的额度。
- 跨 mount pod 共享：所有 pod 的 refill 都打到同一份租户根桶/目录桶，扣减在计数服务内原子完成。
- 公平性：同一父下多个子目录按权重分配父令牌（默认等权）；严格分区下不需要“借用”逻辑。

### 4.3 Lease / quantum

- 为避免每次 I/O 都走一次网络 RTT，mount pod 按 quantum 批量租用额度（建议 10–50ms 的量，例如带宽按 `rate × quantum`，IOPS 按 `rate × quantum` 向上取整）。
- 本地桶只存放“已租到的”token，用尽后再次 refill；本地桶为空时请求阻塞（或按失败语义返回 `EAGAIN`/`EINTR`）。
- 超发上界 ≈ 并发 pod 数 × quantum × rate；对硬上限要求高的租户可调小 quantum。
- 租约带过期时间，pod 崩溃后未用完的额度在 TTL 后回收。

### 4.4 数据路径与元数据路径

- 数据：`Open` 时解析 inode 的最近祖先规则并绑定到 handle；`Read`/`Write`/`CopyFileRange` 按该 scope 计费；`rename` 后 handle 仍按 inode 归属，规则随 inode 走。
- 元数据：每次操作按 inode 查最近规则（带 inode→rule 缓存），`rename` 时失效被移动子树与目标父目录的缓存。
- 失败语义：限流取消（`ctx.Canceled()`）返回 `EINTR`；计数服务不可用时的行为见第 6 节。

## 5. 控制面 API 草案

鉴权在平台网关完成（租户身份 + 角色），mount pod 只读策略快照。

```http
GET    /api/v1/tenants/{t}/qos                 # cap、reserved、available、规则列表
POST   /api/v1/tenants/{t}/qos/rules           # 新增子目录规则
PATCH  /api/v1/tenants/{t}/qos/rules/{id}      # 调整
DELETE /api/v1/tenants/{t}/qos/rules/{id}      # 删除（释放预算）
POST   /api/v1/tenants/{t}/qos/requests        # 申请提高租户 cap（转平台审批）
```

```json
// POST /api/v1/tenants/t1/qos/rules
{ "path": "/run/sczx001",
  "limits": { "bandwidth": "50M", "iops": 500, "metaIOPS": 100 } }

// 201 Created
{ "id": "r-7f3a", "ino": 123456, "limits": {...}, "version": 12 }

// 409 超预算
{ "error": "insufficient_budget",
  "available": { "bandwidth": 30000000 },
  "requested": { "bandwidth": 52428800 } }

// 最终用户调用：403
```

- 审计字段：操作者、租户、路径/inode、旧值→新值、版本号、时间。
- 策略快照：控制面生成带版本号与签名的快照，mount pod 拉取后校验签名；快照过期时保留上一版并告警。

## 6. 失效与降级

| 场景 | 行为 |
|---|---|
| 计数服务不可用 | 默认 fail-open（放行 + 告警），租户可配置 fail-closed；fail-open 期间本地租约用尽后不再限额 |
| 策略快照过期 | 继续使用上一版；控制面告警 |
| mount pod 重启 | 重新拉取策略；本地租约清空 |
| 计数服务重启 | 租约 TTL 过期后重新分配；期间允许短暂超发 |
| 时钟 | 计数服务与客户端均使用单调时钟计算窗口 |

## 7. 与现有补丁的差异

| 现有实现 | 演进后 |
|---|---|
| 每挂载进程五组本地令牌桶 | 本地桶降级为“租约缓存”；新增 `LeaseLimiter` 客户端 |
| 配置来自 flags / `--qos-config` / Format | 配置来自控制面策略快照；flags 仅保留“平台调试用”的本地覆盖 |
| `.control` 1009 + HTTP 对操作者开放 | 仅平台管理网段/身份可调用；租户不可达 |
| 无目录维度 | 新增 inode 规则树、最近祖先解析、rename 失效 |
| 无层级关系 | 子桶 refill 消耗父桶；准入校验 `reserved ≤ cap` |
| 总桶 + 方向桶 | 增加读/写 IOPS 与元数据分类桶 |
| 单进程额度 | 租户根桶跨 mount pod 共享；严格分区 |

保留的部分：VFS 门控点、`qos.Update` 的局部合并语义、`pkg/qos` 的桶实现与 HTTP 处理（作为平台内部接口）。

## 8. 分阶段计划

| 阶段 | 内容 | 交付 |
|---|---|---|
| P0 | 策略模型与 inode 规则解析；门控改用策略快照；控制面 API 骨架 | 设计落地，单租户可用 |
| P1 | 集中计数服务 + 层级 refill + 严格分区；租约协议 | 跨 mount pod 共享与硬上限 |
| P2 | 读/写 IOPS、元数据分类桶；预算准入与并发校验 | 维度完整 |
| P3 | 审批流、租户自助 API、审计与用量看板 | 运营闭环 |

## 9. 测试策略

- 不变量：任意时刻 `Σ 子目录用量 ≤ 租户用量 ≤ cap`；子目录之间无借用；`reserved ≤ cap`。
- 超发边界：并发 pod 数 × quantum × rate 的理论上界内实测验证。
- rename：移动子树后规则归属正确、缓存失效。
- 故障注入：计数服务重启/网络分区/策略快照过期/租约 TTL。
- 兼容：未启用 QoS 的挂载零开销；旧客户端忽略未知策略字段。

## 10. 风险与未决

1. **凭证与网络隔离是前提**：如果租户能拿到元数据地址/对象存储 AK，或能运行自己的 juicefs 客户端，QoS 就只是“君子协定”。透明代理暂不做，但必须通过 K8s Secret 管控、NetworkPolicy、最小权限凭证来保证租户无法直连。
2. **严格分区的利用率**：预留未用即闲置，需在租户侧提供直观的“已预留/剩余”视图，避免管理员过度预留。
3. **元数据分类的语义**：`readdir` 一次可能返回大量条目但只计 1 个 token；需要在文档中明确，避免用户误判。
4. **计数服务热点**：租户根桶是全局热点，需评估 QPS 与分片（按维度分键、批量 refill）。
5. **共享 mount pod 的归因**：一个 mount pod 服务多个租户时，只能按卷归因；若需 per-Pod 公平，建议 per-tenant 独立 mount pod。

## 11. 社区对标

| 机制 | 借鉴点 |
|---|---|
| OpenStack Cinder QoS | 维度模型（total/read/write × bytes/iops）；策略在卷类型、执行在挂载点 |
| Ceph RBD QoS | per-image 配置 + 运行时 setter；总量与方向桶的组合 |
| Ceph mclock | 命名 profile 与层级配置；避免直接暴露全部旋钮 |
| Kubernetes LimitRange/ResourceQuota | 默认值 + 显式覆盖；聚合配额必须集中执行 |
| Envoy local rate limit | 数据面执行、控制面下发；admin 接口独立 |
| Redis CONFIG SET/REWRITE | 运行时与持久化分离 |
| HDFS | 全局限速放在服务端；客户端侧只是近似 |
| Lustre NRS TBF | 按客户端限流的常见做法与“无全局聚合”的局限 |
