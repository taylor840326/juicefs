# JuiceFS 客户端 QoS 设计文档

本目录记录 JuiceFS **客户端 QoS** 功能的需求、方案分析与实现说明。功能基线为社区版 `v1.3.0`（tag `30190ca`），以分支 `qos` 的形式提供。

## 文档索引

| 文档 | 内容 |
|---|---|
| [01-requirements.md](01-requirements.md) | 背景、目标、功能/非功能需求、验收标准、范围外 |
| [02-requirements-analysis.md](02-requirements-analysis.md) | 现有机制分析、限流位置选型、动态控制通道选型、风险与约束 |
| [03-implementation.md](03-implementation.md) | 架构、模块与代码位置、数据路径计费规则、CLI/HTTP 协议、测试与限制 |
| [04-evolution-analysis.md](04-evolution-analysis.md) | 多租户演进：角色与预算、目录级策略、严格分区、层级令牌桶与集中计数 |
| [05-multitenant-implementation.md](05-multitenant-implementation.md) | 多租户需求落盘与实现方案：功能/非功能需求与验收标准、scope 解析、租约与计数服务、控制面 API、代码改造清单、里程碑与待决问题 |

## 能力概览

| 维度 | 参数 | 运行时接口 |
|---|---|---|
| 数据带宽（总/读/写） | `--qos-bandwidth`、`--qos-read-bandwidth`、`--qos-write-bandwidth` | `.control` / HTTP / SIGUSR1 |
| 数据 IOPS | `--qos-iops` | 同上 |
| 元数据 IOPS | `--qos-meta-iops` | 同上 |

```bash
# 挂载时设置
juicefs mount -d sqlite3:///data/meta.db /mnt/jfs \
  --qos-bandwidth 100M --qos-iops 2000 --qos-meta-iops 1000 \
  --qos-http-token "$JFS_QOS_HTTP_TOKEN" --metrics 127.0.0.1:9567

# 运行时查看 / 修改（局部更新，未给出的项保持不变，0 表示清除）
juicefs qos /mnt/jfs
juicefs qos /mnt/jfs --bandwidth 50M --meta-iops 500

# HTTP
curl -H "Authorization: Bearer $JFS_QOS_HTTP_TOKEN" http://127.0.0.1:9567/api/qos
curl -X PUT -H "Authorization: Bearer $JFS_QOS_HTTP_TOKEN" \
     -d '{"bandwidth":"30M","iops":1000}' http://127.0.0.1:9567/api/qos

# 配置文件热重载（挂载时带 --qos-config）
kill -USR1 <juicefs-mount-pid>
```

## 状态

- 代码、单元测试、中英文参数文档均已完成；`go vet`、`go test ./pkg/qos` 通过。
- 已知限制：仅覆盖 `mount` 模式（`gateway`/`webdav`/`mdtest`/`object` 走 `pkg/fs`，不经过 VFS）；额度按挂载进程计算；详见 [03-implementation.md](03-implementation.md#限制与后续工作)。
