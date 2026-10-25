# 验证报告

日期：2026-10-25。此报告区分代码实现、模拟测试、原生内核测试和真实串口证据，不把构建视为目标平台运行。

## 实际环境与前置检查

| 项目 | 实际观察 |
| --- | --- |
| 开发宿主 | Windows amd64；系统 API 报告 Microsoft Windows NT 10.0.26100.0 |
| Go 工具链 | `go version go1.26.4 windows/amd64` |
| 模块最低版本 | go.mod 为 Go 1.25.0；本机未用 Go 1.25 单独运行，CI 已配置 1.25.x |
| 依赖 | 唯一外部模块 golang.org/x/sys v0.42.0；已读取 go.mod、Windows 源码、BSD-3-Clause LICENSE；go.sum 已固定校验和 |
| 初始目录 | 只有任务说明，没有既有 module 或 Git 仓库；未发现适用 AGENTS.md |
| 独立性 | GOWORK=off；无 replace，无 go-visa/VISA/CGo 依赖；`go list -m all` 只有本模块和 x/sys |
| go-visa 基线 | 读取了相邻工作区及远程 main 的相关文件；本地 HEAD 与任务基线不同；已有 go.mod/go.sum 修改未写入，详情见接入设计 |
| 物理硬件操作 | 未打开 COM2，未发送任何路由器命令或数据，未记录 console 内容 |

本环境默认 Go build cache 出现访问拒绝，检查时把 GOCACHE 指向可写的 `%TEMP%\go-asrl-build-cache`，并显式设置 GOWORK=off；没有更改全局 Go 配置。以下结果均为实际执行记录。

## 已执行的自动化检查

| 命令/检查 | 结果与边界 |
| --- | --- |
| `go mod tidy` | 通过；生成固定 x/sys 版本的 go.sum |
| `go test -json ./... -count=1 -timeout=60s` | 退出 0；34 个顶层测试通过、5 个明确跳过，不含子测试计数；两个包通过 |
| Windows 打开/配置模型 | 通过：打开失败不关闭未获句柄；获取状态、设置状态、验证状态、驱动强制改值和设置 timeout 失败均清理已获句柄；OS 原因可识别 |
| Windows 系统调用生命周期模型 | 通过 6 项：取消回调收束后才释放资源、提交/关闭竞争、旧取消隔离、取消写前缀保留、完成成功优先、读取消不影响并行写 |
| Windows 原生命名管道测试 | 5 项跳过；CreateNamedPipe 可执行，但 `CreateFile (pipe client)` 返回 Access is denied；未完成生产传输路径的原生取消/关闭验证 |
| `go vet ./...` | 通过，无诊断 |
| `gofmt -l .` | 通过，无输出 |
| `CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ./...` | 通过，纯 Go 构建；不等于串口真机验证 |
| `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./...` | 通过，交叉构建；Linux 未运行，Open 为不支持存根 |
| `CGO_ENABLED=0 GOOS=darwin GOARCH=arm64 go build ./...` | 通过，交叉构建；macOS 未运行，Open 为不支持存根 |
| `go run ./cmd/asrl-console -h` | 通过，退出 0；实际帮助含默认 COM2/9600、接收模式、全部配置和换行选项；不打开串口 |
| `go test -tags serialintegration . -run '^TestHardwareOpenReadCloseReopen$' -v -count=1 -timeout=30s` | 按设计跳过：未设置 ASRL_TEST_PORT；真实串口部分未执行 |
| `go test -race ./...`（默认 CGo=0） | 未运行测试：工具拒绝，提示 race 需要 CGo |
| `CGO_ENABLED=1 go test -race ./... -count=1 -timeout=60s` | 构建失败，缺少 C 编译器 gcc；race 测试未执行，不记为通过 |
| GitHub Actions | 已交付 Windows / Linux 原生测试、Linux race、三目标构建配置；本次未触发或观察 CI 运行 |

原生管道测试首次执行报告失败，原因是上述受限管道客户端访问。测试现对 ACCESS_DENIED 给出明确 skip；其他错误仍失败。`ASRL_REQUIRE_NATIVE_IO=1` 禁止此 skip，CI 始终启用，避免原生覆盖缺失被隐藏。

通用模拟覆盖：原始 CR/LF/NUL/高位字节、分段短读、累计短写、数据与错误并存、零长度、无进展限速、context/默认预算取早、排队超时/取消、SetTimeout 不阻塞且不改变在途预算、绝对/零读 deadline、读超时后复用、并行读写、幂等关闭中断双方、写调用不交错、部分 I/O 未开始标记及错误原因保留。

console 测试覆盖：默认只接收、不读 stdin/不写串口；按片段展示、超时继续、原始与 hex 输出、四种换行、保留输入空格、不额外 echo；取消接收/发送后关闭端口并等待串口读取结束。这些是模拟测试，不是实机 Ctrl+C 证据。

## 真实串口记录（待操作者填写）

| 项目 | 实际条件/观察 | 结果与证据 |
| --- | --- | --- |
| Windows / Go / 依赖版本 | 上文为开发环境；本机真机联调环境待填写 | 未执行 |
| 端口、9600 与其余串口参数 | COM2；除 9600 外仍待核实，8N1/无流控仅初始建议 | 未执行 |
| 路由器型号/连接方式 | 待填写，可脱敏 | 未执行 |
| 打开与读取已有输出 | 待填写 | 未执行 |
| 操作者发送空行与观察响应 | 待填写实际换行与回显 | 未执行 |
| 可选只读命令收发 | 待填写实际设备命令 | 未执行 |
| 接收超时后继续使用 | 待填写；若设备无法保持安静，保留未验证 | 未执行 |
| Ctrl+C 与重开端口 | 待填写退出耗时与重开结果 | 未执行 |
| 尚未验证的功能/平台 | 真实 Windows 串口驱动/线路、原生 overlapped 完整路径、Go 1.25 运行、Linux/macOS 运行、race、全部参数及仪器协议 | 未执行 |

按 [Windows 联调步骤](windows-console-testing.md) 执行后，只更新有实际证据的行。路由器 console 通过也不能扩展为 SCPI、二进制仪器、其他参数/平台或线路时序通过。

## 当前交付状态

库、Windows 后端、非 Windows 构建边界、console、自动化测试、CI 和四份说明文档均已交付；没有修改 go-visa 或发布仓库。Windows 串口实际可用性仍需操作者/可用原生环境补齐证据。本次没有将模拟结果当作 COM2 或完整原生 Windows I/O 通过。

主要剩余限制：Windows-only 实际后端；未实现 RTS/CTS、XON/XOFF、端口枚举、运行时线路控制、discard/drain/Break。打开过程不可取消；损坏驱动若不完成取消请求，无法既硬限时返回又保证缓冲区生命周期。具体契约与选择见 [设计](design.md)。
