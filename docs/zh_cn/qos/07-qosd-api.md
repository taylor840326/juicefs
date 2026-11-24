# qosd 接口说明（多租户 QoS 控制面）

`qosd` 是 JuiceFS 多租户 QoS 的独立控制服务：策略按 **volume** 保存在 Redis 中，通过 HTTP API 增删改查；各挂载进程每 3 秒拉取本卷策略，并用 `/v1/grant` 领取共享令牌租约。

- 代码：`cmd/qosd/main.go`（服务入口）、`pkg/qos/server.go`（HTTP API）、`pkg/qos/store.go`（Redis 存储）
- 二进制：`juicefs-qosd`（`make juicefs-qosd`，`CGO_ENABLED=0` 可交叉编译）
- 客户端参数：`--qos-server`、`--qos-server-token`、`--qos-lease-url`、`--qos-lease-token`、`--qos-retries`、`--qos-fail-open`

## 1. 启动

```bash
juicefs-qosd \
  --listen 127.0.0.1:19567 \
  --redis redis://127.0.0.1:6379/0 \
  --token "$JFS_QOSD_TOKEN" \
  --log-level info
```

| 参数 | 环境变量 | 说明 |
|---|---|---|
| `--listen` | – | 监听地址，默认 `127.0.0.1:19567` |
| `--redis` | `JFS_QOSD_REDIS` | Redis DSN，必填；支持 `redis://host:port/db` 与 `host:port` |
| `--token` | `JFS_QOSD_TOKEN` | Bearer token；为空时不鉴权（仅建议本机开发） |
| `--log-level` | – | `info` / `warn` / `debug`，默认 `info` |
| `--version` | – | 打印版本后退出 |

客户端侧必须使用同一个 token 访问策略与租约接口。

## 2. 认证

除 `GET /v1/healthz` 外，所有接口都要求凭证：

```bash
-H "Authorization: Bearer $JFS_QOSD_TOKEN"
# 兼容写法：
-H "X-JuiceFS-Token: $JFS_QOSD_TOKEN"
```

缺少/错误的 token 返回 `401`，并带 `WWW-Authenticate: Bearer realm="qosd"`。比较使用常量时间实现。

## 3. 数据模型

策略文档（`GET /v1/policies/{volume}` 的返回体，也是 `--qos-policy-file` 的格式）：

```json
{
  "version": 13,
  "tenant": "t1",
  "volume": "mt-demo",
  "epoch": 0,
  "cap": {"bandwidth": 125829120, "iops": 5000, "metaIOPS": 2000},
  "rules": [
    {"id": "proj-a", "path": "proj-a", "ino": 0,
     "limits": {"bandwidth": 20971520, "metaIOPS": 50}},
    {"id": "proj-b", "path": "proj-b", "ino": 12345,
     "limits": {"bandwidth": 10485760, "metaOps": {"lookup": 200}}}
  ]
}
```

| 字段 | 说明 |
|---|---|
| `version` | 服务端在每次写入时通过 Redis `INCR` 分配；客户端用它判断是否需要重载，客户端提交的值会被覆盖 |
| `tenant` | 租户名，必填；同一租户的卷共享 `cap` 预算 |
| `volume` | 卷名；`PUT` 时以 URL 中的 volume 为准 |
| `cap` | 租户总量（所有挂载共享）；字段同 `limits` |
| `rules` | 目录规则列表；`path` 相对挂载根（可带前导 `/`），由客户端解析成 inode；`ino` 非 0 时直接按 inode 生效 |
| `limits` | 限流维度，见下表；`0` 表示未设置 |

`limits` 字段（字节/秒或次/秒）：

| 字段 | 维度 |
|---|---|
| `bandwidth` | 总带宽 |
| `readBandwidth` / `writeBandwidth` | 读/写带宽（设置后覆盖总带宽） |
| `iops` | 总 IOPS |
| `readIOPS` / `writeIOPS` | 读/写 IOPS |
| `metaIOPS` | 元数据操作总数 |
| `metaOps` | 元数据分类：`lookup`、`getattr`、`create`、`unlink`、`readdir`、`statfs` |

规则匹配采用**最近祖先**：对某目录/文件操作时，从它向上找第一条规则；找不到则用租户 `cap`。

## 4. 接口一览

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/v1/healthz` | 健康检查与计数统计（免鉴权） |
| `GET` | `/v1/policies?tenant=t1` | 列出策略（`tenant` 可省略） |
| `GET` | `/v1/policies/{volume}` | 读取单个卷的策略；不存在返回 404 |
| `PUT` | `/v1/policies/{volume}` | 创建或整体替换策略 |
| `DELETE` | `/v1/policies/{volume}` | 删除策略 |
| `POST` | `/v1/policies/{volume}/rules` | 新增一条规则 |
| `PUT` | `/v1/policies/{volume}/rules/{id}` | 替换一条规则 |
| `DELETE` | `/v1/policies/{volume}/rules/{id}` | 删除一条规则 |
| `POST` | `/v1/grant` | 领取共享令牌租约（挂载进程使用） |

### 4.1 健康检查

```bash
curl -s http://127.0.0.1:19567/v1/healthz | jq
```

```json
{
  "counter": "tenants=1 buckets=3 grants=53 denied=0 granted_tokens=104857600",
  "service": "qosd",
  "status": "ok",
  "version": "1.3.0+2025-07-03.30190ca"
}
```

### 4.2 列出 / 读取策略

```bash
# 全部策略
curl -s -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:19567/v1/policies | jq

# 按租户过滤
curl -s -H "Authorization: Bearer $TOKEN" \
  "http://127.0.0.1:19567/v1/policies?tenant=t1" | jq

# 单个卷
curl -s -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:19567/v1/policies/mt-demo | jq
```

列表返回 `{"policies": [...], "count": N}`；单卷返回策略文档本身。

### 4.3 创建 / 整体替换策略

```bash
curl -s -X PUT \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d '{
        "tenant": "t1",
        "cap": {"bandwidth": 104857600, "iops": 5000, "metaIOPS": 2000},
        "rules": [
          {"id": "proj-a", "path": "proj-a",
           "limits": {"bandwidth": 20971520, "metaIOPS": 50}},
          {"id": "proj-b", "path": "proj-b",
           "limits": {"bandwidth": 10485760}}
        ]
      }' \
  http://127.0.0.1:19567/v1/policies/mt-demo | jq
```

服务端行为：

- `volume` 以 URL 为准，`tenant` 必填；
- 规则必须同时有 `path` 或非 0 `ino`，且至少一个限流维度非 0；
- 同一策略内 `path` 不可重复，`id` 缺省时按 path 生成；
- `version` 由服务端分配（Redis `INCR`），返回值即新版本。

### 4.4 增删改单条规则

```bash
# 新增规则（id 可省略，缺省为 path 转换而来）
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"id": "proj-c", "path": "proj-c",
       "limits": {"bandwidth": 5242880, "metaOps": {"readdir": 20}}}' \
  http://127.0.0.1:19567/v1/policies/mt-demo/rules | jq

# 替换规则（body 是完整规则，id 以 URL 为准）
curl -s -X PUT \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"path": "proj-c", "limits": {"bandwidth": 10485760}}' \
  http://127.0.0.1:19567/v1/policies/mt-demo/rules/proj-c | jq

# 删除规则（id 为规则的 id，不是路径）
curl -s -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:19567/v1/policies/mt-demo/rules/proj-c
```

### 4.5 删除策略

```bash
curl -s -X DELETE \
  -H "Authorization: Bearer $TOKEN" \
  http://127.0.0.1:19567/v1/policies/mt-demo
# 204 No Content
```

删除后挂载进程在下一次 3s 轮询看到 404，会清空本卷的策略（回到不限速）。

### 4.6 租约接口（挂载进程使用）

```bash
curl -s -X POST \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d '{"tenant": "t1", "dim": "bandwidth", "want": 10485760,
       "levels": [{"ino": 0, "rate": 125829120}]}' \
  http://127.0.0.1:19567/v1/grant | jq
```

```json
{"granted": 10485760}
```

| 字段 | 说明 |
|---|---|
| `tenant` | 租户 |
| `dim` | 维度：`bandwidth`、`readBandwidth`、`writeBandwidth`、`iops`、`readIOPS`、`writeIOPS`、`metaIOPS`、`meta.lookup` 等 |
| `want` | 期望令牌数（字节或操作数） |
| `levels` | 从当前 scope 到根租户的桶链（`ino` + `rate`） |

服务端从每一层可用令牌中取最小值返回，保证所有挂载领取的令牌总和不超过租户 cap（**严格分区**）。`granted` 可能小于 `want`，客户端的租约层会隔一小段时间再来领取。

## 5. 错误码

| 状态码 | 场景 |
|---|---|
| `400` | JSON 非法、缺少 `tenant`、规则没有 path/ino、规则没有限流维度、缺 `dim`/`levels` |
| `401` | token 缺失或错误 |
| `404` | 策略/规则不存在 |
| `405` | 方法不匹配（带 `Allow` 头） |
| `409` | 规则 id 或 path 重复 |
| `500` | Redis 读写失败 |

错误体统一为 `{"error": "..."}`。

## 6. 版本与生效时序

1. `PUT /v1/policies/{volume}` 成功后，服务端把版本号 `INCR` 并写回 Redis；
2. 挂载进程每 **3 秒** `GET /v1/policies/{volume}`，发现 `version` 变化即重载（`--qos-policy-file` 模式则比较文件 sha256）；
3. 挂载进程收到 `SIGUSR1` 时立即拉取一次，不必等轮询；
4. 重载后：新打开的句柄立即使用新规则；已打开句柄在下次 I/O 时按 epoch 重新解析；正在等待令牌的操作在下一轮检查到 epoch 变化后，用新规则重试；
5. 请求失败按 `--io-retries`（可用 `--qos-retries` 覆盖）重试；fail-open 模式下单次租约最多等 3 秒，失败后 30 秒冷却期内直接放行。

## 7. Redis 键布局

| 键 | 类型 | 内容 |
|---|---|---|
| `jfs:qos:policy:<volume>` | string | 策略 JSON |
| `jfs:qos:version:<volume>` | string | 版本计数器（`INCR`） |
| `jfs:qos:volumes` | set | 已存在的卷名 |
| `jfs:qos:tenants` | set | 已存在的租户名 |

```bash
redis-cli -p 6379 GET jfs:qos:policy:mt-demo | jq
redis-cli -p 6379 GET jfs:qos:version:mt-demo
redis-cli -p 6379 SMEMBERS jfs:qos:volumes
```

`qosd` 重启不会丢失策略；Redis 不可用时策略接口返回 500，客户端保留上一次成功拉取的策略并继续按 3 秒重试。

## 8. 与 qosctl 的对应关系

`qosctl` 是同一套接口的 Go 命令行封装（见 [08-qosctl.md](08-qosctl.md)）：

| qosctl 命令 | 等价接口 |
|---|---|
| `qosctl show` | `GET /v1/policies/{volume}` |
| `qosctl list` | `GET /v1/policies/{volume}`（格式化输出） |
| `qosctl cap ...` / `set ...` / `del ...` | `GET` + `PUT /v1/policies/{volume}` |
| `qosctl reload` | 向本机挂载进程发送 `SIGUSR1` |
