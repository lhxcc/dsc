# Java / Kotlin 评审规则

精确优先：只报能确证的缺陷；格式与 import 由 formatter 覆盖。

## 空值与契约

- 可能为 null 的返回值被直接调用方法（NPE），尤其 Map.get、findFirst、外部 API 返回值。
- `Optional.get()` 未先 `isPresent`/`orElse`；Optional 用作字段或参数（反模式，仅提示）。
- 重写 `equals` 未同时重写 `hashCode`，或 equals 语义与可变字段绑定（HashMap 失效）。
- Kotlin 中 Java 平台类型（`String!`）被当作非空使用；`!!` 用于确实可能为空的值。

## 资源与异常

- `InputStream`/`Connection`/`Statement`/`ResultSet`/`Lock` 未在 finally 或 try-with-resources 关闭。
- `catch (Exception e) {}` 吞异常、返回默认值掩盖失败；捕获过于宽泛。
- finally 中 return/抛错覆盖原异常。
- 线程池、ExecutorService 未 shutdown；Future 异常被丢弃。

## 并发与共享状态

- 非线程安全集合（HashMap/ArrayList/SimpleDateFormat/非线程安全 SimpleDate）跨线程共享。
- `volatile` 误用于复合操作；`check-then-act` 缺少原子性。
- 双重检查锁缺少 volatile；单例初始化非线程安全。
- 在锁内做 I/O、网络、回调或长时间计算。
- Kotlin 协程：`GlobalScope` 泄漏、结构化并发被破坏、在 `Dispatchers.Main` 上做阻塞工作。

## 数值与集合

- 用 `==`/`!=` 比较包装类型（Integer 缓存边界）或 BigDecimal 用 `equals`（scale 敏感）。
- 整型除法丢失精度；`int` 溢出；金额使用 double/float。
- 迭代时修改集合（ConcurrentModificationException）。
- `subList`/`Arrays.asList` 返回视图未复制就长期持有。

## 安全

- SQL 字符串拼接（应使用 PreparedStatement）；MyBatis `${}` 用于用户输入。
- 反序列化不可信数据（Java 原生序列化、Jackson 默认多态类型）。
- 路径拼接不可信输入导致穿越；上传文件未校验类型/大小。
- 日志记录密码/token/身份证等敏感信息；异常信息直接返回给客户端。
