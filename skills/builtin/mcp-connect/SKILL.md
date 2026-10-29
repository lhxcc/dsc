---
name: mcp-connect
description: 用 DSC 連接外部 MCP 伺服器（streamable HTTP 或 stdio 本機子進程）：運行期用 mcp_connect/mcp_list/mcp_load 接上並按需加載工具、讀取伺服器的 resources 與 prompts、或用 overlay 配置持久化；驗證 mcp__<server>__* 是否可用，並排查「連不上／看不到工具」的常見原因。
---

# mcp-connect — 用 DSC 連接 MCP 伺服器

當使用者說「用 DSC 連上某個 MCP 服務」「把 MCP 工具接進來」「為什麼 MCP 連不上」「連上了但看不到工具」時，照本技能做。

## 先記住三條

1. **不要**用 `shell` 再起一個 `dsc …` 進程去「連 MCP」。那是另一個進程，有它自己的工具表，
   當前會話永遠看不到那些工具，還會多一份會話與任務狀態。
2. **MCP 默認關閉，且只有人能打開**。開關屬於 MCP 插件自己（`/mcp on|off` 寫它的狀態
   文件，宿主不認識這個開關）。關閉時 MCP 相關工具**一個都不注入**——連入口 `mcp_connect`
   都沒有，目錄成本恰為 0。所以你會看到三種狀態：
   - 工具清單裡**完全沒有** `mcp_connect`/`mcp_list`/`mcp_load` → MCP 未啟用。請使用者
     執行 `/mcp on`（或請他在 `/mcp connect …` 裡自己連）。**不要**自己改配置文件，也
     不要起新進程繞過去。
   - 只有 `mcp_connect`（沒有 `mcp_list`）→ 已啟用但還沒連伺服器。直接 `mcp_connect`。
   - 有 `mcp_list`/`mcp_load` → 已連上伺服器，按下面流程用。
3. **連上 ≠ 看得見**。工具被接進宿主後仍**不進你的工具目錄**（一台伺服器可能有幾百個工具）。
   要用 `mcp_load` 明確加載，用完 `mcp_unload` 撤出。

## 兩種傳輸：先看伺服器是哪一種

| 傳輸 | 伺服器形態 | 連接方式 |
|---|---|---|
| **stdio** | 需要你啟動的命令（`node x.mjs mcp`、`python -m …`、`uvx …`、自帶二進制）。**多數 MCP 伺服器是這種** | 給 `command` + `args`，DSC 拉起子進程，講它的 stdin/stdout |
| **streamable HTTP** | 已經在監聽的服務（`http(s)://host:port/mcp`） | 給 `endpoint` |

判別方法：伺服器的說明文件有沒有讓你執行一個命令？有 → stdio；有沒有給 URL？有 → HTTP。
**不要**為了 HTTP 而去找「怎麼讓它開 HTTP 監聽」——stdio 是原生形態，直接用。

stdio 的兩條邊界（配錯會踩很久）：

- 子進程**只繼承基線環境**（`PATH`/`HOME`/`TEMP` 等），**不繼承宿主整份環境**。伺服器要的
  token 必須寫進 `env`——這是明確的授權動作，不是遺漏。
- 建議 `command` 直接寫解釋器或二進制，別用 `npx`/`uvx` 這類**啟動器**：被回收的是啟動器
  本身，POSIX 上它拉起的真實伺服器可能變成孤兒進程。一次性試用可以。

## 路線 A：運行期接上（推薦，不用改配置、不用重啟）

### 1. 連接伺服器

```
# stdio（推薦給本機命令型伺服器）
mcp_connect(server_name="srv", command="node", args=["O:/path/server.mjs", "mcp"], cwd="O:/path")

# streamable HTTP
mcp_connect(server_name="srv", endpoint="http://127.0.0.1:8765/mcp")
```

- `server_name` 決定工具名前綴：`mcp__srv__<tool>`，只允許 `[A-Za-z0-9_-]`，≤32 字符；
- 多台伺服器用不同 `server_name`（同名會互相覆蓋）；
- `transport` 可省略：給了 `endpoint` 就是 http，給了 `command` 就是 stdio；
- 這個工具需要審批（它等於對外擴張能力；stdio 還會在本機拉起一個進程）。被拒時停下來
  問使用者，不要改參數硬試。

回傳會告訴你發現了幾個工具——**那是註冊數，不是可見數**。

### 2. 看清有哪些工具，再按需加載

```
mcp_list(server="srv", query="unit")     # 名字 + 一行說明，不含參數 schema
mcp_load(tools=["mcp__srv__list_units", "mcp__srv__get_unit"])
```

- 只加載這次任務真正要用的。目錄每個請求都發：實測一台 229 個工具的伺服器佔 **約 7.4 萬 tokens**
  （1M 窗口 7%，128K 模型 57%），加載 10 個只要 ~2.5K；
- 加載在**下一跳**生效（工具目錄變了），不要在同一跳就假設能調；
- 裸工具名（`list_units`）在唯一時可用，重名會報歧義並列出候選。

### 3. 用完撤出

```
mcp_unload()                              # 全部卸載
mcp_unload(server="srv")                  # 卸載某台伺服器的
mcp_unload(tools=["mcp__srv__list_units"])
```

卸載只是把工具移出目錄，**連接還在**——再 `mcp_load` 即可回來，不用重連。
任務結束（或不再需要那批工具）就卸載，別讓它一直佔著每個請求的目錄成本。

### 4. 伺服器還可能有 resources / prompts（這兩類不是工具）

MCP 除了工具，還有兩種只讀能力，DSC 各給兩個訪問器：

| 工具 | 作用 |
|---|---|
| `mcp_list_resources` / `mcp_read_resource(uri=…)` | 伺服器暴露的內容（文檔、資料集、檔案）。讀回來是文字；二進位只回一句「已省略 + 型別 + 大小」 |
| `mcp_list_prompts` / `mcp_get_prompt(name=…, arguments={…})` | 伺服器的提示詞模板。取回來是渲染好的文字（每段帶 `[role]` 前綴） |

三條要點：

- 這四個工具**只在伺服器聲明該能力時才存在**。只有 tools 的伺服器（大多數）目錄裡完全沒有它們，
  所以工具清單裡找不到並不代表壞了。
- 它們**不需要 `mcp_load`**：不像幾百個伺服器工具那樣要挑，能力在就直接可用。
- 多台伺服器有同名資源／提示詞時必須傳 `server=`（不傳就報歧義並列出候選）——猜錯就是讀錯內容。

先列再讀：`uri` / `name` 要照列表原樣抄（伺服器自訂 scheme，例如 `file:///…`、`mem://…`）。

### 5. 目錄會自己更新（不用重連）

伺服器可以在運行期加減工具、資源、提示詞模板，並推送
`notifications/{tools,resources,prompts}/list_changed`。DSC 已經訂閱了這條通道：

- 收到通知就自己重新拉一次對應清單，**不需要你重連、也不需要使用者重啟**；
- 新出現的伺服器工具照舊**默認不可見**（要 `mcp_load`），消失的工具會從「已加載」裡自動摘掉；
- 資源/提示詞工具是即時讀插件快取的，`mcp_list_resources` 再看一眼就是新條數。

所以「我剛讓伺服器生成了一份內容，怎麼看不到」的正確做法是**再列一次**，而不是斷開重連。

## 使用者側的等價操作（斜杆命令）

使用者在終端裡可以自己做同樣的事，與你的工具共用一份宿主實現——所以他說「我自己連好了」
時，先用 `mcp_list` 確認，不要重連：

| 命令 | 作用 |
|---|---|
| `/mcp` | 狀態：開關、已連伺服器（含傳輸與目標）、已加載工具與其目錄成本 |
| `/mcp on` / `/mcp off` | 注入開關（默認關，插件寫自己的狀態文件，重啟保持）。關掉後模型看不到任何 MCP 工具（含 `mcp_connect`），連接與加載記錄保留 |
| `/mcp list [server]` | 列出工具（`[loaded]` 表示已在模型目錄裡） |
| `/mcp connect <name> <endpoint 或 command> [args...]` | 連接：`http(s)://` 開頭按 endpoint；其餘按 stdio 命令（這些都是插件聲明的命令，前端只負責轉發） |
| `/mcp disconnect [server]` | 斷開並註銷工具（不帶參數=全部；stdio 同時回收子進程） |
| `/mcp load <tool...>` / `--server <name>` | 加載進模型目錄 |
| `/mcp unload [tool...]` / `--server <name>` | 卸載（不帶參數=全部） |
| `/mcp resources [server]` | 列出資源（`uri`、描述、MIME） |
| `/mcp read <uri> [server]` | 讀一份資源（二進位只回省略說明） |
| `/mcp prompts [server]` / `/mcp prompt <name> [k=v …]` | 列出／渲染提示詞模板 |

## 路線 B：寫進配置（需要每次啟動都連時用）

```yaml
# mcp-srv.yaml（override tool-mcp 插件的配置：同名條目替換其 config）
- name: tool-mcp          # ★ 必須是插件名，這樣才是覆蓋而不是新增一實例
  type: tool
  enabled: true           # ★ 必須寫：省略等於 false，整條會被跳過（日誌會有 WARN）
  config:
    mcp:
      servers:
        - name: srv       # ★ 工具名前綴：mcp__srv__*
          # —— stdio ——
          transport: stdio
          command: node
          args: ["O:/path/server.mjs", "mcp"]
          cwd: O:/path    # 伺服器按相對路徑找資源時才需要
          # env:            # 只有這裡寫明的變量才會進子進程
          #   SOME_TOKEN: ""
        # —— 或者 streamable HTTP ——
        # - name: other
        #   transport: http
        #   endpoint: http://127.0.0.1:8765/mcp
```

```bash
dsc -patch <絕對路徑>/mcp-srv.yaml
```

也可以把同樣的 `config.mcp` 塊寫進 `config/config.yaml` 的 `plugins:` 條目裡（效果相同）。
現成示例：`config/examples/mcp-stdio/`（stdio）、`config/examples/mcp-memory/`（HTTP）。

**注意**：路線 B 只解決「連接」，不解決「可見」——工具照樣要 `mcp_load` 才會進目錄。

## 驗證

1. 啟動日誌（路線 B）：`patch MCP connected  server_name=srv transport=stdio tools=<N>`；
2. `mcp_list` 能列出具名工具（說明連接與發現都成功）；
3. `mcp_load` 之後，工具名出現在你的工具清單裡，直接調用能拿到結果。

## 排錯對照表

| 症狀 | 原因 | 處理 |
|---|---|---|
| `connection refused` / 端口沒在聽 | 走 HTTP 但伺服器沒起（或它本來就是 stdio 型） | 改走 stdio（`command` + `args`）；或確認它真的在監聽 |
| `start stdio MCP server …: executable file not found` | `command` 不在 `PATH`，或寫了相對路徑 | 寫絕對路徑，或先確認哪個解釋器存在 |
| `stdio MCP server … exited: …` | 子進程啟動即退出（參數錯、缺依賴、cwd 不對） | 先用同一個 `command/args/cwd` 在終端手動跑一遍，看它的報錯 |
| 工具清單裡連 `mcp_connect` 都沒有 | MCP 未啟用（默認關） | 請使用者 `/mcp on`；模型側沒有開關它的工具 |
| `mcp_list` 說沒有伺服器 | 還沒連，或連的是另一個 DSC 進程 | 用 `mcp_connect` 在本會話連（不要 shell 起新 dsc） |
| 連上了，但工具清單裡看不到那批工具 | 正常：預設不曝光 | `mcp_load` 你要用的那幾個 |
| `mcp_load` 說名字找不到 | 名字記錯／沒加 `mcp__<server>__` 前綴 | 先 `mcp_list` 拿準確名字（裸名唯一時也可用） |
| `mcp_list_resources` / `mcp_list_prompts` 報「no connected server exposes …」 | 那台伺服器沒有聲明該能力（多數只有 tools） | 正常；用它的工具即可，別去找不存在的資源 |
| `mcp_read_resource` 說 URI 不存在／歧義 | 抄錯 `uri`，或同 URI 出現在多台伺服器 | 先 `mcp_list_resources` 照抄；歧義時補 `server=` |
| HTTP 400 `Mcp-Session-Id header is required` | 客戶端沒回帶 initialize 下發的會話 id（舊版 bug，已修） | 升級 DSC |
| HTTP 400 `Parse error: Invalid JSON-RPC message` | 請求 id 超過 JS 安全整數 2^53（舊版用 UnixNano，已修） | 升級 DSC |
| HTTP 404 `Session not found` | 伺服器重啟過，會話失效 | DSC 會自動重新握手一次；持續出現則重連 |
| HTTP 406 `Not Acceptable` | 端點不是 streamable HTTP（可能是舊式 SSE 端點） | 換成 `/mcp` 端點 |
| 伺服器說要某個 token，但子進程讀不到 | stdio 子進程只繼承基線環境 | 把該變量寫進 `env`（顯式授權） |

## 安全邊界

MCP 工具的權限等於**本機執行 DSC 的使用者**（加載後即可被調用），請只連接 loopback 或你信任的位址；
stdio 等於在本機執行那條命令，連接前先確認 `command`/`args` 就是使用者想跑的那個。
不要把帶憑據的 endpoint 或 token 寫進會被提交的配置文件。
