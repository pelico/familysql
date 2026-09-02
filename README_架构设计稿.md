# familysql · 观察笔记

> 私人事件记录 + AI 分析系统。面向一个人自己用，本地单机单二进制部署，数据不走云，全留痕可推翻。
> 不是做"AI情感咨询"——**分析是辅助，结论必须自己下**。

---

## 一、这个工具是干什么的

记录生活里具体发生了什么（谁说了什么、做了什么），然后：

1. **回看** — 一段时间后翻，看出哪些模式在重复
2. **问** — 针对一个假设，AI 把支持/矛盾的事实都列出来，不下结论
3. **构建假设** — 允许多条事实合并推断，但必须用假设句式、标置信度、引用 event_id
4. **复检** — 新事实出现后，拿之前保存的分析结论对照，校准对 AI 的信任程度
5. **回复草拟** — 针对配偶发来的一条消息，生成"当场怎么回"(仅情绪确认+延后表态，不含任何说理) + "事后复盘参考"(基于历史互动样本)，两块物理分离
6. **AI 识图记录** — 粘贴聊天截图，自动识别对话/事件，生成待审核候选后入事实库

**核心设计原则：**

| 原则 | 具体含义 | 为什么 |
|---|---|---|
| **只记录，不诊断** | 事实只写"摔了碗"，不写"XX在生气"；标签是关键词，不是人格定性 | 标签一旦贴上，后续检索/分析都会强化自我暗示 |
| **AI 没资格代填主观判断** | `severity_self`（严重度自评）AI 不填，必须用户手选；`valence`（事件性质）AI 只给建议值供确认 | 严重度是你的主观感受，AI 瞎填 1=默认轻微，相当于替你做了一次隐性判断 |
| **输出必须可回溯** | 任何分析结论每条都要标 event_id 引用；修正事实走"留痕"（写 corrections 表，不是硬改 events） | AI 是黑盒，你要知道它每句话站在哪条记录上；改数据也必须知道改前是什么 |
| **提示词可改不用重部署** | 5 个分析模式的系统 prompt 全部在设置页可调，改完即时写入 DB，提问生效 | 你会想改措辞/增加约束/细化输出格式，每次改完重新编译容器不值得 |
| **采样偏差要诚实** | 如果你只记冲突事件（人普遍如此），提问时系统会自动在 prompt 里加一段强制声明："本样本全是高严重度，缺乏平和互动，不得仅凭这些下'关系整体如何'的判断" | LLM 几乎不会主动说"你没给我看全貌"，必须代码算好分布硬塞给它 |

---

## 二、技术栈 + 部署

- 语言：Go（Gin）
- 数据库：SQLite + **FTS5（trigram 分词器，支持中文子串检索）**
- 前端：单页原生 JS（无框架，`index.html` 单文件）
- 多模态：消息支持 base64 内嵌图片（`image_url` 结构，OpenAI 兼容接口）
- LLM 接口：OpenAI 兼容接口（任何服务商/本地 ollama 都能用），支持**多模型并行**（选 2-3 个同时跑同一条问题，横向对比回复）
- 并发模型：每个请求在 **sessionMessagesHandler** 里用 `sync.WaitGroup` 并发执行「分析回复生成」和「事实提取」，提取不阻塞主对话

**部署：**

```bash
# 编译（FTS5 需要 CGO + sqlite_fts5 tag）
CGO_ENABLED=1 go build -tags sqlite_fts5 -o familysql .

# 运行（默认端口 18080，数据写入 ./data/database.db）
./familysql
```

**环境变量（可选注入模型配置）：**

`LLM_API_BASE` / `LLM_API_KEY` / `LLM_MODEL` — 首次启动没任何模型时会作为默认活跃模型注入，之后设置页可以手动再加多个。

数据全部在 `./data/`，备份只需拷贝 `data/database.db*`（WAL 模式会有 `db-wal`、`db-shm` 伴生文件，建议用 SQLite `.backup` 命令导出完整快照）。

---

## 三、数据模型

### 3.1 核心表

```
events                         事实（最小单位）
├─ id                         PK
├─ timestamp                  ISO8601 带时区（RFC3339）
├─ people                     人物（逗号分隔自由文本）
├─ tags                       标签（逗号分隔自由文本）
├─ severity_self              严重度主观自评 1-5（不允许 AI 代填）
├─ valence                    事件性质：conflict / neutral / positive（AI 建议，用户确认）
├─ content                    事实正文（客观描述，不写推断）
├─ status                     raw / reviewed（审核写入时置 reviewed）
└─ created_at

events_fts                     FTS5 虚拟表（trigram 分词器）
  content, tags, people       三列同步索引，INSERT/UPDATE/DELETE 由触发器自动同步

corrections                    修正留痕（改 events 前先写旧值进这张表）
├─ event_id, field, old_value, new_value, reason, corrected_at

analyses                       保存的分析结论（手动点"保存为分析结论"写入）
├─ based_on_event_ids         基于哪些 event_id 得出
├─ agent_used                 哪个模型/profile
└─ output                     全文

sessions                       会话（多轮分析对话）
├─ mode                       pattern_query / contradiction_check / hypothesis_only / review / response_draft
├─ filter_people / filter_tags 限定事实检索范围（可空）
├─ profile_ids                并行跑哪几个模型（JSON 数组，空=用全部活跃）
├─ fact_profile_id            事实提取专用模型 ID（避免和分析同一 API 撞 429）
└─ messages                   JSON：Turn 数组（用户内容+图片 + AI 多模型回复 + 候选事实）

interaction_patterns           情境-反应对照库（给"回复草拟"模式 Block B 用）
├─ person                     哪个对象（如"配偶"）
├─ trigger_context            触发情境（如"她发消息说'你从来都不关心'"）
├─ observed_pattern           观察到的固定模式（自由文本）
├─ ref_event_ids              引用哪些 event_id
├─ response_tried             试过哪些应对方式（用 " | " 分隔历次）
├─ outcome                    对应结果（历次用 " | " 分隔）
└─ observation_count          同类情境发生次数（<3 时前端标"样本不足"）

llm_profiles                   模型配置列表
├─ name, api_base, api_key, model, is_active
└─ 新增/激活/切换都在设置页完成，不重启

mode_prompts                   模式 prompt（设置页改的就是这张表）
├─ mode                       PK：pattern_query / contradiction_check / ...
├─ prompt_text                用户自定义 prompt（空=没改过，用代码默认）
└─ updated_at

events_fts_config              SQLite FTS 内建元数据表（里面存 tokenize='trigram'，用于迁移检测）
```

### 3.2 为什么 severity_self 和 valence 是两个正交维度

| 事件 | severity_self（你的主观影响） | valence（行为客观性质） |
|---|---|---|
| "我爸今天忘记吃降压药" | 4（可能很严重） | neutral（客观事件，无正负） |
| "她发来消息说'你从来不关心'" | 3 | conflict（冲突） |
| "周末一起做了顿饭，气氛轻松" | 1 | positive（正面） |
| "她翻旧账吵了半小时" | 4 | conflict（冲突） |

如果没有 valence，全靠 severity 近似："一起做饭"和"轻微小摩擦"都是 severity=1，完全不可区分。
- severity_self：**纯主观**，AI 没资格代填 → 用户手填
- valence：**相对客观**（摔碗=冲突、做饭=正面是行为本身决定的，不需要你主观感受）→ AI 在提取时给建议值，用户审核面板确认

---

## 四、核心流程

### 4.1 发送一条分析消息（会话内）

```
                              ┌──────────────────────────────┐
                              │  用户在会话消息输入框：       │
                              │  文字 + 可选粘贴截图 (Ctrl+V) │
                              └──────────────┬───────────────┘
                                             │
                              ┌──────────────▼───────────────┐
                              │ POST /api/sessions/:id/message│
                              └──────────────┬───────────────┘
                                             │
                             ┌───────────────▼───────────────┐
                             │ sessionMessagesHandler 后端    │
                             │   · parse 会话（mode/筛选）     │
                             │   · fetchContextEvents(事实 A+B │
                             │     两路检索，取并集)           │
                             │   · 命中 response_draft 还额外  │
                             │     拼 interaction_patterns 入   │
                             │     context                    │
                             │   · 检测采样偏差（全高严重度）  │
                             │   · 选 profile（多模型并行）    │
                             └───────────────┬───────────────┘
                                             │
                        ┌────────────────────┼───────────────────┐
                  sync.WaitGroup             │            sync.WaitGroup
                  并发 ────────────┐         │  ┌──────────────并发
                                   ▼         ▼  ▼
                     ┌────────────────────┐  ┌────────────────────┐
                     │ 多模型分析回复生成  │  │ runFactExtract    │
                     │ (profile_ids 指定  │  │ (独立 profile)     │
                     │   的每一个并行)    │  │  独立 prompt       │
                     │                    │  │  视觉支持图片      │
                     │ mode prompt +      │  │  输出 JSON 结构化  │
                     │ 事实 block + 采样  │  │  人物/时间/标签/   │
                     │ 偏差声明 + patterns│  │  valence建议/内容  │
                     │ → system message    │  │  AI 不填 severity  │
                     └─────────┬──────────┘  └─────────┬──────────┘
                               │                      │
                               ▼                      ▼
                  PerProfileReply[]（多模型）   []FactCandidate(候选事实)
                               │                      │
                               └──────────┬───────────┘
                                          ▼
                              Turn 结构体写入 sessions.messages
                                          │
                                          ▼
                        返回前端：AI 回复卡片（多模型横向并排）
                                   + 下方待审核事实面板
```

**关键架构点：**

- **分析和提取互相隔离**：用不同 profile、不同 prompt、不同函数，避免"提取转录"和"综合推断"互相污染
- **事实提取失败不拖累主对话**：提取出错时生成一条 `status=extract_error` 的候选（留内容给用户手动改），AI 回复照常返回——对话永远不因为提取崩而卡死
- **候选事实先挂在 Turn 上，不进 events**：只有用户点"确认写入事实库"才落 events 表（status=reviewed）。这就是"审核闭环"——AI 提取结果默认是草稿，数据库里 events 只有用户手确认过的
- **事实检索是「路径 A ∪ 路径 B」两路取并集**（见 `fetchContextEvents`）：
  - **路径 A（硬过滤）**：按 `filter_people` / `filter_tags` 用 `LIKE` 匹配，按时间倒序取最近 **30 条**
  - **路径 B（语义联想）**：用户提问非空时，把问题原文直接丢进 `events_fts MATCH ?` 做 FTS5（trigram）检索，按 BM25 rank 取 **20 条**
  - 两路结果通过 `map[id]EventRow` 去重合并 → 按时间倒序排序 → 最后截断到调用方传入的 `topN`
  - 设计意图：路径 A 是"你明确想限定范围"的闸门，路径 B 是"万一你不填筛选条件也能查到点相关"的兜底
- **温度参数分层**：分析回复用 `temperature=0.2`（允许一定的措辞多样性），事实提取用 `callLLMForJSON` 的 `temperature=0.1`（越低越稳定，结构化输出更一致）。事实提取失败时 `stripFencedJSON` 会先剥 ```json``` 围栏，再用 `strings.Index` 从文本里抠最外层 `{...}` 兜底——兼容有些模型喜欢 JSON 前后加废话的坏习惯

### 4.2 新建会话（选 mode / 模型 / 人物限定）

- `mode`：5 选 1。不同 mode 是完全不同的 system prompt，本质是把 AI 临时调教成不同角色
- `人物限定 / 标签限定`：`filter_people` / `filter_tags`，`fetchContextEvents` 的**路径 A** 会按这两个字段过滤 events
- `并行模型`：多选，多选几个就跑几次 LLM，横向对比不同模型的回复（很实用，尤其是某模型特别会分析、某模型特别会措辞时）
- `事实提取专用模型`：单独选一个。因为"分析"和"提取"在同一请求下同时发，如果两者走同一个 API Key/Endpoint，某些 API 有并发限制很容易 429

### 4.3 采样偏差检测（代码硬塞元认知警示）

触发条件（保守，避免误报）：当前用于分析的事实集合 ≥3 条，全是 severity≥3 高严重度，且没有任何 severity≤2 的相对平和记录。

触发后 system prompt 末尾硬接一段（不管 AI 想不想要）：

```
【采样局限提示：当前事实集合共 12 条，其中 12 条为高严重度(≥3)事件，
未包含任何严重度≤2 的相对平和互动。这是数据采集层的偏差...任何关于「关系整体状态/是否值得维系」
的推断必须声明这一局限，不得仅凭冲突样本下整体性结论。】
```

这是**在数据层修不了的前提下，分析层的必做补丁**——因为人普遍只在出问题时才记东西，正面/平和的瞬间没动机记，时间一长 DB 里全是冲突，任何分析都会偏悲观。LLM 几乎不会主动说"你只给了我负面切片"，只能我们自己算好分布强制喂。

### 4.4 回复草拟模式（response_draft）的 Block A/B 隔离

#### Block A 和 Block B 分别是什么，解决什么问题

你的真实沟通模式里存在一个反直觉现象：**「先安抚情绪、紧接着讲几句道理」——这一整串会被对方读成「敷衍、不走心」**。于是一条建议被拆成完全不同使用时机的两块：

| | Block A：当场回复候选 | Block B：复盘参考 |
|---|---|---|
| **使用时机** | 对方消息刚发来，你 10 秒内要回的那个瞬间 | 发完 A 之后几小时、或几天后安静坐下复盘的时候 |
| **内容** | 2-3 个版本，**只有情绪确认 + 延后表态**。比如「收到，我先把手上的事处理下，晚上回家说」 | 该类矛盾历史上出现过几次、之前试过什么应对、结果怎样、可能的谈判方向 |
| **直接发给对方？** | ✅ 是（选一版或自己改一下发） | ❌ **绝对不能发**——里面有你单方面对历史的解读，直接转发等于翻旧账 |
| **长度约束** | 每版 ≤2 句，不能说理 | 可以详细，带 event_id 引用，带「基于单方面数据，非对方真实立场」声明 |
| **数据来源** | 只看本轮你提供的对方原话 + 你的草稿 | `interaction_patterns` 表的历史模式样本 + `events` 表相关事实 |

两块之间**不能互相渗透**——只要 Block A 出现任何「你其实可以换个角度想」「上次我们就说好了」之类的内容，哪怕是好意，都等于把「讲道理」塞进了「当场回复」这个窗口，产品目标就破了。所以这不是推荐性的「建议分开写」，是结构性的「必须」。

#### 两层隔离分别是什么、各隔离什么

这里说的「隔离」不是网络/权限层面的隔离，是**内容层面 + 使用时机层面**的双重隔离，共两层（不是三层，安全边界第 5 条里之前的表述不准确，已修正）：

**第一层：Prompt 硬约束（隔离内容边界）**

`response_draft` 的 system prompt 要求输出必须用**两个固定标题**分隔：
```
## 当场回复候选
1. ……
2. ……

## 复盘参考
……
```

并且在 prompt 里逐条写明渗透禁令：
- Block A：**禁止**包含建议、说理、辩解、引用历史事件、提出解决方案、任何形式的「但其实……」
- Block B：必须在开头注明「样本数 N（<3 时标"样本不足，谨慎参考"）」；每条分析性结论必须引 event_id；涉及对方动机的判断必须声明「基于单方面数据推断，非对方真实立场」

这层的作用：**告诉模型两块的边界在哪，减少它自己乱混的概率**。是软约束（LLM 可能不听），但提供了确定性的结构化锚点，让前端第二层可以基于标题解析渲染。

**第二层：前端物理隔离（隔离使用时机）**

就算 prompt 写得再清楚，你在情绪紧张的当下（对方消息刚发过来），如果眼前同时出现 A 和 B 两块长文，很容易下意识把 B 里的几句也复制粘贴出去——这是人之常情，不是靠意志力防得住的。所以前端做了物理分离：

- **Block A 默认展开**：绿色背景、大号字体、每版一个「复制」按钮。这块是你打开页面第一眼看到的、唯一能直接复制的内容。
- **Block B 折叠在 `<details>` 标签里**：紫色背景、默认不展开，标题写着「🔒 复盘参考（事后看，不要直接转发）」。必须手动点一下才能展开。里面的文本块旁边不设「复制」按钮（或者复制按钮很小、且有明显警告），就是为了提高「一冲动就转发」的操作成本。
- Turn 下方再挂一个「📝 记录这次结果」小表单：只有展开 Block B 后才看得见，引导你把「真正发出去的回复是什么 / 对方实际反应是什么」事后回填，不鼓励当场就写复盘。

这两层共同的效果：**最坏情况下，即使模型偷偷把一点说理塞进 Block A，你也只能复制到 Block A 那有限的几句（2 句 × 2-3 版 = 最多 6 句可复制），不会在情绪上头时把整段 Block B 的分析性内容一键甩出去**——这就是这个设计要守住的底线。

#### 事后回填（upsert，和隔离无关的第三条机制，之前混在「三层」里是表述错误）

`POST /api/interaction_patterns/upsert` 按 `person + trigger_context` 匹配已有记录：
- 匹配到：`observation_count+1`，`response_tried` / `outcome` / `ref_event_ids` 用 ` | `（或逗号）分隔**追加**，历次保留不覆盖
- 没匹配：新建一条，count=1

这是冷启动积累样本用的：前期 Block B 会反复提示「暂无结果记录，建议本次事后补记」，坚持回填 2-3 次同类情境，Block B 就能看到「上次我这么回 → 对方更生气」这种自己对自己的学习记录。不是隔离机制，是 self-supervised 的数据飞轮。

### 4.5 FTS5 中文检索的坑 + 为什么用 trigram

原始代码用默认 unicode61 分词器，中文会**整段连续中文字符当成一个 token**——检索只有查询字符串是原文完整连续子串才命中，实测"甲亢""盐"等正常关键词全返回空，本质是哑巴功能。

现在 schema 是：

```sql
CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(
  content, tags, people,
  content=events, content_rowid=id,
  tokenize='trigram'
);
```

trigram = 三字滑动窗口："我今天去医院"会切成"我今天" "今天去" "天去医" "去医院"。只要查询字符串长度 ≥3，基本就能用任意子串命中。

**迁移策略（新部署 / 旧数据都兼容）：**

DB 初始化后检查 `events_fts_config`（SQLite FTS 内建元数据表）里的 `tokenize` 值，如果不是 `trigram`：
1. `DROP TABLE events_fts`
2. 重建 trigram 版
3. `INSERT INTO events_fts SELECT ... FROM events` 回填所有旧记录

一次迁移永久生效，下次启动跳过。

### 4.6 多模态消息封装（图片是怎么传给 LLM 的）

前端粘贴/上传图片后，把图片转成 **base64 Data URL**（`data:image/png;base64,xxxx`），放在消息里的 `image_data` 字段发给后端。后端在 `llm.go` 里用统一的消息结构处理：

```
APIMessage {
  Role: "user" | "system" | "assistant"
  Content: 普通文本          ← 纯文本模式下用这个
  ContentParts: [            ← 有图时非空，toWire() 序列化时自动把 content 变成数组
    {Type:"text", Text:"用户打的文字"},
    {Type:"image_url", ImageURL: {url: "data:image/png;base64,..."}}
  ]
}
```

序列化逻辑在 `APIMessage.toWire()`：`ContentParts` 为空就走标准 `{role, content:string}`；非空就走 `{role, content: [...]}`（OpenAI 视觉兼容格式）。这样写一份代码同时兼容纯文本模型和多模态模型——纯文本模型的 Endpoint 如果遇到 `image_url` 报错，调用外层会走降级（事实提取那边的 `runFactExtract` 就有 try/catch 语义：提取失败不影响主对话）。

**调用封装分层：**

| 函数 | temperature | 用途 | 返回 |
|---|---|---|---|
| `callLLMByProfile` | 0.2 | 主对话分析回复（5种 mode） | 原文 `string` |
| `callLLMForJSON` | 0.1 | 事实提取 / 结构化输出候选事实 | 解析到 Go struct，失败会做两层兜底（剥围栏 → 抠大括号） |

---

## 五、模式说明（对应 5 个 mode）

### 5.1 pattern_query 模式查询
只做事实罗列。输出必须：(1) 按主题分组 (2) 每条结论标引用 (3) 证据不足直接写"证据不足，暂无法判断"。适合"最近发生了什么"类提问。

### 5.2 contradiction_check 矛盾检测
针对用户的一个假设，分别列"支持的事实"和"矛盾的事实"，每条引 event_id。不替下结论。适合验证直觉。

### 5.3 hypothesis_only 假设构建
允许综合推断，但每条：(1) 假设句式 (2) 置信度（按引用数量+时间跨度硬算，**不是 AI 自我感觉**）(3) 只在证据真实分歧时补"其他可能读法"（非互斥不包装对立）(4) 引 event_id。输出不分正反小标题，每条一段，末尾收"不确定性："。

**置信度标准（非主观）：**
- 单事件/同一天 → 低
- ≥3条、跨≥3天、不全是同一次互动 → 中
- ≥5条、跨≥1周、不同互动场景 → 高
- 输出时注明依据："（中置信度：4条事件支持，跨 6 天）"

### 5.4 review 复检校准
拿"之前保存的分析结论"对照新增的事实，列"被印证的部分 / 被削弱的部分 / 建议修正方向"。作用是：**校准你对 AI 分析的信任**——AI 的错误不是天塌下来，只要你定期复检，它其实可以作为"自己之前不成熟想法"的留存快照。

### 5.5 response_draft 回复草拟
见 4.4 节。和前 4 个 mode 的关键区别：**不做关系整体诊断**，只解决"这次该怎么回"。

---

## 六、前端结构（单页无框架）

`public/index.html` 一个文件搞定，分几块：

**全局布局：**
```
┌─ 侧栏 sidebar-nav (≥1024px 常驻，<1024px 汉堡) ─┐ ┌─ 顶栏 app-header ──────────────────┐
│ 观察笔记 Logo                                        │ │ ☰汉堡(小屏) Logo 观察笔记    新建会话 ⚙ │
│ · 分析 / 事实库 / 校准                              │ └─────────────────────────────────────┘
└─────────────────────────────────────────────────────┘
┌─ 主区 main-wrap ──────────────────────────────────────────────────────────────────┐
│ Tab 分段器 tab-bar-wrapper (<1024px 出现，侧栏隐藏时作为模块切换)               │
│                                                                                   │
│ 分析 Tab: 新建会话卡片 / 历史会话列表 / 已保存分析结论                            │
│ 事实库 Tab: 筛选栏(人物/标签/搜索) + 共 N 条 + 卡片列表(ev-card)                 │
│ 校准 Tab: 校准仪表盘 + 修正过的事件列表                                            │
└───────────────────────────────────────────────────────────────────────────────────┘
```

**可扩展点（目前没做，但数据结构都有了）：**
- 校准页顶部统计卡片：总事件数 / 各 mode 画像 / prompt 版本 vs 准确率
- 事实库卡片显示 valence 彩色 pill（目前 DB 已返回，前端可按 valence 色区分）
- 定期提示："这周只记了 N 条冲突，0 条正面，建议补记缓和瞬间"（基于 valence 分布触发，需要样本积累后才有意义）
- response_draft 待回填汇总列表（目前只有 turn 挂表单，容易漏）

---

## 七、Prompt 版本管理

设计取舍：**prompt 不做成独立的版本化 txt 文件，默认写在 Go 代码常量里，但运行时可被 DB 覆盖。**

理由：单二进制部署。如果 prompt 是外置 txt，每次编译要嵌入资源（embed），反而多一层复杂度；改 prompt 走 DB 覆盖已经达到"不重新部署"的目的。

**运行时生效链路：**
```
新建会话 → 发送消息
    │
    ▼
getEffectiveModePrompt(mode)
    │
    ├─ 读 DB mode_prompts 有没有对应 mode 记录
    │     有  → 用用户自定义的
    │     无  → 回退 modeSystemPrompt()（代码默认值）
    │
    ▼
拼上 facts_block + 采样偏差声明 + patterns_context(仅response_draft)
    │
    ▼
调用 LLM
```

**设置页操作：**
- GET `/api/mode-prompts` 返回 5 个 mode 当前生效 prompt，带 `is_default` 标志
- PUT `/api/mode-prompts/:mode` → `INSERT OR REPLACE` 写 DB，下次提问立刻用
- DELETE `/api/mode-prompts/:mode` → 删记录，下次提问自动回退代码默认（相当于恢复默认）

---

## 八、安全边界（什么功能是故意不做的）

1. **AI 不做诊断**：prompt 里有约束，但这是软约束。AI 可能越界说出"父亲处于创伤共生受害者状态"这类话——**这是你要盯的，不是系统能 100% 防的**。约束被压缩后安全边际变小，如果你发现 AI 频繁越界，把诊断性黑名单补进对应模式的 prompt（设置页就能改，不用重新部署）。
2. **AI 不判断关系整体好不好**：即使 hypothesis_only 也是"假设句式 + 置信度 + 不确定性收尾"，不是判决书。
3. **AI 不提供法律/医疗/心理专业建议**：任何 prompt 里都没这条——你如果要加，在设置页改就行。
4. **事件性质 valence 是建议值**：AI 给出的 conflict/neutral/positive 是基于行为文本的客观分类，但**最终必须你确认**——系统在审核面板提供了下拉，可以改。
5. **Block A 可能越界（且只有两层，不是三层）**：「Block A/B 隔离」实际是两层——① Prompt 硬约束（固定标题分隔 + A 禁止说理）② 前端物理隔离（A 默认展开、B 折叠）。之前写成"三层"是把事后回填的 upsert 机制误算进去了，upsert 是冷启动积累样本的，和隔离无关，这点已在 4.4 节更正。另外，即使 prompt 写了"禁止说理"，模型仍可能在"我理解你"之后偷偷塞一句"但其实可以……"。这属于 LLM 本身对齐局限，产品能做的是「最多可复制 2-3 版、每版 2 句」——把越界的可能泄漏面压缩到最小，不是 100% 杜绝。

---

## 九、典型使用步骤（新人上手）

1. **启动后先配模型**：右上角设置 → 新增模型（API Base / Key / Model）→ 保存并激活为并行模型（至少 1 个，建议 2 个对比）
2. **先记第一批事实**：在"新增事实"页，用"AI 识图记录"贴 5-10 张截图 → 审核面板每张改时间/严重度/人物/标签/性质 → 确认写入事实库。或者用"快记"手写更方便的琐碎记录。
3. **开第一个分析会话**：选假设构建模式，人物限定写"XX"（重要——不然 patterns context 和检索范围都不准），随便发一个观察到的现象/假设，看输出
4. **如果分析偏悲观**：这是正常的——你刚开始只记了冲突事件。系统会自动在 system prompt 里加采样局限提示，AI 自己会声明"基于冲突切片"；之后有意识记几条正面/平和的记录，偏差会自然缓解
5. **遇到矛盾想开怎么回**：开回复草拟模式，人物限定填对方，把对方的话 + 你想回的草稿一起发 → 从 Block A 选一版发 → 过几天回这个会话填"记录这次结果"
6. **想调 prompt**：设置页 → 提示词管理 → 改对应模式 textarea → 保存。发一条新消息立刻用新版。改坏了点"恢复默认"。

---

## 十、文件索引（给读代码的人）

```
main.go                       路由注册 + DB schema + 迁移（events 加列 / FTS 分词器切换）
  · POST /api/events          新增事实（手写）
  · GET /api/events           事件列表（支持 people/tags/severity_min/max/valence 筛选）
  · GET /api/events/search    FTS全文检索
  · GET /api/events/:id       单条详情
  · POST /api/events/:id/correct 修正（写 corrections 留痕）
  · GET /api/metadata         人物/标签去重（前端下拉）
  · POST /api/sessions        新建会话
  · GET /api/sessions         会话列表
  · GET /api/sessions/:id     会话详情
  · POST /api/sessions/:id/messages 发消息（核心分析入口）
  · POST /api/sessions/:id/candidates/:idx 单条候选事实确认/丢弃
  · POST /api/sessions/:id/batch-confirm 候选批量确认
  · POST /api/events/vision-extract AI识图候选生成
  · POST /api/interaction_patterns 新建互动模式
  · GET  /api/interaction_patterns  互动模式列表（按person筛选）
  · POST /api/interaction_patterns/upsert 回填（按person+trigger更新，无则新建）
  · GET/PUT/DELETE /api/mode-prompts[:mode]  模式prompt管理

handlers_analyze.go           核心业务逻辑
  · validModes / modeSystemPrompt  5个模式的代码默认prompt
  · getEffectiveModePrompt         DB优先，回退默认
  · factExtractSystemPrompt        事实提取专用prompt（独立，不做推断）
  · runFactExtract                 事实提取调用（独立profile，可降级）
  · fetchContextEvents             事实检索（路径A 筛选条件扫30条 ∪ 路径B FTS检索20条）
  · samplingBiasNotice             采样偏差检测（≥3条全高严重度时返回声明）
  · buildPatternsContext           response_draft 拼历史互动模式样本
  · sessionMessagesHandler         发送消息主函数（并发提取+分析）
  · confirmFactCandidateHandler    单条候选确认（写 events）
  · batchConfirmHandler            批量候选确认（写 events）
  · correctEventHandler / listCorrectionsHandler  修正留痕

llm.go                        LLM 调用封装
  · callLLMByProfile           普通文本回复（流式？不，当前同步返回全文）
  · callLLMForJSON             结构化JSON回复（给事实提取用，会剥 fenced JSON）
  · APIMessage / toWire()      自动根据有无图片生成 text 版 / multi-modal 版请求

public/index.html             单页前端（原生 JS，无框架）
  · switchTab / 分段器 / 侧栏汉堡
  · renderSessionList / renderActiveSession / renderTurn
  · renderResponseDraftBlocks     response_draft A/B 分块渲染
  · renderResponseDraftResultForm 回填小表单 + submitDraftResult(upsert)
  · loadTimeline / renderFacts    事实库列表（含修正留痕弹窗）
  · openSettings / renderProfiles 模型管理
  · renderModePrompts             提示词管理（5个模式textarea+保存+恢复默认）

go.mod                        依赖：gin-gonic/gin + mattn/go-sqlite3
```

---

## 十一、设计上的已知取舍（不是 bug，是 trade-off）

| 取舍 | 现在这样做的理由 | 代价 |
|---|---|---|
| prompt 默认写死 Go 常量 + DB 覆盖 | 单二进制部署，不引入 embed 或外置文件管理 | 追溯"某次分析用了哪版 prompt"需要结合 mode_prompts.updated_at 和 turn.created_at，不能精确匹配版本号 |
| severity_self 纯手填，AI 不代填 | 守住"主观感受属于你的判断"这条底线 | 每张候选卡片要多点一下选 1-5，录入摩擦略高 |
| FTS 用 trigram，不用 jieba/中文分词 | SQLite 内置，零依赖部署 | 两字词查询（如"甲亢"）匹配差（trigram 要求至少 3 字）；可以搜"甲亢病"/"的甲亢"等变体绕过 |
| 没有"正面事件强制记录" | 工具是辅助，不能替你决定什么值得记 | 采样偏差永久存在；只能靠分析层声明警示 + 你自己有意识补记 |
| response_draft Block B 数据冷启动难 | 前 2-3 次同类情境，Block B 全是"暂无样本，建议补记" | 需要你坚持回填几次才能看到价值；这是自积累工具的必然 |
| 前端无框架单文件 | 部署/阅读简单，单个 index.html 随便改 | 文件会越来越大（目前已经 100KB+）；复杂交互写起来不如 Vue/React 舒服 |
