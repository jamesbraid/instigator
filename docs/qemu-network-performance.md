# QEMU private-network performance

Small installer reads need not become small host socket writes. The shell
groups whole `dd` input blocks into approximately 32 KiB transfers while
preserving count, skip, record accounting, and foreground cancellation.
The QEMU stream writer combines length prefixes and frames in reusable storage
and drains at most 32 already-queued frames per write. It flushes when the queue
empties or reaches that limit, without waiting for more packets.

The frame reader reuses scratch storage. gVisor copies each received frame into
its own packet storage before the next read, so queued packets retain distinct
contents. Frame size validation precedes allocation. A complete header with a
missing body is a truncated frame, rather than clean EOF.

## Observed QEMU/IRIX comparison

An Apple M2 Max laptop running macOS 15.8 and Go 1.26.5 compared original source
`e39a21e25f982274a9ab1c0fdddf2fe18e6d373b` with these optimizations on 2026-10-03.
Each headless Origin 200 guest had one R10000 CPU, 128 MiB RAM, RAD4, and its own
fresh 8 GiB disk. Both installed identical package selections from hash-checked
local media over a loopback TCP QEMU stream.

| Measurement | Original | Optimized |
| --- | ---: | ---: |
| Package phase wall time | 1,665.01 s | 1,370.81 s |
| Instigator process CPU during package phase | 961.28 s | 262.91 s |
| Package `dd` stdout calls | 45,621,498 | 719,899 |
| Median 16 MiB guest transfer | 25.30 MB/s | 31.88 MB/s |
| Median small response | 21.52 ms | 21.73 ms |

Both package phases completed the same 1,256 `dd` commands with identical
per-command byte counts and exit statuses. Three alternating transfer pairs
fetched the same 16 MiB media prefix through installed userland before reboot.
All six guest checksums and lengths matched the independently extracted host
prefix. Both guests verified the RAD4 helper transfer, rebuilt their kernels,
and rebooted to serial login.

These are bounded observations. The installations ran concurrently while
another guest remained active. Server stdout totals include file tails that
the installer may not consume. Five small-response samples cannot establish
tail latency. Local staging excludes HTTP fetch costs. Both installers logged
the same WorkShop chroot error and initial RAD4 autoconfig failure. Finalization
built the kernels successfully. Desktop operation and networking after the
installed systems booted were not tested.

The comparison predates the foreground SIGINT fix in
`03198f31a57ab9966d713a5d86e0cbdb1042c052`. That fix avoids sending unneeded file
tails and can change installation cost independently. The shipping code combines
both changes, with cancellation tests covering blocked grouped writes and the
following seek and shell marker. The table does not estimate the incremental
gain over the SIGINT fix.

## References and further experiments

Go's generic [`io.Copy` buffer](https://github.com/golang/go/blob/go1.26.8/src/io/io.go)
uses 32 KiB after checking specialized interfaces. Pinned gVisor's
[`fdbased` send path](https://github.com/google/gvisor/blob/be2bc9cda014/pkg/tcpip/link/fdbased/endpoint.go)
batches packets, and its
[`channel` queue](https://github.com/google/gvisor/blob/be2bc9cda014/pkg/tcpip/link/channel/channel.go)
provides both blocking and nonblocking reads. These are reference patterns,
not evidence that a particular buffer size is optimal here.

Scatter/gather writes and owned receive storage were also measured on concrete
TCP and Unix sockets. Their additional throughput gains were small and variable,
so neither is included. Direct unbuffered receive was slower. The retained
implementation uses contiguous batches and buffered receive scratch.

## Reproduce adapter measurements

The benchmarks transfer byte-checked data through the shell and production
frame pumps. Both endpoints use Go netstack, so they measure adapter cost rather
than QEMU or IRIX performance.

```sh
go test ./internal/qemunet -run '^$' -bench BenchmarkShellTransfer -benchmem -benchtime=100x
go test ./internal/instcmd -run '^$' -bench BenchmarkShellDD -benchmem -benchtime=100x
```
