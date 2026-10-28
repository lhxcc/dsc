---
name: mcp-connect
description: 用 DSC 連接外部 MCP 伺服器（streamable HTTP）：寫 overlay 配置、用 -patch 啟動、驗證 mcp__<server>__* 工具是否註冊成功，並排查「連不上 MCP」的常見原因。
---

# mcp-connect — 用 DSC 連接 MCP 伺服器

當使用者說「用 DSC 連上某個 MCP 服務」「把 MCP 工具接進來」「為什麼 MCP 連不上」時，照本技能做。

## 先記住一條：MCP 是宿主配置項，不是子命令

DSC 的 MCP 工具由**宿主在啟動時按配置連接**，註冊成 `mcp__<server_name>__<tool>` 的工具。
所以：

- **不要**用 `shell` 再起一個 `dsc …` 進程去「連 MCP」——那是另一個進程，它自己的工具表，
  當前會話永遠看不到那些工具，而且會多一份會話/任務狀態。
- **不要**指望有一條 `dsc mcp connect` 之類的命令。要用的是下面的配置 + `-patch`。

## 前提：只支援 streamable HTTP

DSC 的 MCP 客戶端只實作 **streamable HTTP**（`http(s)://host:port/mcp`）。
**stdio 尚未支援**——很多 MCP 伺服器預設走 stdio（例如 REA 的 `rea mcp`），
必須讓它改成 HTTP 監聽，否則永遠連不上（表現為 connection refused 或根本沒有端口）。

## 三步接上

### 1. 讓 MCP 伺服器以 HTTP 監聽

以 REA 為例（其他伺服器看各自的 `--http`／`--port` 參數）：

```bash
cd <rea 目錄> && rea mcp --http --port 8765
```

成功的日誌會出現一行（記下 url）：

```json
{"transport":"streamable-http","url":"http://127.0.0.1:8765/mcp","msg":"MCP transport listening"}
```

先確認它真的在聽（端口要和上面一致）：

```bash
curl -sS --noproxy '*' -o /dev/null -w '%{http_code}\n' -X POST http://127.0.0.1:8765/mcp \
  -H 'Content-Type: application/json' -H 'Accept: application/json, text/event-stream' \
  -d '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"probe","version":"0"}}}'
# 期望 200（并带 Mcp-Session-Id 响应头）
```

> `--noproxy '*'` 不是可選項：本機設了 `HTTP_PROXY` 時，curl 會把 127.0.0.1 也發給代理，
> 得到 502。DSC 自己不會（Go 對 loopback 預設繞過代理），但手動驗證時會踩。

### 2. 寫一份 overlay 配置

```yaml
# mcp-rea.yaml
- name: mcp-rea          # 條目名，任意
  type: dsc
  enabled: true          # ★ 必須寫：省略等於 false，整條會被跳過（日誌會有 WARN）
  config:
    mcp:
      server_name: rea   # ★ 工具名前綴：mcp__rea__*，只允許 [A-Za-z0-9_-]，≤32 字符
      endpoint: http://127.0.0.1:8765/mcp
```

多個 MCP 伺服器就寫多個條目，**`server_name` 必須不同**（否則工具名互相覆蓋）。

### 3. 帶著 overlay 啟動 DSC

```bash
dsc -patch <絕對路徑>/mcp-rea.yaml
```

也可以把同樣的 `config.mcp` 塊寫進 `config/config.yaml` 的 `plugins:` 條目裡（效果相同，但要記得 `enabled: true`）。

## 驗證真的接上了

1. 啟動日誌裡應有：`patch MCP connected  server_name=rea tools=<N>`
2. 工具清單裡出現前綴為 `mcp__rea__` 的工具（名字規則：`mcp__<server_name>__<原始工具名>`，
   非法字符被替換成 `_`，總長截到 64）。
3. 讓模型直接調用其中一個；能拿到結果就說明鏈路通了。

## 排錯對照表

| 症狀 | 原因 | 處理 |
|---|---|---|
| `connection refused` / 端口沒在聽 | 伺服器沒起，或起成了 **stdio** 模式 | 加 `--http --port N`；確認日誌有 `MCP transport listening` |
| 一個 `mcp__*` 工具都沒有，日誌有 `patch MCP config present but entry is disabled` | overlay 漏寫 `enabled: true` | 補上 |
| HTTP 400 `Mcp-Session-Id header is required` | 客戶端沒回帶 initialize 下發的會話 id（舊版 DSC 的 bug，已修） | 升級 DSC |
| HTTP 400 `Parse error: Invalid JSON-RPC message` | 請求 id 超過 JS 安全整數 2^53（舊版用 UnixNano，已修） | 升級 DSC |
| HTTP 404 `Session not found` | 伺服器重啟過，會話失效 | DSC 會自動重新握手一次；持續出現則重啟 DSC |
| HTTP 406 `Not Acceptable` | 端點不是 streamable HTTP（可能是舊式 SSE 端點） | 換成 `/mcp` 端點；確認伺服器是 streamable HTTP |
| 日誌 `patch MCP stdio transport not yet supported` | 配置寫了 `transport: stdio` | 改讓伺服器開 HTTP；stdio 目前不支援 |

## 安全邊界

MCP 工具的權限等於**本機執行 DSC 的使用者**，請只連接 loopback 或你信任的位址；
不要把帶憑據的 endpoint 寫進會被提交的配置文件。
