# CPU Memory Stress

一个用 Go 编写的 CPU / 内存压测工具，用于在指定时间内按目标占用率持续消耗 CPU 资源，并按可分配内存比例预留内存。

当前实现采用更接近成熟压测工具的方式：

- CPU 使用固定 duty cycle 的忙循环，在时间片内持续执行浮点计算
- 内存使用匿名私有 `mmap` 映射，并逐页写入、周期性重触碰，确保页面常驻

默认支持多核心运行：

- 默认 worker 数量等于当前机器的逻辑 CPU 数
- 默认会把 `GOMAXPROCS` 设置为 `-t` 指定的值
- 每个 worker 按固定时间片执行“忙循环 + 休眠”，从而控制目标 CPU 占用率
- 支持按“整机总 CPU 占用率”模式运行，程序会自动识别逻辑核数并拆分 worker
- 支持按“可分配内存百分比”预留内存，并通过匿名 `mmap` 逐页写入确保实际占用
- 会把整机总 CPU / 内存使用率按秒写入可执行文件同级目录的 `<binary_name>.log`
- 可通过 `-no-log` 关闭日志文件输出
- 可通过 `-bg` 让进程在后台持续运行
- 正常运行时不向控制台输出状态，只在参数错误或日志写入失败时输出到 `stderr`

## 适用场景

- 压测机器 CPU
- 压测机器内存
- 验证监控、告警、自动扩缩容
- 模拟高 CPU 负载场景
- 模拟高内存占用场景
- 临时拉高 CPU 利用率进行测试

## 文件说明

- `cpu_memory_stress.go`: 主程序
- `Makefile`: 构建脚本
- `cpu_memory_stress`: 编译后的可执行文件

## 环境要求

- Go 1.22 或更高版本
- Linux

当前仓库的构建方式使用 `go build`。

## 编译

使用 `make`：

```bash
make
```

或直接使用 Go：

```bash
go build -o cpu_memory_stress cpu_memory_stress.go
```

清理产物：

```bash
make clean
```

## 参数

```text
-t threads   worker 数量，默认等于当前逻辑 CPU 数
-p percent   每个 worker 的目标 CPU 占用率，范围 0-100，默认 100
-total pct   整机总 CPU 目标占用率，范围 1-100；自动识别逻辑核数并覆盖 -t/-p
-m percent   目标内存占用率，范围 0-100；按可分配内存百分比分配并常驻
            别名：-mem；单独使用时不会启用 CPU 压测，默认保留 10% 安全余量
-d seconds   运行时长，默认不停止；传 0 表示一直运行直到手动停止
-s slice_ms  控制时间片，单位毫秒，默认 100
-no-log      不输出日志文件
-bg          后台运行并脱离当前终端
-h           显示帮助
```

## 用法示例

单核心、25% 占用、运行 2 秒：

```bash
./cpu_memory_stress -t 1 -p 25 -d 2
```

4 个 worker、满载运行 2 分钟：

```bash
./cpu_memory_stress -t 4 -p 100 -d 120
```

按当前机器全部逻辑核满载运行 5 分钟：

```bash
./cpu_memory_stress -t "$(nproc)" -p 100 -d 300
```

8 个 worker、70% 占用、不传停止时间，持续运行直到手动停止：

```bash
./cpu_memory_stress -t 8 -p 70
```

整机总 CPU 占用率约 65%、不传停止时间，持续运行直到手动停止：

```bash
./cpu_memory_stress -total 65
```

按可分配内存的 30% 占用 1 分钟：

```bash
./cpu_memory_stress -m 30 -d 60
```

同时压 CPU 和内存：

```bash
./cpu_memory_stress -total 70 -m 40 -d 300
```

关闭日志文件输出：

```bash
./cpu_memory_stress -total 70 -m 40 -d 300 -no-log
```

后台运行：

```bash
./cpu_memory_stress -total 70 -m 40 -d 300 -bg
```

整机总 CPU 占用率约 50%、内存占用率 50%，持续运行直到手动停止：

```bash
./cpu_memory_stress -total 50 -m 50
```

## 多核心说明

程序本身已经支持多核心。只要把 `-t` 设置为大于 1 的值，就会启动多个 worker 并并行运行。

例如：

```bash
./cpu_memory_stress -t 8 -p 100 -d 300
```

如果希望使用当前机器全部逻辑核：

```bash
./cpu_memory_stress -t "$(nproc)" -p 100 -d 300
```

如果你只关心“整机总 CPU 占用率”，可以直接让程序自动判断逻辑核数；
不传 `-d` 时会一直运行：

```bash
./cpu_memory_stress -total 65
```

例如在 8 逻辑核机器上，`-total 65` 会自动拆成 5 个 100% worker 和 1 个 20% worker，
总目标占用率约为 `65%`。

如果你只想让程序在指定 CPU 核上运行，可以配合 `taskset`：

```bash
taskset -c 0-7 ./cpu_memory_stress -t 8 -p 100 -d 300
```

这表示：

- 进程只允许运行在 `0-7` 号 CPU 核上
- 同时启动 8 个 worker

## 内存占用说明

`-m` / `-mem` 按“可分配内存”的百分比计算目标值：

- 默认读取 `/proc/meminfo` 的 `MemAvailable`
- 如果进程运行在 cgroup 内，则会再比较 cgroup 剩余空间，取更小的那个
- 默认额外保留 `10%` 安全余量，避免把主机或容器压到完全无可用内存
- 使用匿名私有 `mmap` 分配内存，避免仅依赖 Go 堆
- 分配后会逐页写入，并周期性重触碰页面，避免只占虚拟地址空间、不实际占用物理内存

例如：

- 主机当前 `MemAvailable` 是 `20 GiB`，执行 `./cpu_memory_stress -m 25`，先保留 `2 GiB` 安全余量，可分配约 `18 GiB`，目标约为 `4.5 GiB`
- 如果容器剩余可用内存只有 `1 GiB`，同样执行 `./cpu_memory_stress -m 25`，先保留 `10%` 余量后，目标约为 `230 MiB`

## 停止方式

- 到达 `-d` 指定时间后自动退出
- 不传 `-d` 时会一直运行
- 运行中按 `Ctrl-C` 可立即退出并释放已保留内存
- 也可以发送 `SIGTERM`，行为相同
- 使用 `-bg` 后可通过 `pkill -f cpu_memory_stress` 或 `kill <pid>` 停止

## 日志

- 日志文件路径默认是可执行文件同级目录下的 `<binary_name>.log`
- 例如可执行文件名为 `cpu_memory_stress` 时日志为 `cpu_memory_stress.log`，可执行文件名为 `stress` 时日志为 `stress.log`
- 程序启动后会每秒记录一次整机总 CPU 和总内存使用率
- 日志只保留最新 100 条，超过后会覆盖最旧记录
- 日志内容包含启动、目标配置、采样结果和停止信息
- 传入 `-no-log` 后不会创建或写入日志文件
- 启动日志会记录 `cpu_method` 和 `memory_method`

示例：

```text
2026-06-03T03:30:00Z started cpu_mode=total logical_cpus=8 workers=6 duration=unlimited slice=100ms log_interval=1s
2026-06-03T03:30:00Z total_target=65% worker_targets=[100%,100%,100%,100%,100%,20%]
2026-06-03T03:30:00Z memory_target=40% available_bytes=21474836480 allocatable_bytes=19327352832 reserved_bytes=7730941132 safety_margin_percent=10
2026-06-03T03:30:01Z total_cpu=64.87% total_mem=71.36% mem_used_bytes=15324794880 mem_available_bytes=6150041600 mem_total_bytes=21474836480
2026-06-03T03:30:02Z total_cpu=65.14% total_mem=71.42% mem_used_bytes=15337656320 mem_available_bytes=6137180160 mem_total_bytes=21474836480
2026-06-03T03:35:10Z stopped sink=3954923400080164008
```

## 输出示例

正常运行时控制台无输出。

只有参数错误或日志写入失败这类异常情况才会输出到 `stderr`。

其中 `sink` 只是为了防止编译器过度优化忙循环，本身没有业务含义；它只会写入日志里的 `stopped` 行。
