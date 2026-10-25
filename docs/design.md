# 设计与最终契约

## 边界与依赖选择

本模块是串口传输层，不依赖 go-visa，不实现消息 END、SCPI、终端仿真、登录或资源发现。Go module 为 `github.com/jkbhuan/go-asrl`，包名 `asrl`，最低 Go 1.25.0。

开发前检查发现目录仅有任务文件，未发现适用的 `AGENTS.md` 或既有 Go module；未初始化 Git 或创建远程仓库。相邻 go-visa 的修改保持原样，核对详情见接入文档。

评估候选版本 `go.bug.st/serial v1.6.4`：

| 项目 | 核对结果 |
| --- | --- |
| 许可证 | BSD-3-Clause；已读取该 tag 的 LICENSE |
| Go / 依赖 | go.mod 为 Go 1.17，Windows 使用 x/sys，无需 VISA 或 CGo |
| 原始模式 | 源码设置 DCB，关闭 NUL 丢弃、替换及软件流控 |
| 读阻塞 | overlapped ReadFile 后等待 GetOverlappedResult；读超时可返回 `(0,nil)` |
| 写超时 / context | 公开接口只有 SetReadTimeout；Write 等待完成，缺少逐次写预算和指定请求取消接口 |
| Close | Windows 实现关闭句柄；不能仅靠外围 select 保证在途请求已收束 |

依据：[候选 go.mod](https://github.com/bugst/go-serial/blob/v1.6.4/go.mod)、[许可证](https://github.com/bugst/go-serial/blob/v1.6.4/LICENSE)、[Windows 实现](https://github.com/bugst/go-serial/blob/v1.6.4/serial_windows.go)、[公开接口](https://pkg.go.dev/go.bug.st/serial@v1.6.4)。这些是对指定版本的评估，不推断所有未来版本。

最终选择直接使用 Windows 原生 overlapped API，依赖唯一外部模块 `golang.org/x/sys v0.42.0`。已从本机模块缓存核对该版本的 `go.mod`（Go 1.25.0）、Windows 包及 BSD-3-Clause LICENSE；校验和固定在 go.sum。未复制候选库源码。x/sys 许可证见 [Go 源码](https://github.com/golang/sys/blob/v0.42.0/LICENSE)；不替用户决定本项目自己的发布许可证。

## 配置与平台

`Config` 包含 `BaudRate`、`DataBits`、`Parity`、`StopBits`、`FlowControl`、`Timeout`、`DTR`、`RTS`。零字段规范化为 9600、8、none、1、none、5s，两个线路布尔值为 false。这些默认值只是建议，不能视为路由器确认参数。负的打开超时非法；显式无限超时通过 `SetTimeout(0)` 设置。

非法枚举、负波特率、超出 DWORD 的波特率、数据位范围和停止位组合均在打开前拒绝。5 数据位可用 1 或 1.5 停止位；6～8 数据位可用 1 或 2 停止位。未知流控是参数错误，已知的 rtscts/xonxoff 在 Windows 返回能力错误，不自动降级。

Windows 接受裸 COM 正整数名并转换为 `\\.\COM<number>`，不限单个数字，也不套用 0～99 的 board 范围；拒绝前导零、冒号和嵌入 NUL。已有 `\\.\` / `\\?\` 设备路径保持原样，最终由 OS/串口配置调用判断设备是否有效。

CreateFile 以读写、独占、OPEN_EXISTING、FILE_FLAG_OVERLAPPED 打开。GetCommState 后设置明确的二进制 DCB，再 SetCommState，并读回核对波特率、数据位、校验、停止位和相关 flags。驱动返回 INVALID_PARAMETER / NOT_SUPPORTED 时归类为 `ErrNotSupported` 并保留 OS 原因；其他错误保留原分类。配置读回不一致也返回不支持，不静默继续。

每一个成功获得句柄后的失败路径都关闭句柄；清理失败与原错误一起返回。打开/配置本身为同步系统调用，不提供 `OpenContext`，不承诺可被 context 中断。

只有 Windows 提供实际串口后端；amd64 为首要目标。`!windows` build tag 为 Linux、macOS 等提供可导入、可构建的存根，合法配置的 `Open` 返回 `ErrNotSupported`，不把 Unix 的数字或路径解释为 COM。

## 字节流与并发

Port 通过非导出的 backend 接口分离跨平台的预算、排队、短写和错误包装，以及平台内核请求生命周期。接口可替换，默认测试不访问串口。Windows 打开调用和传输调用各有独立、实例级的可替换函数集合，没有全局可变测试钩子。

读和写各有一个可取消的 channel gate，允许一个读和一个写同时进行。同方向队列不保证公平或业务归属。写持有 gate 直到整个短写循环完成，多个写调用的前缀不交错。所有等待都计入同一预算。

Read 收到正数字节即返回，不要求填满。无进展 `(0,nil)` 在内部等待 2ms 后重试，等待也响应取消/关闭；不会向调用者返回反复的空成功或把安静串口当 EOF。Write 持续累计短写，未完成只会在错误时返回；保留 `n>0` 与错误。系统请求长度最多 1MiB，避免 DWORD 长度截断；公共 Write 自动继续剩余内容。

零长度读写优先返回 `(0,nil)`，包括已关闭端口和已取消 context。其他 context 参数必须非 nil。Port 只能由 Open 获得，不支持复制或将零值作为打开端口使用。

不修改 CR/LF、NUL、高位字节或回显，不自动 purge，不发 Break，不重连、不重发。较少数据位由 UART 的物理格式决定，不能据此承诺 5 数据位能传输完整的八位数值。写成功表示后端接受数据，不表示物理 drain 或设备执行。

## 总预算和错误规则

每个非空 API 调用在排队前捕获一次默认超时，建立覆盖整次调用的 context。`SetTimeout` 用原子整数更新，不等 I/O、不修改在途预算；非正值移除默认预算。

ReadContext / WriteContext 的预算与 context deadline 取较早者。ReadWithDeadline 用指定绝对时间替换默认预算，再与 context 合并；零绝对时间仅移除默认预算，无 deadline 的取消仍然有效。已取消/过期入口、排队取消和获得 gate 后尚未接触 backend 的失败标为 `OperationNotStarted() == true`。

Windows 若已创建事件但尚未调用 ReadFile/WriteFile，失败也可标为未开始。进入实际系统读写后，即使 n 为零也不推断未开始。某次短写已经触及 I/O 时，后续迭代的“未开始”错误不会给整个 API 调用添加正标记。所有包装保留 Unwrap，false 标记用于阻止后续迭代的内层标记误报。

优先规则：

1. 零长度无操作优先。
2. I/O 前发现关闭和取消同时存在时，关闭优先；均不触及设备。
3. 已完成的正数字节读 / 完整写若系统报告成功，保留成功，即使取消/关闭也已发生。
4. 被中断的操作中，显式关闭优先；否则按 context 取消/预算报告。关闭竞争时可能同时 `errors.Is(ErrClosed)` 和匹配 context 原因，调用者应先判断 ErrClosed。
5. 其他真实 OS 错误保留，不被刚好发生的取消覆盖。部分数据连同错误一起返回。

`OpError` 包含操作和原始端口名。主动取消匹配 context.Canceled；预算/context deadline 匹配 context.DeadlineExceeded、ErrTimeout、os.ErrDeadlineExceeded，并实现 Timeout()。原生 ERROR_TIMEOUT / ERROR_SEM_TIMEOUT 匹配库和 OS 超时，但不伪称一定来自 context deadline。

主动关闭匹配 ErrClosed；失效句柄、明确设备不存在/未连接及 broken pipe 等有证据的 I/O 错误匹配 ErrDisconnected 并保留 OS errno。打开时找不到端口、被占用或权限不足原样保留 OS 原因，不武断区分 busy 与 permission。普通安静超时不关闭端口，也不匹配断开。

## Windows 请求生命周期

每个请求分配独立的手动复位事件和 OVERLAPPED。提交与 Close 的“进入关闭状态 + 取消全部在途请求”共用一个短锁，防止 Close 发出取消后又提交新请求。阻塞的 GetOverlappedResult 在锁外等待。

context.AfterFunc 在请求提交后注册，只对本次 OVERLAPPED 调用 CancelIoEx，避免“先取消、后提交”漏掉请求。请求结果返回后停止取消回调；若回调已经开始，等待它结束。之后才能释放事件、减少 active 计数并把缓冲区控制权还给调用者。runtime.KeepAlive 覆盖缓冲区及 OVERLAPPED 的整个在途生命周期。

Close 不拿方向 gate：设置终态并 CancelIoEx(handle,nil)，随后等待 active 请求及其回调完成，最后 CloseHandle。正常取消保留同一打开句柄，普通读超时后可继续使用。无旧回调遗留，无暗中重开。

依据 [Microsoft CancelIoEx](https://learn.microsoft.com/en-us/windows/win32/api/ioapiset/nf-ioapiset-cancelioex)：请求取消不等于完成，必须观察最终完成状态。取消时完成成功、操作中止、其他 OS 失败都可能出现。非正常驱动若永不完成取消，API 无法强制保证硬时限；库选择等待收束以避免在返回后继续访问调用者缓冲区。这是驱动层限制，不能通过遗留 goroutine 的 select 超时掩盖。

读使用固定的特殊 COMMTIMEOUTS 首字节模式：interval 与 multiplier 为 MAXDWORD，constant 为 50ms；有缓存立即返回，无数据时等首字节或该周期。空完成由 Port 重试，公开超时仍由总预算决定。写的 driver timeout 为零，由指定请求取消实现预算。该模式依据 [COMMTIMEOUTS](https://learn.microsoft.com/en-us/windows/win32/api/winbase/ns-winbase-commtimeouts)，真实串口驱动行为仍待实测。

## 线路与未提供的能力

静态 DTR/RTS 只在配置阶段按明确布尔值设置；默认均关闭。不在普通 I/O、超时或取消时翻转。不执行 EscapeCommFunction、Break 或自动恢复线路。CreateFile/驱动加载/CloseHandle 可能改变线路或产生瞬态，无法保证无毛刺；用户应核实路由器及线缆要求。参见 [DCB 线路语义](https://learn.microsoft.com/en-us/windows/win32/api/winbase/ns-winbase-dcb)。

本版不提供端口枚举、手动线路控制、硬件/软件流控、线路状态、discard、drain、Break。未来新增时必须分别命名丢弃接收、丢弃待发和等待发送完成，不能混用 Flush；RTS 手动控制须与硬件握手互斥。没有这些能力的接口不会伪造成功。

## 验证边界

通用模拟后端覆盖数据、预算、排队、并发、生命周期；Windows 系统调用模型覆盖取消/事件/句柄所有权顺序、提交与关闭竞争、取消隔离和部分结果；打开调用模型覆盖配置失败清理及 DCB 映射。模型不证明真实驱动行为。

Windows 原生管道测试使用生产传输代码，不接触 COM2；能验证内核 overlapped 取消/完成、全双工、关闭和句柄复用，但不验证串口 DCB、UART、线路电平或 COMMTIMEOUTS。本次环境管道客户端打开被拒绝，这组测试未完成。CI 强制要求其运行。

物理串口测试必须同时启用 serialintegration tag、显式指定 ASRL_TEST_PORT。仅做打开/读/关闭/重开，不发送字节。console 的实际路由器收发、Ctrl+C 和设备行为须由操作者按联调文档补充，当前均未执行。
