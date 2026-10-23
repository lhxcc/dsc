# Go 评审规则

精确优先：只报能确证的缺陷。`gofmt`、`go vet`、compiler、`go test -race` 已能发现的
问题不要重复报（除非差异里显示了它们无法表达的用户可见后果）。

## 错误与 API 契约

- 错误被忽略、覆盖或转成默认值从而掩盖失败；`errcheck` 类问题。最佳努力操作只有在
  失败确实安全且上下文可见时才可接受。
- 错误包装丢失原因（需要 `errors.Is/As` 时用 `%v` 而非 `%w`）、包装 nil、在公开边界
  泄露内部细节。
- 请求/worker/库代码里 `panic`、`log.Fatal`、`os.Exit`，本可返回错误。
- `defer` 清理覆盖主错误，或 Close/Commit/Rollback 的错误被丢弃后仍报告成功。

## nil、接口与值语义

- 把类型化 nil 指针/map/slice/func/error 存进非 nil 接口后被当作"不存在"。
- 向 nil map 写入、对 nil channel 收发（永久阻塞）、解引用可能为 nil 的指针。
- 首次使用后复制含 `sync.Mutex`/`RWMutex`/`Once`/`atomic` 的值（值接收者、append、
  map value、返回副本）。
- 值接收者只改到副本、调用方却以为共享状态被更新（含 map/slice/指针/lock 的结构体）。
- `sync.Once` 用于必须失败后重试的初始化。

## 上下文、goroutine 与取消

- 请求作用域内的工作使用 `context.Background()/TODO()`，丢失调用方 deadline/取消。
- `WithCancel/WithTimeout/WithDeadline` 的 cancel 未调用（除非所有权已明确转移）。
- 阻塞 I/O、重试、select、循环缺少取消或 deadline，而依赖可能长时间不返回。
- goroutine 可能永久存活（等 channel/锁/I/O/无限重试），且没有退出路径或错误反馈。
- 闭包捕获循环变量/可变外部变量，且能观察到后续值（注意 go directive ≥1.22 语义差异）。

## channel、锁与共享状态

只在能证明状态会被并发访问时报告竞态/死锁。

- 无同步地并发读写 map/slice/指针/计数器/复合状态；可交错的 check-then-act。
- 持有 mutex 做阻塞 I/O、channel 操作、回调、网络/数据库调用或长 CPU 计算。
- `RLock` 期间改数据；atomic 与非 atomic 混用同一状态。
- channel 可能永久阻塞（对端可能停止/缓冲满/未 select 取消）。
- 多方可关闭同一 channel、向已关闭 channel 发送、重复关闭。
- `select` 的 default 忙等或丢弃必要工作；无退避/取消的无限重试。
- WaitGroup 的 Add 与 Wait 竞争、漏 Done、首次使用后复制。

## 资源与定时器

- `http.Response.Body`、`sql.Rows`、文件、socket、事务、锁未在所有可达路径关闭。
- 循环内 `defer` 且在长循环/持续运行函数里延迟释放文件、连接、行、锁。
- `sql.Rows` 循环后漏 `rows.Err()`，或提前退出时未关闭 rows。
- 服务端路径上的外部 HTTP 调用缺少 body 清理、请求 context 或超时。
- timer/ticker 在工作结束后仍存续并持续触发（注意 Go 1.23+ 的 timer 语义变化，
  不要仅以"GC 泄漏"为由报告未 Stop）。

## 切片、字节与数值

- 返回/缓存/传递的 slice/map/byte buffer 底层数组随后被复用或修改，导致其他持有者
  观察到的数据变化（确认所有权后再报；零拷贝 API 可能是有意设计）。
- `append` 到与调用方共享底层数组的 slice，修改逃逸；保留巨大 buffer 的小切片。
- 可达的越界索引/切片/长度运算，尤其是空输入与边界输入。
- 整数转换、收窄、有符号/无符号比较、容量计算、偏移运算可能溢出或把负数变成巨大值。

## 安全边界

确认攻击者可控或信任边界后再报，并给出具体利用路径。

- SQL、shell、URL、路径、header、模板、正则、序列化数据由不可信输入拼接，且未做参数化/
  校验/转义/白名单。
- 路径穿越、跟随符号链接、不安全解压、不安全临时文件、权限假设导致越权读写。
- HTML 用 `text/template` 渲染、用不可信内容构造 trusted template。
- SSRF/凭据泄露：不可信目标、跨主机转发敏感 header、重定向到内网。
- 密钥、token、私钥、个人敏感数据被记录、写进错误或硬编码。
- 安全敏感场景用 `math/rand`（会话 ID、token、nonce、salt），应使用 `crypto/rand`。
