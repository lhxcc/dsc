# TypeScript / JavaScript 评审规则

精确优先：只报能确证的缺陷；格式、分号、引号、import 顺序由 Prettier/ESLint 覆盖。

## 空值与类型

- 可能为 `null`/`undefined` 的值被直接解构、取属性、调用方法（确认上游确实可能缺失）。
- `any`/`as` 断言掩盖了真实类型不匹配，且运行时取值会失败。
- `==` 与 `===` 混用导致隐式转换错误（`0`/`""`/`null` 边界）。
- 可选链 `?.` 之后仍然直接使用非可选结果，或 `!` 非空断言用于确实可能为空的值。

## 异步与并发

- `await` 遗漏：Promise 未被 await/return，错误变成 unhandled rejection。
- `forEach` 里使用 `async`（不会等待），或循环内串行 await 造成不必要的阻塞。
- `Promise.all` 中任一失败即整体丢失其他结果，且没有 `allSettled` 的补偿。
- 竞态：并发请求/定时器回调修改同一状态，缺少取消或版本校验（如 stale response 覆盖新数据）。
- `setInterval`/`setTimeout`/事件监听未清理，组件卸载后仍触发（React `useEffect` 缺 cleanup）。
- 使用 `for...in` 遍历数组/对象时未过滤原型链属性。

## 错误处理

- `try/catch` 吞掉错误（空 catch、只 console.log 后继续返回成功）。
- Promise 链末缺少 `.catch`，或 `catch` 中 `throw` 丢失原始错误上下文。
- 错误对象被转成字符串后丢失 `cause`/堆栈。

## 可变性与副作用

- 直接修改 props/参数/共享对象（尤其数组 `sort`/`reverse` 原地修改调用方数据）。
- 闭包捕获可变变量导致过期值（React hooks 依赖数组缺失、事件处理器读到旧 state）。
- 深浅拷贝误用：展开运算符只做浅拷贝，嵌套对象仍共享。
- 日期/正则等可变对象被跨调用复用（`lastIndex`、`setDate`）。

## 安全

- `innerHTML`/`dangerouslySetInnerHTML`/`document.write` 拼接不可信内容（XSS）。
- 用字符串拼接构造 SQL/shell/URL；`eval`、`new Function`、动态 `require` 处理外部输入。
- 客户端代码中硬编码密钥/token。
- 开放重定向：把用户输入直接作为跳转目标。

## 资源与性能

- 事件监听、WebSocket、订阅未在卸载时移除，造成泄漏。
- 大列表渲染缺少 key 或 key 使用索引导致状态错位。
- 在渲染路径中做重计算/新对象创建且未 memo（只在有明确性能后果时报）。
