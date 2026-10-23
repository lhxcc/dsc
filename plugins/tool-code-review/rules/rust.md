# Rust 评审规则

精确优先：只报能确证的缺陷；`rustfmt`/`clippy` 已覆盖的格式与惯用法不要重复报。

## 错误处理

- `unwrap()`/`expect()` 用于可能失败且失败可恢复的路径（生产请求、IO、解析外部输入）。
- `let _ = ...` 丢弃 `Result`；`?` 之后的错误被转换为丢失上下文的类型。
- `panic!`/`unreachable!`/`todo!` 遗留到生产路径；索引 `v[i]` 可能越界。
- 自定义 Error 未实现 `std::error::Error`/`Display`，或 `source()` 丢失因果链。

## 所有权与生命周期

- 不必要的 `clone()` 掩盖借用冲突（仅在明确性能/语义后果时报）。
- 把引用/借用传给异步任务或 spawn 的线程，生命周期不匹配（编译期已阻止的不报）。
- `Rc`/`RefCell` 在跨线程场景使用；`RefCell` 的 `borrow_mut` 与 `borrow` 重叠导致运行期 panic。
- `unsafe` 块缺少安全前提说明，或前提与调用方契约不符。

## 并发

- 在锁内执行 I/O/await/回调；`Mutex` 中毒未处理（`unwrap` on lock）。
- `tokio::spawn` 的任务未保存 JoinHandle、错误被丢弃。
- 无界 channel/队列导致内存增长；`select!` 分支缺少取消处理。
- `Atomic` 与非原子访问混用同一状态。

## 切片、索引与数值

- `as` 收窄转换截断（`i64 as i32`）、无符号/有符号互转。
- 切片边界计算溢出；`len()-1` 在空集合上。
- `usize` 减法下溢（debug panic / release 回绕）。

## 安全

- SQL/shell/路径由不可信输入拼接；`Command::new("sh").arg("-c")` 传入外部输入。
- 硬编码密钥；日志泄露敏感信息。
- 反序列化不可信数据使用不安全的格式/配置。
