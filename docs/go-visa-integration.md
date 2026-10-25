# go-visa 下一阶段接入设计

本阶段只交付 go-asrl 和本文，不修改 go-visa 的正式 API、资源解析或驱动。

## 本次核对结果

2026-10-25 已读取相邻工作区的 go.mod、transport 接口、capability、operation、resource spec/parser、manager 和 Session 相关路径。

- 本地 go-visa HEAD 为 `2b76f9ee73efc1738e3ade77b96e62bb6032161b`，与任务提供的远程 main 基线 `c897fdc1064ec9c3570592f5bd8d8d157fbc2223` 不同。
- 本地状态已有 `go.mod`、`go.sum` 修改；本次没有写入该目录。
- 还通过 GitHub API 重新读取远程 main 的 go.mod、transport.go、capability.go、operation.go、session.go 和 session_idle.go，对应行为与任务描述一致。API 返回的是文件 blob SHA，未将它伪称为 main 提交 SHA，也未对本地执行 pull/reset。
- 远程与本地 go.mod 所见均为 Go 1.25，依赖 go-hislip v1.0.2、go-vicp v1.0.4、go-vxi v1.0.1。

远程核对入口：[go.mod](https://github.com/jkbhuan/go-visa/blob/main/go.mod)、[连接接口](https://github.com/jkbhuan/go-visa/blob/main/internal/transport/transport.go)、[能力](https://github.com/jkbhuan/go-visa/blob/main/internal/transport/capability.go)、[未开始标记](https://github.com/jkbhuan/go-visa/blob/main/internal/transport/operation.go)、[Session](https://github.com/jkbhuan/go-visa/blob/main/session.go)、[idle 读取](https://github.com/jkbhuan/go-visa/blob/main/session_idle.go)。正式接入时仍须按当时的目标分支重新检查。

## 驱动与能力适配

新增 go-visa 的 `internal/driver/asrl`。只有适配器导入独立 go-asrl；go-asrl 不能反向导入 go-visa 的 internal 包或依赖 go-visa。

适配器的核心形态如下，留待下一阶段在 go-visa 内编译和测试：

```go
type conn struct { port *asrl.Port }

func (c *conn) Write(ctx context.Context, p []byte) (int, error) {
    return c.port.WriteContext(ctx, p)
}

func (c *conn) Read(ctx context.Context, p []byte) (int, bool, error) {
    n, err := c.port.ReadContext(ctx, p)
    return n, false, err
}

func (c *conn) ReadWithDeadline(ctx context.Context, p []byte, deadline time.Time) (int, error) {
    return c.port.ReadWithDeadline(ctx, p, deadline)
}

func (c *conn) SetTimeout(d time.Duration) { c.port.SetTimeout(d) }
func (c *conn) Close() error { return c.port.Close() }
```

Read 的 `end` 始终为 false，包括部分读取、CR/LF、提示符、暂时无数据、超时和关闭。串口没有原生消息 END。

上述方法结构上满足 Conn、TimeoutSetter 和 DeadlineReader。ReadWithDeadline 的零 deadline 必须原样传下去，不能重新加默认超时；context 仍约束等待。适配器保留所有实际 n、context 原因和 OperationNotStarted 标记，错误增加上下文时使用 `%w` 或可 Unwrap 的包装。

不要根据 n==0 给下层错误追加未开始标记：零字节的在途写也可能已触及设备。只在适配器确实尚未进入下层 I/O 时添加该结构标记；部分写后失败不能按“未发送”重试。

## 资源、配置与注册

| 路径/概念 | 下一阶段变化 |
| --- | --- |
| internal/resource/spec.go | 新增 ProtocolASRL；把串口逻辑编号与明确 OS 设备路径作为独立字段，不复用 TCPIP Host/Port/Board 含义 |
| internal/resource/parser.go | 解析明确支持的 ASRL 语法，初始目标 `ASRL2::INSTR`；校验资源类和编号，错误包含原资源 |
| 串口选项 | 表达 BaudRate、DataBits、Parity、StopBits、FlowControl、Timeout 和初始线路状态；未指定值与无限超时分开 |
| internal/driver/asrl | 将资源和选项转换成 OS 端口名、go-asrl Config；提供能力适配及错误原因保留 |
| registry / manager.go | 注册 ProtocolASRL 与新驱动，加入默认 manager 和相应打开选项，不影响现有协议 |
| 错误层与测试 | 增加错误映射、资源解析、能力、Session 流式分帧与生命周期验证 |

Windows 的拟定规则：`ASRL2::INSTR` → `COM2`，`ASRL10::INSTR` → `COM10`，再由 go-asrl 转成设备路径。串口编号不沿用 TCPIP board 的 0～99 限制；Windows 的零编号、非法数字和不明确形式应显式拒绝或给出平台规则。

Unix 不能把 ASRL2 猜成枚举列表的第二项。可提供用户明确配置的 ASRL 编号到 `/dev/tty...` / `/dev/cu...` 映射，或清晰记录的设备路径选项；路径按平台原样传递。本版 go-asrl 在非 Windows 返回不支持，适配器不能通过字符串映射虚构实际后端。

Open 是同步打开和配置，不能对外声称 context 能中断系统打开过程。超时可在驱动获得端口后经 SetTimeout 明确配置；Session 的零超时不能误变成 Config{} 的默认 5 秒。

## Session 的流式处理

只增加驱动还不足以接入。当前 `session.go` 的 isSocket() 直接检查 ProtocolSocket；`session_io.go` 多个读、Query 和起始状态分支依赖它，`session_idle.go` 同时要求 isSocket 和 DeadlineReader。

应统一表达“原始字节流”能力，或集中判断 SOCKET/ASRL 两类协议，随后逐项审查：

1. `session.go`：消息状态和原始流判断；避免把 ASRL 当作带消息 END 的连接。
2. `session_io.go`：ReadRaw、ReadString、Query、QueryRaw、QueryBinaryBlock、读终止符启用条件、streamBuf 保留和分帧。
3. `ensureQueryStartLocked()`：前一消息或流缓冲未消费时，保留拒绝开始新 Query 的检查；新增协议不能绕过。
4. `session_idle.go`：对具备 DeadlineReader 的原始流开放 idle 读取，单次绝对 deadline 覆盖默认超时，仍服从外层 context 和操作总预算。
5. `session_attribute.go`、选项和错误处理：超时变更能到达端口，终止符由 Session 负责，安静 timeout 不进入误判连接失效的路径。

文本转换、写终止符追加、Query 事务串行化、SCPI definite-length 二进制块解析都留给 Session/上层。go-asrl 仅保留字节，不能再追加或删除一次。console 回显可能与输出混在一起，不在传输适配器里猜测和剥离。

无换行提示符可由上层 idle 收集；idle 只是“安静了某段时间”的启发式结束条件，不证明设备消息已完整，也不能伪造 transport END。

## 错误与能力边界

错误映射按以下顺序处理关闭竞争和恢复决策：先检查明确的主动关闭，再区分 context/总预算、设备/句柄失效、其他 OS 错误及未开始标记。保持底层可识别原因，不能将端口占用或权限不足吞成超时。

- 正常 idle 超时是上层收集结束条件，不等于连接丢失；普通读超时后端口可继续读。
- `ErrClosed` 表示明确终态；`ErrDisconnected` 只用于有 OS 证据的失效，不保证每个 UART 都能感知远端离线。
- 部分写/部分读与错误并存时保留 n，Session 根据实际前缀和事务状态决定恢复，不自动重发。
- 串口输入/输出 discard 不等于 VISA Device Clear；Break 不等于 VISA Trigger；modem 状态不等于仪器状态字节；OS 独占打开不等于仪器 Lock。
- 本版没有 Clear、Trigger、ReadStatusByte、Lock 等能力，适配器不要实现返回假成功的方法。保持能力缺失，由上层返回不支持。

下一阶段应以假的 go-asrl 端口适配器验证 end=false、默认/绝对 deadline、超时可继续、未开始标记、部分结果、Session 流缓冲、Query 起始状态和无换行 idle；真实 COM2 证据作为额外验证，不能替代上述契约测试。
