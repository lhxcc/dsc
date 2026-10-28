---
name: mcp-connect
description: 用 DSC 連接外部 MCP 伺服器（streamable HTTP）：運行期用 mcp_connect/mcp_list/mcp_load 接上並按需加載工具、或用 overlay 配置持久化；驗證 mcp__<server>__* 是否可用，並排查「連不上／看不到工具」的常見原因。
---

# mcp-connect — 用 DSC 連接 MCP 伺服器

當使用者說「用 DSC 連上某個 MCP 服務」「把 MCP 工具接進來」「為什麼 MCP 連不上」「連上了但看不到工具」時，照本技能做。

## 先記住兩條

1. **不要**用 `shell` 再起一個 `dsc …` 進程去「連 MCP」。那是另一個進程，有它自己的工具表，
   當前會話永遠看不到那些工具，還會多一份會話與任務狀態。
2. **連上 ≠ 看得見**。MCP 工具被接進宿主後**預設不進你的工具目錄**（一台伺服器可能有幾百個工具，
   目錄每個請求都要發）。要用 `mcp_load` 明確加載，用完 `mcp_unload` 撤出。

## 前提：只支援 streamable HTTP

DSC 的 MCP 客戶端只實作 **streamable HTTP**（`http(s)://host:port/mcp`）。
**stdio 尚未支援**——很多 MCP 伺服器預設走 stdio（例如 `rea mcp`），必須讓它改成 HTTP 監聽，
否則永遠連不上（表現為 connection refused，或根本沒有端口）。

## 路線 A：運行期接上（推薦，不用改配置、不用重啟）

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

> `--noproxy '*'` 不是可選項：本機設了 `HTTP_PROXY` 時，curl 會把 127.0.0.1 也發給代理，得到 502。
> DSC 自己不會（Go 對 loopback 預設繞過代理），但手動驗證時會踩。

### 2. 連接伺服器

```
mcp_connect(server_name="rea", endpoint="http://127.0.0.1:8765/mcp")
```

- `server_name` 決定工具名前綴：`mcp__rea__<tool>`，只允許 `[A-Za-z0-9_-]`，≤32 字符；
- 多台伺服器用不同 `server_name`（同名會互相覆蓋）；
- 這個工具需要審批（它等於對外擴張能力）；被拒時停下來問使用者，不要改參數硬試。

回傳會告訴你發現了幾個工具——**那是註冊數，不是可見數**。

### 3. 看清有哪些工具，再按需加載

```
mcp_list(server="rea", query="unit")     # 名字 + 一行說明，不含參數 schema
mcp_load(tools=["mcp__rea__list_jeb_units", "mcp__rea__get_jeb_unit"])
```

- 只加載這次任務真正要用的。目錄每個請求都發：實測一台 229 個工具的伺服器佔 **約 7.4 萬 tokens**
  （1M 窗口 7%，128K 模型 57%），加載 10 個只要 ~2.5K；
- 加載在**下一跳**生效（工具目錄變了），不要在同一跳就假設能調；
- 裸工具名（`list_jeb_units`）在唯一時可用，重名會報歧義並列出候選。

### 4. 用完撤出

```
mcp_unload()                              # 全部卸載
mcp_unload(server="rea")                  # 卸載某台伺服器的
mcp_unload(tools=["mcp__rea__list_jeb_units"])
```

卸載只是把工具移出目錄，**連接還在**——再 `mcp_load` 即可回來，不用重連。
任務結束（或不再需要那批工具）就卸載，別讓它一直佔著每個請求的目錄成本。

## 路線 B：寫進配置（需要每次啟動都連時用）

```yaml
# mcp-rea.yaml
- name: mcp-rea          # 條目名，任意
  type: dsc
  enabled: true          # ★ 必須寫：省略等於 false，整條會被跳過（日誌會有 WARN）
  config:
    mcp:
      server_name: rea   # ★ 工具名前綴：mcp__rea__*
      endpoint: http://127.0.0.1:8765/mcp
```

```bash
dsc -patch <絕對路徑>/mcp-rea.yaml
```

也可以把同樣的 `config.mcp` 塊寫進 `config/config.yaml` 的 `plugins:` 條目裡（效果相同）。

**注意**：路線 B 只解決「連接」，不解決「可見」——工具照樣要 `mcp_load` 才會進目錄。

## 驗證

1. 啟動日誌（路線 B）：`patch MCP connected  server_name=rea tools=<N>`；
2. `mcp_list` 能列出具名工具（說明連接與發現都成功）；
3. `mcp_load` 之後，工具名出現在你的工具清單裡，直接調用能拿到結果。

## 排錯對照表

| 症狀 | 原因 | 處理 |
|---|---|---|
| `connection refused` / 端口沒在聽 | 伺服器沒起，或起成了 **stdio** 模式 | 加 `--http --port N`；確認日誌有 `MCP transport listening` |
| `mcp_list` 說沒有伺服器 | 還沒連，或連的是另一個 DSC 進程 | 用 `mcp_connect` 在本會話連（不要 shell 起新 dsc） |
| 連上了，但工具清單裡看不到那批工具 | 正常：預設不曝光 | `mcp_load` 你要用的那幾個 |
| `mcp_load` 說名字找不到 | 名字記錯／沒加 `mcp__<server>__` 前綴 | 先 `mcp_list` 拿準確名字（裸名唯一時也可用） |
| HTTP 400 `Mcp-Session-Id header is required` | 客戶端沒回帶 initialize 下發的會話 id（舊版 bug，已修） | 升級 DSC |
| HTTP 400 `Parse error: Invalid JSON-RPC message` | 請求 id 超過 JS 安全整數 2^53（舊版用 UnixNano，已修） | 升級 DSC |
| HTTP 404 `Session not found` | 伺服器重啟過，會話失效 | DSC 會自動重新握手一次；持續出現則重連 |
| HTTP 406 `Not Acceptable` | 端點不是 streamable HTTP（可能是舊式 SSE 端點） | 換成 `/mcp` 端點 |
| 日誌 `patch MCP stdio transport not yet supported` | 配置寫了 `transport: stdio` | 改讓伺服器開 HTTP；stdio 目前不支援 |

## 安全邊界

MCP 工具的權限等於**本機執行 DSC 的使用者**（加載後即可被調用），請只連接 loopback 或你信任的位址；
不要把帶憑據的 endpoint 寫進會被提交的配置文件。
