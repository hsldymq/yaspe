# franz-go v1.21.6 定向验证

验证日期：2026-09-07。范围：固定版本客户端的部分 Kafka 协议行为，不是 yaspe Runtime 或
Kafka Connector 的实现验证。对应契约见 [Kafka Design](../../designs/0007-position-and-kafka-rebalance.md)。

## 环境与复现

- Go：`go1.27.0-X:nodwarf5 linux/amd64`；启用 race detector；
- franz-go：v1.21.6；kmsg：v1.13.1；
- kfake：v0.0.0-20260906002741-ab55e0424097；完整依赖与校验和见 [go.mod](go.mod)、[go.sum](go.sum)；
- 一个 kfake broker，通过 `net.Pipe` 传输真实客户端编码的 Kafka 请求/响应，无外部
  Kafka 服务、TCP 网络或磁盘持久化；
- 测试与 barrier 见 [adapter_test.go](adapter_test.go)，实际执行输出见 [result.txt](result.txt)。

在本目录运行：

```sh
go test -race -v -count=1 -timeout 60s ./...
```

本目录是独立 Go module，首次复现需要下载其固定依赖；根目录 `go test ./...` 不包含
这些测试。测试不依赖 `time.Sleep` 碰撞竞态，使用回调、channel 和请求拦截确定注入位置；
等待 barrier 有五秒失败上限，客户端 heartbeat/fetch timeout 使用真实时间。

## 已观察结果

| 测试 | 注入/观察 | 结果支持的范围 |
|---|---|---|
| TestPartialPollRetainsFetchBudgetAndPauseResume | 一份 12 条结果按每次 2 条 poll；中途 pause/resume | 剩余结果留在客户端，未取完前未观察到新 fetch，顺序保持 |
| TestClassicAndRebalanceWindow | 非空 poll 后 ForceRebalance，等待 blocked 通知再 AllowRebalance | 观察到 classic JoinGroup、未观察到 next-gen heartbeat；revoke 等窗口释放，出现空 cooperative revoke |
| TestInFlightCommitCancellation | broker 收到 OffsetCommit 后不回复，再取消调用 context | 单个、无竞争的同步提交返回 context.Canceled |
| TestCommitWaitsForCallbackReturn | 成功提交 callback 被 barrier 阻挡，再取消 context | 同步提交仍等待 callback 返回，取消不能代替 callback 收敛 |
| TestCloseWaitsForRevokeCallback | 从独立 goroutine Close，在 revoke callback 中设 barrier | Close 等待 callback 返回，callback 不应同步关闭自己的客户端 |
| TestLostThenErrorThenReassignmentWithoutPolling | 暂停 fetch、停止 poll，heartbeat 注入 UnknownMemberID | 顺序为 Lost → GroupManageError → Assigned，会话重入不依赖继续业务 poll |
| TestStartupAuthorizationFailureWithoutAnyPoll | 首次 JoinGroup 注入 GroupAuthorizationFailed，完全不 poll | 空 Lost 后由错误 hook 暴露权限错误，支持独立最终失败通知路径 |

七项均通过，race detector 在这些执行中未报告竞争；不将该结果外推为任意交错的证明。

## 证据限制与剩余工作

- 辅助客户端配置 `RequestRetries(0)`、`FetchMaxWait(10ms)`；会话失效测试使用短 heartbeat
  周期。它们是缩小场景的测试配置，不是 yaspe 的生产默认参数；
- 未验证完整五秒逻辑提交预算与退避组合、内部锁竞争下的取消、跨连接旧请求在 broker
  上迟到生效、部分 partition commit 失败后的外部状态；
- 未实现或验证 yaspe 的一分钟恢复 timer、ReportFailure 去重、Runtime admission、
  RevokeHandle/fence、Connector 的 1,024 条缓存或 MaxConcurrentFetches 默认 2 的组合；
- 没有真实 broker、多客户端 partition 转移、完整 eager/cooperative 故障矩阵、网络分区、
  进程重启、资源泄漏检测或生产 workload/benchmark；
- 测试未覆盖大消息、压缩批次和长期背压，不能据此声明固定记录数/内存上限；
- 已确认 callback 阻挡会使同步提交/关闭等待，不能把传入 timeout 直接当作整个调用
  必然按时结束的证明；需由 Connector 保持单请求和快速 callback，再进行剩余核验。

源码补充：v1.21.6 的 `OnPartitionsCallbackBlocked` 通过独立 goroutine 调用，不能把其
实际执行时刻当作可靠的 revoke 起点；设计采用 revoke 回调入口计时。该结论来自
[版本源码](https://github.com/twmb/franz-go/blob/v1.21.6/pkg/kgo/consumer.go)，不是上述测试
对任意调度的测量证明。Lost 返回后再调用错误 hook 的顺序也与
[group 源码](https://github.com/twmb/franz-go/blob/v1.21.6/pkg/kgo/consumer_group.go)一致。
