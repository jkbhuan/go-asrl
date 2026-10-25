# go-asrl

`github.com/jkbhuan/go-asrl`，包名 `asrl`，是可独立使用的 Go 串口字节流库。最低 Go 1.25；Windows 后端使用 `golang.org/x/sys/windows`，不需要 CGo、NI-VISA 或厂商运行时。

| 平台 | 当前能力 |
| --- | --- |
| Windows amd64 | 原生串口打开/配置、原始字节读写、总预算、context 取消、并发关闭、全双工 |
| Linux / macOS / 其他非 Windows | 包和示例可构建；`Open` 返回 `ErrNotSupported` |

Windows 原生串口和 COM2 路由器尚未实测。当前环境的通用测试通过；原生管道测试因沙箱拒绝打开管道客户端而跳过。完整执行记录见 [验证报告](docs/validation-report.md)。

## 库的使用

```go
package main

import (
    "context"
    "errors"
    "fmt"
    "time"

    asrl "github.com/jkbhuan/go-asrl"
)

func main() {
    // 9600、8N1、无流控、5s；除波特率外仍须核实路由器参数。
    cfg := asrl.DefaultConfig()
    port, err := asrl.Open("COM2", cfg)
    if err != nil { panic(err) }
    defer port.Close()

    ctx, cancel := context.WithTimeout(context.Background(), time.Second)
    defer cancel()
    buf := make([]byte, 4096)
    n, err := port.ReadContext(ctx, buf)
    if n > 0 { fmt.Printf("% X\n", buf[:n]) } // 先处理数据，再处理错误。
    if err != nil && !errors.Is(err, asrl.ErrTimeout) { panic(err) }
}
```

- `Read` / `Write` / `Close` 实现 `io.ReadWriteCloser`。有 context 的独立方法为 `ReadContext`、`WriteContext`。
- `Config{}` 的零字段采用默认值，包括 5 秒超时。`Config.Normalize()` 可在打开前获得完整配置并验证数值。显式无限等待用 `port.SetTimeout(0)`；`SetTimeout(d <= 0)` 对后续操作移除默认预算。
- 支持 Windows `COM2`、`COM10` 等名字及 `\\.\...`、`\\?\...` 设备路径。不接受 `ASRL2::INSTR` 或数字端口编号。裸 COM 名不能含前导零或冒号。
- 数据位 5～8；校验 none/odd/even/mark/space；停止位 1、1.5（仅 5 数据位）、2（仅 6～8 数据位）。实际组合和波特率仍取决于驱动；拒绝或读回不一致时返回能力错误，保留 OS 原因。物理验证目前仅是待执行的 9600/8N1 目标。
- 当前流控只支持 `none`；`rtscts` / `xonxoff` 返回 `ErrNotSupported`。DTR/RTS 默认不置位，可通过配置布尔值选择初始静态电平；不提供运行时翻转接口。驱动在打开/关闭时仍可能产生瞬态。
- 一次操作的总预算包括同方向排队和短写循环。默认预算与 context deadline 取较早者。`ReadWithDeadline(ctx, buf, deadline)` 用绝对 deadline 替换默认预算；零 deadline 移除默认预算，context 仍然生效。
- 至少一读一写可并行；同方向串行。`Close` 幂等，从读写锁外请求取消并等待内核请求和回调结束。已完成的成功 I/O 在取消竞争中仍可返回成功。
- 核心读写不增删 CR/LF、不 trim、不转换编码、不解释回显、不丢弃缓冲区。读不必填满缓冲区；串口安静不会产生 EOF。写会累计短写；零长度始终是 `(0, nil)`。
- 超时/取消可能伴随部分数据。部分写前缀可能已到设备；库不自动重发。写成功仅代表后端接受字节。

可用 `errors.Is` 区分 `ErrInvalidConfig`、`ErrNotSupported`、`ErrTimeout`、`ErrClosed`、`ErrDisconnected`；`errors.As` 到 `*asrl.OpError` 获得操作、端口及可解包原因。预算超时同时匹配 `context.DeadlineExceeded` 和 `os.ErrDeadlineExceeded`；主动取消匹配 `context.Canceled`。未开始 I/O 的错误可通过结构接口 `OperationNotStarted() bool` 识别；不能根据 `n == 0` 判断没有接触设备。完整规则见 [设计](docs/design.md)。

## COM2 console

在本项目根目录运行，先关闭 PuTTY、串口助手等占用端口的程序。已知只有 COM2 和 9600；以下 8N1、无流控、DTR/RTS 关闭均为待核实的初始建议。

```powershell
$env:GOWORK = 'off'

# 默认只接收，不发送任何内容。
go run ./cmd/asrl-console -port COM2 -baud 9600

# 核实设备参数后启用按行交互。空行在此配置下发送一次 CR。
go run ./cmd/asrl-console -port COM2 -baud 9600 -data-bits 8 -parity none -stop-bits 1 -flow-control none -interactive -line-ending cr

# 查看全部参数 / 以十六进制观察控制字节。
go run ./cmd/asrl-console -h
go run ./cmd/asrl-console -port COM2 -baud 9600 -hex
```

启动信息写入 stderr，接收字节写入 stdout。接收不等待换行；读超时后继续。交互的 `-line-ending` 支持 `cr`、`lf`、`crlf`、`none`，只处理本地输入行的换行并显式添加所选后缀。`none` 仍是按行输入，并非逐键终端模式。程序不额外显示发送文本；终端自身可能回显本地输入。交互输入 EOF 或 Ctrl+C 退出并关闭串口。

`-timeout 0` 在打开后显式移除默认预算；`-dtr` / `-rts` 选择初始置位。程序不会探测、登录或发送 `*IDN?`、Break、重启、恢复出厂等命令。型号、提示符及安全的只读命令由操作者根据实际设备决定。

完整操作与真机记录方法见 [Windows 联调](docs/windows-console-testing.md)。

## 检查与后续接入

```powershell
$env:GOWORK = 'off'
go test ./...
go vet ./...

# 原生 Windows 内核管道测试：严格要求可用，否则失败。
$env:ASRL_REQUIRE_NATIVE_IO = '1'
go test . -run TestWindowsNative -v -count=1 -timeout=60s
Remove-Item Env:ASRL_REQUIRE_NATIVE_IO

# 仅在原生平台具备 CGo/race 工具链时执行。
go test -race ./...
```

默认测试不访问物理串口。Windows 管道受限时测试会明确跳过；CI 设置 `ASRL_REQUIRE_NATIVE_IO=1`，拒绝把跳过当作通过。真实串口测试另需 `serialintegration` build tag 和显式端口，见联调文档。

CI 配置覆盖 Windows 与 Linux 的 Go 1.25 / stable 测试、Linux race，以及 Windows amd64、Linux amd64、macOS arm64 的纯 Go 构建。CI 尚未在本次交付中运行。go-visa 正式接入留到下一阶段，见 [接入设计](docs/go-visa-integration.md)。
