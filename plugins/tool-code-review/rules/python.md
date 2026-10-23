# Python 评审规则

精确优先：只报能确证的缺陷；PEP8 格式、import 排序由 formatter/linter 覆盖。

## 可变默认参数与共享状态

- 函数默认参数使用可变对象（`[]`/`{}`/`set()`），跨调用共享。
- 类属性持有可变对象被实例共享；dataclass 未用 `field(default_factory=...)`。
- 模块级可变全局状态在并发/多请求下被修改。

## 异常处理

- 裸 `except:` 或 `except Exception: pass` 吞掉错误（含 KeyboardInterrupt/SystemExit 语义）。
- 捕获后不重新抛出、不记录、返回默认值掩盖失败。
- `finally` 中 return/抛错覆盖原始异常。
- 用异常做正常控制流且开销在热路径上（只在确证时报告）。

## 资源与上下文

- 文件/socket/连接/游标未用 `with` 或未显式关闭；异常路径泄漏。
- `subprocess` 使用 `shell=True` 拼接不可信输入（命令注入）。
- 临时文件使用可预测名称、`tempfile.mktemp`（竞态）。

## 类型与边界

- 可变对象作为 dict key / set 元素（`TypeError`）——仅在运行时可达时报。
- 整数/浮点除零、`None` 参与算术、字符串与数字隐式比较（Python 3 报错）。
- 迭代时修改正在遍历的容器（dict/list）。
- 浅拷贝（`copy.copy`/`list(...)`）后仍共享嵌套对象。

## 并发

- `threading` 下对共享可变状态无锁更新；误以为 GIL 能保证复合操作原子。
- `asyncio` 中调用阻塞函数（`time.sleep`、同步 IO）阻塞事件循环。
- 创建 task 后不 await/不保存引用，异常被静默丢弃。
- multiprocessing 传递不可 pickle 的对象或在 Windows 下缺少 `if __name__ == "__main__"`。

## 安全

- `eval`/`exec`/`pickle.loads`/`yaml.load`（非 safe_load）处理不可信数据。
- SQL 字符串拼接、`os.system` 拼接外部输入。
- 硬编码密钥；日志打印密码/token。
- 路径拼接未校验 `..`（路径穿越）。
