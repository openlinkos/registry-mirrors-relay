# 压测

简体中文 | [English](../benchmark.md)

用 [`tools/relaybench`](../../tools/relaybench/main.go) 测。它在本机起一个假上游（TLS，开着 HTTP/2，镜像层的内容按规律生成、不占内存），再把 relay 作为子进程启动，依次测：

1. 空闲内存（RSS）；
2. manifest 缓存命中的延迟：先拉一次，再按 digest、按 tag 各请求 5000 次；
3. 单核吞吐：relay 的 `GOMAXPROCS=1`，4 个客户端全速下载 1 GiB 的层，持续 15 秒；
4. 慢客户端：1000 个客户端同时下载，每个只读 32 KiB/s，全部开始后等 15 秒，取 10 次采样里最大的 RSS，减去空闲 RSS 再除以流数。

```bash
go build -o relaybench ./tools/relaybench
CGO_ENABLED=0 go build -o registry-mirrors-relay ./cmd/registry-mirrors-relay
ulimit -n 65536
./relaybench -relay ./registry-mirrors-relay -extra $'proxy:\n  buffer_size: 16KiB\n'
```

## 结果（2026-10-23，v0.1.0 之前）

一台 16 核 Linux 开发机，Go 1.27.3，假上游和压测客户端在同一台机器上、不受 relay 的 `GOMAXPROCS` 限制。

| 项目 | 镜像层走 HTTP/2 | HTTP/1.1，32 KiB 缓冲 | HTTP/1.1，16 KiB 缓冲（默认） | 默认 + `GOMEMLIMIT=150MiB` |
| --- | --- | --- | --- | --- |
| 空闲 RSS | 16.3 MiB | 16.3 MiB | 16.6 MiB | 16.4 MiB |
| manifest 按 digest 命中 p50 / p99 | 0.10 / 0.17 ms | 0.11 / 0.18 ms | 0.10 / 0.15 ms | 0.10 / 0.17 ms |
| manifest 按 tag 命中 p50 / p99 | 0.10 / 0.14 ms | 0.10 / 0.15 ms | 0.09 / 0.14 ms | 0.09 / 0.13 ms |
| 1 万次 manifest 请求时上游收到的 GET | 1 | 1 | 1 | 1 |
| 单核吞吐 | 4.46 Gbit/s | 7.55 Gbit/s | 7.99 Gbit/s | 7.73 Gbit/s |
| 1000 个慢流时的 RSS | 1488.7 MiB | 201.4 MiB | 183.3 MiB | 151.0 MiB |
| 每个流 | 1508 KiB | 189 KiB | 171 KiB | 138 KiB |
| 慢流期间到上游的 TCP 连接 | 5 | 1001 | 1001 | 1001 |

## 结论

- **镜像层默认走 HTTP/1.1。** 走 HTTP/2 时，Go 的客户端会替每个流先收下最多 4 MiB 还没被读走的数据：客户端慢、上游快时，内存随并发线性增长，1000 个慢流用了 1.5 GiB。走 HTTP/1.1 时背压交给 TCP，进程里每个流只占转发缓冲和连接本身的开销；代价是每个下载占一条到上游的连接。manifest 和令牌请求很小，仍走 HTTP/2 复用连接。需要时可以用 `proxy.blob_http2: true` 改回来。
- **转发缓冲默认 16 KiB。** 比 32 KiB 省内存，吞吐也略高（TLS 记录最大就是 16 KiB）。
- **每个流约 140–170 KiB。** 主要是两条连接各自的开销：客户端到 relay 的连接（读写缓冲、协程栈），relay 到上游的 TLS 连接（记录缓冲、读写缓冲、两个协程），再加 16 KiB 转发缓冲。默认 `GOGC` 下 GC 会留一倍左右的余量；在容器里 relay 会按内存上限自动设置 `GOMEMLIMIT`，GC 更积极，RSS 也更低（最后一列）。
- **manifest 缓存命中在 0.2 ms 以内**，单核吞吐远超千兆网卡。

## 真实上游的额度

另外经 relay 从 Docker Hub 完整拉取 `alpine:3.22` 10 次（每次之间删掉本地镜像，间隔 7 秒，跨过两次 tag 过期）：

- Docker Hub 的匿名剩余额度从 73 变成 71，**10 次拉取共消耗 2 次**，都是第一次拉取时按 digest 取 index 和 amd64 manifest。
- relay 日志里的 40 个 manifest 请求：34 个命中缓存，3 个回源 GET（都在第一次），3 次 tag 过期后的确认只发了 HEAD（Docker Hub 不计次数）。
- 不经缓存时，每次拉取至少 2 次 GET，10 次至少 20 次。
