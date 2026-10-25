# Windows / COM2 本机联调

这份步骤用于操作者在真实 Windows 电脑上执行。当前仅知道 COM2 接有支持 console 的路由器、波特率 9600；型号、登录状态、提示符及其余串口参数未知。本文的 8N1、无流控、DTR/RTS 关闭都是可修改的初始建议，尚未确认为设备要求。

## 准备

1. 在设备管理器确认端口确为 COM2，确认线缆和接口电气规格适用于该路由器。
2. 根据实际型号资料核实数据位、校验、停止位、流控、DTR/RTS 要求。可先用已知可用终端确认参数，再退出终端释放端口。
3. 关闭 PuTTY、串口助手等占用 COM2 的程序。本库独占打开串口；权限不足和端口占用都可能由 OS 返回“拒绝访问”，程序保留原错误。
4. 项目根目录使用 Go 1.25 或更高版本；本库不要求 CGo、VISA 或路由器厂商运行时。

```powershell
$env:GOWORK = 'off'
go version
go test ./...
go vet ./...
go run ./cmd/asrl-console -h
```

普通测试不访问串口。如果在受限开发环境中 Go build cache 不可写，可仅为本次 shell 设置 `$env:GOCACHE = Join-Path $env:TEMP 'go-asrl-build-cache'`，不必修改全局 Go 配置。

## 先只读，再按需交互

```powershell
# 不会主动发送任何字节。
go run ./cmd/asrl-console -port COM2 -baud 9600

# 需要观察 CR/LF、NUL 或高位字节时，可退出后以 hex 模式重开。
go run ./cmd/asrl-console -port COM2 -baud 9600 -hex
```

启动后检查 stderr 打印的有效配置。没有已有输出不能直接判为失败，console 可能正在等待输入。接收内容按到达片段即时显示，不要求换行或提示符；超时后继续接收。

Ctrl+C 退出，核实参数后启用交互：

```powershell
go run ./cmd/asrl-console -port COM2 -baud 9600 -data-bits 8 -parity none -stop-bits 1 -flow-control none -interactive -line-ending cr
```

若设备明确要求初始 DTR 或 RTS 置位，可添加 `-dtr`、`-rts`。默认 false；库不在普通读写或取消时翻转线路，但驱动在打开/关闭过程可能产生瞬态。若设备要求 RTS/CTS 或 XON/XOFF，本版返回不支持，不应改成无流控冒充验证成功。

操作者可先输入一次空行，本例发送单个 CR。观察是否出现提示符、回显或登录提示。若无响应，依据实际设备资料退出后选择 `-line-ending lf` 或 `crlf`；程序不会自动遍历换行或命令。

交互采用本地按行输入：移除本地 LF/CRLF，再附加指定后缀。空行 + `none` 是零字节无操作。`none` 不会变成逐键 raw 模式，也不提供方向键、终端协商或全屏仿真。程序不额外打印发送文本，本地终端自己的输入回显仍可能存在。输入流 EOF 也会结束交互会话。

若出现登录提示，凭据由操作者处理；密码在本地按行终端中可能可见。不要把凭据、完整配置或敏感 console 记录写入仓库。仅选择实际设备已知安全的只读查询命令验证收发；无须为了本任务修改路由器配置。程序本身不探测型号、不自动登录、不发 `*IDN?`、重启、恢复出厂、写配置或 Break。

## 超时、退出和重开

1. 在路由器可保持安静的条件下，保持只读会话超过默认 5 秒，确认读超时没有结束会话；再通过操作者确认安全的方式产生输出，观察继续接收。
2. 若设备无法保持安静，将“物理读超时后继续使用”记录为未验证，不能用未观察到的现象填通过。模拟测试只能补充库逻辑证据。
3. 分别在空闲等待接收和有输入时按 Ctrl+C，记录退出耗时。程序取消并等待串口读写、关闭句柄；本地 stdin 读取 goroutine 可能等进程退出，不持有串口或执行串口 I/O。
4. 立即重跑只读命令，确认 COM2 能重开。`go run` 包含构建与包装进程；如需精确测量运行程序退出，可先构建再直接运行：

```powershell
go build -o asrl-console.exe ./cmd/asrl-console
.\asrl-console.exe -port COM2 -baud 9600
```

`-timeout 0` 会取消默认预算，Ctrl+C 仍应生效。默认 5 秒预算是每次调用的总时间，不是每个字节的间隔。

部分写后超时/取消时，前缀可能已被路由器接收，程序不会自动重发。操作者按设备实际状态决定恢复步骤。

## 可选的显式真实串口测试

此测试只做打开、读取、关闭、重开，不发送字节。打开本身仍可能影响线路，先核实设备要求。没有 `ASRL_TEST_PORT` 时明确跳过；不默认选择 COM2。

```powershell
$env:GOWORK = 'off'
$env:ASRL_TEST_PORT = 'COM2'
$env:ASRL_TEST_BAUD = '9600'
$env:ASRL_TEST_DATA_BITS = '8'       # 以下均须按实际资料确认。
$env:ASRL_TEST_PARITY = 'none'
$env:ASRL_TEST_STOP_BITS = '1'
$env:ASRL_TEST_FLOW_CONTROL = 'none'
$env:ASRL_TEST_DTR = 'false'
$env:ASRL_TEST_RTS = 'false'
go test -tags serialintegration . -run '^TestHardwareOpenReadCloseReopen$' -v -count=1 -timeout=30s
Remove-Item Env:ASRL_TEST_PORT
```

该测试不记录收到的文本，只记录字节数。若恰好收到数据，不能由本次结果认定安静超时已验证；它也不验证操作者发送、登录和命令处理。

## 不依赖物理端口的 Windows 原生检查

在允许本地命名管道的原生 Windows 环境执行：

```powershell
$env:GOWORK = 'off'
$env:ASRL_REQUIRE_NATIVE_IO = '1'
go test . -run '^TestWindowsNative' -v -count=1 -timeout=60s
Remove-Item Env:ASRL_REQUIRE_NATIVE_IO
```

它使用生产 overlapped 传输路径和两个本地管道端点，验证取消、关闭、全双工和超时后继续使用，不验证串口驱动或 COM2。普通测试在管道打开被拒绝时会显式跳过；上述开关和 CI 会把此情况视为失败，防止缺失原生覆盖被隐藏。

## 填写证据

把实际参数、必要的脱敏现象、退出耗时和重开结果填入 [验证报告](validation-report.md) 的真机表。未执行的项目继续保留“未执行”。COM2 路由器只能证明当前设备和配置下的打开、文本收发与生命周期，不能证明 SCPI、二进制仪器协议、全部参数、其他平台或电气时序。

拔掉 USB 转串口线的可观察错误因驱动而异；拔线也不等于普通 UART 能感知所有远端断开。
