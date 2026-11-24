# qosctl 命令行工具

`qosctl` 是 QoS 控制面的 Go 命令行客户端，功能与 `juicefs-multitenant-qos-test/qos.sh` 对齐：读写 `qosd` 中某个卷的策略，并在本机通知挂载进程立即拉取。

- 源码：`cmd/qosctl/main.go`
- 构建：`make juicefs-qosctl`（`CGO_ENABLED=0`，无 cgo/FUSE/元数据引擎依赖）
- 协议：与 [07-qosd-api.md](07-qosd-api.md) 完全一致（HTTP + Bearer token）

## 1. 全局参数

| 参数 | 环境变量 | 默认值 |
|---|---|---|
| `--server` | `JFS_QOSD_URL` | `http://127.0.0.1:19567` |
| `--token` | `JFS_QOSD_TOKEN` | 空 |
| `--volume` | `JFS_QOS_VOLUME` | 空：服务中只有一个策略时自动选它，否则报错 |

## 2. 命令

| 命令 | 说明 |
|---|---|
| `qosctl show` | 输出某卷的完整策略 JSON |
| `qosctl list` | 以人类可读表格输出租户 cap 与目录规则 |
| `qosctl cap [limits...]` | 修改租户 cap（字段级稀疏更新） |
| `qosctl set PATH [limits...]` | 新建或更新目录规则（路径相对挂载根） |
| `qosctl del PATH` | 删除目录规则 |
| `qosctl reload` | 向本机所有该卷的挂载进程发送 `SIGUSR1` |
| `qosctl check [--script P]` | 运行验收脚本（默认 `./test.sh`） |
| `qosctl version` | 版本 |

限流参数（`0` 表示显式清除该维度）：

```text
--bandwidth --read-bandwidth --write-bandwidth
--iops --read-iops --write-iops --meta-iops
--meta-lookup --meta-getattr --meta-create --meta-unlink --meta-readdir --meta-statfs
```

速率支持 `B/K/M/G/T/P` 后缀（1024 进制），不写后缀按字节/秒处理。

## 3. 示例

```bash
export JFS_QOSD_URL=http://127.0.0.1:19567
export JFS_QOSD_TOKEN=demo-token
export JFS_QOS_VOLUME=mt-demo       # 单策略服务可省略

# 查看
qosctl show
qosctl list
# tenant t1  volume mt-demo  version 10
#   cap                      bandwidth=100MiB iops=5000 metaIOPS=2000
#   /proj-a                  bandwidth=20MiB iops=- metaIOPS=50
#   /proj-b                  bandwidth=10MiB iops=- metaIOPS=-

# 修改租户总量（只改给出的维度）
qosctl cap --bandwidth 120M --meta-iops 3000

# 新建/更新目录规则
qosctl set /proj-a --bandwidth 20M --meta-iops 50
qosctl set /proj-c --bandwidth 5M --meta-lookup 20

# 删除规则、清掉某个维度
qosctl del /proj-c
qosctl set /proj-a --bandwidth 0

# 立即通知本机挂载进程（不等 3 秒轮询）
qosctl reload
```

输出示例：

```text
  [INFO] policy for mt-demo saved as version 12
  [INFO] SIGUSR1 -> 93078 (/Users/taylor/juicefs-mt-demo/mnt)
  [INFO] policy reload signalled to 1 mount process(es); mounts also poll every 3s
tenant t1  volume mt-demo  version 12
  cap                      bandwidth=120MiB iops=5000 metaIOPS=2000
  /proj-a                  bandwidth=20MiB iops=- metaIOPS=50
```

## 4. 实现要点

- **读取—修改—写回**：`cap/set/del` 先 `GET` 最新策略，在本地做稀疏合并，再 `PUT` 整体替换；版本号由服务端分配，避免并发写入时版本回退。
- **规则 id**：与 `qos.sh` 一致，`id` 缺省为 `rule-<path 中的 / 换成 ->`（根目录为 `rule-root`）；`del` 按规范化后的 path 匹配。
- **SIGUSR1 只发给 daemon**：挂载进程的 supervisor 与 daemon 共享命令行，工具从 `mount` 表枚举 `JuiceFS:<volume>` 挂载点，再在进程列表中找出“父进程也是挂载进程”的那个（daemon）；若误发给 supervisor 会将其终止。
- **跨平台**：Linux 读取 `/proc/<pid>/cmdline` 与 `/proc/<pid>/stat`；macOS 使用 `ps -axo pid=,ppid=,command=`。
- **失败重试**：HTTP 请求由 `qosd` 侧保证幂等（整体替换），`qosctl` 本身不重试，失败直接返回非 0；挂载进程侧的拉取与租约重试由 `--qos-retries` 控制。

## 5. 与 qos.sh 的差异

| 能力 | qos.sh | qosctl |
|---|---|---|
| 读策略 / 列表 | `show` / `list` | `show` / `list` |
| 修改 cap / 规则 | `cap` / `set` / `del` | 同名命令 |
| 立即生效 | 发 SIGUSR1 并扫描日志确认版本 | 发 SIGUSR1，不读日志（3 秒轮询兜底） |
| 日志展示 | `show` 附带最近 "QoS policy" 日志 | 不展示（可用 `journalctl`/挂载日志查看） |
| 依赖 | bash + curl + python3 | 单个静态二进制 |
| 运行验收 | `check` 执行 `test.sh` | `check [--script PATH]` |

两者可以混用：策略存在 `qosd`/Redis 中，命令只是读写同一份数据。
