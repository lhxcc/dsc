# tool-code-review

独立、可复用的代码评审插件：把「确定性工程 + 单次专注评审」做成模型可调用的只读工具。

评审不依赖主 agent 的上下文：插件自己采集 diff / 读取文件、按语言匹配规则、并发调用
宿主聚合 LLM，返回带**文件与行号**的结构化评论。设计参考
[alibaba/open-code-review](https://github.com/alibaba/open-code-review) 的核心思想：

- **确定性选择范围**——哪些文件评审、哪些跳过，由代码决定并随结果报告跳过原因；
- **规则按文件匹配**——扩展名命中语言规则文档，收窄模型注意力；
- **评论必须可落地**——路径必须属于本次评审文件，行号必须是新增行；删除代码的问题
  锚定到删除位置紧随其后的行。评论行号在 ±3 行内吸附，否则丢弃——杜绝「行号漂移」。

## 工具

| 工具 | 用途 |
| --- | --- |
| `code_review_diff` | 评审 git 改动：`base`（相对基线）、`staged`（只看暂存区）、`paths`（收窄范围）、`include_tests`、`max_files`、`severity_min` |
| `code_review_scan` | 整文件扫描：`paths`（必填，文件或目录）、`include_tests`、`max_files`、`max_lines`、`severity_min` |
| `code_review_rules` | 返回路径命中的规则文档（纯本地，不调 LLM） |

三个工具均声明 `read-only` 能力：不写工作区，可在只读沙箱下运行。

### `code_review_diff` 的 diff 语义

- `base` 为空：`git diff HEAD`（暂存 + 未暂存）；仓库尚无提交时退化为 `git diff`。
- `staged=true`：`git diff --cached`。
- `base` 非空：`git diff <base>`（工作区相对该基线的全部改动）。

默认跳过：二进制、删除文件、vendored/构建产物目录、生成物（`*.pb.go`、`*.min.js` 等）、
依赖锁文件、测试文件（`include_tests=true` 打开）、单文件 diff 过大者。

## 结果

```json
{
  "ok": true,
  "mode": "diff",
  "files_reviewed": 3,
  "files_skipped": [{"path": "go.sum", "reason": "dependency lock file"}],
  "comments": [
    {
      "path": "core/x.go",
      "start_line": 42,
      "end_line": 42,
      "severity": "high",
      "category": "bug",
      "content": "忽略 Close 错误会让写盘失败被当作成功",
      "suggestion_code": "if err := f.Close(); err != nil { return err }"
    }
  ],
  "counts": {"critical": 0, "high": 1, "medium": 0, "low": 0},
  "dropped_invalid": 0
}
```

`severity`：`critical` / `high` / `medium` / `low`；
`category`：`bug` / `security` / `performance` / `maintainability` / `test` /
`documentation` / `style` / `other`。

## 依赖

- 宿主侧配置了一个 LLM 插件（插件经 interconnect 使用聚合 LLM，不需要自己的模型配置）。
  没有 LLM 互联时评审工具直接报错，而不是返回空结果。
- 评审 diff 需要 `git` 在 `PATH` 中；`code_review_scan` 不需要。

## 规则覆盖

内置规则文档（`//go:embed rules/*.md`）：`go`、`ts_js`、`python`、`java`（含 Kotlin）、
`rust`、`c_cpp`、`shell`、`config`，未命中扩展名时使用 `default`。

覆盖方式（优先级从高到低）：

1. `DSC_CODE_REVIEW_RULES_DIR` 指向的目录下的 `<rule>.md`；
2. 工作区 `.dsc/review-rules/<rule>.md`；
3. 内置规则。

## 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `DSC_CODE_REVIEW_PARALLELISM` | `3` | 并发评审单元数（1-8） |
| `DSC_CODE_REVIEW_TIMEOUT` | `120s` | 单个评审单元的超时 |
| `DSC_CODE_REVIEW_MAX_TOKENS` | `8192` | 单次评审的输出上限（推理模型的思考 token 也计入此预算） |
| `DSC_CODE_REVIEW_BUNDLE_LINES` | `600` | 单次评审的 diff 行数上限 |
| `DSC_CODE_REVIEW_BUNDLE_FILES` | `6` | 单次评审的文件数上限 |
| `DSC_CODE_REVIEW_MAX_FINDINGS` | `100` | 单次返回的评论上限（超出标记 `truncated`） |
| `DSC_CODE_REVIEW_RULES_DIR` | 空 | 规则文档覆盖目录 |

## 平台

纯 Go 实现，无 CGO，随发布包在主力三端（linux/amd64、darwin/amd64、windows/amd64）
交叉编译通过；其余平台同样可用（仅依赖 `git` 可执行文件）。
