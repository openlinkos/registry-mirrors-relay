# Benchmarks

[简体中文](zh/benchmark.md) | English

Measured with [`tools/relaybench`](../tools/relaybench/main.go). It starts a fake upstream on this machine (TLS with HTTP/2; layer contents are generated from a pattern and take no memory), starts the relay as a child process, then measures in turn:

1. Idle memory (RSS).
2. Latency of manifest cache hits: one pull first, then 5000 requests each by digest and by tag.
3. Single-core throughput: the relay runs with `GOMAXPROCS=1` while 4 clients download a 1 GiB layer at full speed for 15 seconds.
4. Slow clients: 1000 clients download at once, each reading only 32 KiB/s; 15 seconds after all have started, the largest RSS of 10 samples, minus the idle RSS, divided by the number of streams.

```bash
go build -o relaybench ./tools/relaybench
CGO_ENABLED=0 go build -o registry-mirrors-relay ./cmd/registry-mirrors-relay
ulimit -n 65536
./relaybench -relay ./registry-mirrors-relay -extra $'proxy:\n  buffer_size: 16KiB\n'
```

## Results (2026-10-23, before v0.1.0)

A 16-core Linux development machine, Go 1.27.3; the fake upstream and the benchmark clients run on the same machine and are not limited by the relay's `GOMAXPROCS`.

| Measure | Layers over HTTP/2 | HTTP/1.1, 32 KiB buffer | HTTP/1.1, 16 KiB buffer (default) | Default + `GOMEMLIMIT=150MiB` |
| --- | --- | --- | --- | --- |
| Idle RSS | 16.3 MiB | 16.3 MiB | 16.6 MiB | 16.4 MiB |
| Manifest hit by digest, p50 / p99 | 0.10 / 0.17 ms | 0.11 / 0.18 ms | 0.10 / 0.15 ms | 0.10 / 0.17 ms |
| Manifest hit by tag, p50 / p99 | 0.10 / 0.14 ms | 0.10 / 0.15 ms | 0.09 / 0.14 ms | 0.09 / 0.13 ms |
| GETs reaching the upstream for 10,000 manifest requests | 1 | 1 | 1 | 1 |
| Single-core throughput | 4.46 Gbit/s | 7.55 Gbit/s | 7.99 Gbit/s | 7.73 Gbit/s |
| RSS with 1000 slow streams | 1488.7 MiB | 201.4 MiB | 183.3 MiB | 151.0 MiB |
| Per stream | 1508 KiB | 189 KiB | 171 KiB | 138 KiB |
| TCP connections to the upstream during the slow streams | 5 | 1001 | 1001 | 1001 |

## Conclusions

- **Layers use HTTP/1.1 by default.** Over HTTP/2, Go's client buffers up to 4 MiB of unread data per stream: with slow clients and a fast upstream, memory grows linearly with concurrency, and 1000 slow streams took 1.5 GiB. Over HTTP/1.1, back-pressure is left to TCP, and each stream costs only the forwarding buffer and the connection itself; the price is one upstream connection per download. Manifest and token requests are small and still share connections over HTTP/2. `proxy.blob_http2: true` switches back when needed.
- **The forwarding buffer is 16 KiB by default.** It uses less memory than 32 KiB and gives slightly higher throughput (16 KiB is the largest TLS record).
- **About 140–170 KiB per stream.** Mostly the overhead of the two connections: the client's connection to the relay (read and write buffers, a goroutine stack) and the relay's TLS connection to the upstream (record buffers, read and write buffers, two goroutines), plus the 16 KiB forwarding buffer. With the default `GOGC` the GC leaves about as much again as headroom; in a container the relay sets `GOMEMLIMIT` from the memory limit, so the GC works harder and RSS is lower (last column).
- **Manifest cache hits take under 0.2 ms**, and single-core throughput is far beyond a gigabit network card.

## Quota on a real upstream

Separately, `alpine:3.22` was pulled in full from Docker Hub through the relay 10 times (removing the local image between pulls, 7 seconds apart, so that the tag expired twice):

- Docker Hub's anonymous quota left went from 73 to 71: **10 pulls used 2**, both on the first pull, fetching the index and the amd64 manifest by digest.
- Of the 40 manifest requests in the relay's log, 34 were cache hits, 3 were GETs to the upstream (all on the first pull), and the 3 checks after the tag expired sent only HEAD (which Docker Hub does not count).
- Without the cache, every pull needs at least 2 GETs, so 10 pulls need at least 20.
