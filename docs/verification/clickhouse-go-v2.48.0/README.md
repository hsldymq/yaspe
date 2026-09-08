# clickhouse-go v2.48.0 Native batch 定向验证

验证日期：2026-09-08。此附件验证原始驱动 batch/connect 方法的局部行为，不是 yaspe
Connector、真实 ClickHouse 服务端或完整 Native 协议兼容性验证。

## 固定基线与复现

- 驱动：github.com/ClickHouse/clickhouse-go/v2 v2.48.0；ch-go v0.74.0 等依赖由原驱动
  go.mod/go.sum 固定，不修改原始生产 Go 文件；
- 模块校验和：`h1:auzd4VkapQYhQF8F2Gog7s3x78Bi1JZmByxGbrw3C+4=`；
- 执行工具链见 [result.txt](result.txt)，开启 race detector；
- [run_probe.py](run_probe.py) 下载/读取缓存中的固定模块，核验校验和，复制到临时目录，
  排除上游测试后加入 [探针源码](yaspe_probe_test.go.txt)，运行七项 TestProbe 测试并清理副本；
- 探针以 `.go.txt` 保存，避免根目录 `go test ./...` 将依赖驱动私有类型的白盒测试误当作
  yaspe 包编译。驱动源代码、模块缓存和构建产物不保存在本仓库。

在本目录运行，首次需要获取固定驱动和依赖：

```sh
python3 run_probe.py
```

脚本内部运行 `go test -mod=readonly -race -v -count=1 -timeout 45s -run '^TestProbe' .`。
测试通过 channel/barrier 在准备响应或 Send 响应处暂停，再注入取消；不以 sleep 碰撞竞态。

## 已观察结果

| 测试 | 局部证据 |
|---|---|
| TestProbeSendWaitsForAcknowledgement | Append 无连接写入；Send 写两行和结束块，收到模拟 EOS 后返回成功 |
| TestProbeCloseDoesNotSendBufferedRows | Close 仅发送结束块，不发送已 Append 的行；IsSent 仍变为 true；重复 Close 不重复释放 |
| TestProbeAbortDoesNotSendRows | Abort 不发送行，以错误状态调用释放路径；不是外部 rollback |
| TestProbeCancelDuringSend | 行已发送但未给确认时取消，返回错误并关闭连接；IsSent 仍为 true |
| TestProbeFreshBatchAfterUnknownAttempt | 首次结果未确认后，使用新 context/new batch 发送相同 UInt64 行，编码字节一致 |
| TestProbePreparePerformsNetworkIO | Prepare 在 Append 前写 INSERT 请求并读取模拟列样本 |
| TestProbeCancelDuringPrepareRead | 等待列样本时取消，返回 context.Canceled 并关闭连接 |

七项通过，执行中 race detector 未报告竞争。完整原始输出见 result.txt。

## 限制

- 白盒直接构造驱动私有 batch/connect，使用可控 net.Conn；没有真实服务器、TCP、
  握手、认证或公共 Conn 的连接池获取/竞争；释放回调也由测试提供；
- 使用无压缩 UInt64 样本和测试用 revision=0 的 block 编码，不证明现代服务器协议、
  复杂类型、schema 变化或业务编码器兼容性；
- 模拟连接的 socket deadline 方法为空实现，因此验证的是 context 取消路径，不能证明
  五秒尝试超时、十秒重试总预算、阻塞 Write 的中断或 Close deadline 的严格执行；
- 未验证真实落库、表引擎、分布式转发、副本/持久化设置、去重、服务端部分成功及异常码；
- 未实现 yaspe 的 Accept、组批、reporter、三次尝试/退避策略、Unknown 历史聚合或关闭
  协调器，也未进行独立 leak detector、长时压力或 benchmark；
- 新 batch 重发相同行数据不代表 exactly-once；模拟 EOS 只验证驱动成功返回路径，不能
  推导任意业务配置下的实际持久化保证。

设计与后续验证要求见 [ClickHouse Design](../../designs/0009-clickhouse-connector.md)。
