package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gin-gonic/gin"
)

// EventRow 用于分析上下文拼装的事实行
type EventRow struct {
	ID        int
	Timestamp string
	People    string
	Tags      string
	Severity  int
	Valence   string // "conflict" / "neutral" / "positive" / "" 未分类
	Content   string
}

// FactCandidate 是某一轮对话中被提取出来、等待人工确认的候选事实。
type FactCandidate struct {
	ID                 int    `json:"id,omitempty"` // 落库后的事件ID（确认后填充）
	SuggestedTimestamp string `json:"suggested_timestamp"`
	People             string `json:"people"`
	Tags               string `json:"tags"`
	Severity           *int   `json:"severity,omitempty"`
	Valence            string `json:"valence,omitempty"` // "conflict"/"neutral"/"positive"/"" AI建议，用户确认
	Content            string `json:"content"`
	Status             string `json:"status"` // "pending" / "confirmed" / "dismissed"
	TimestampUncertain bool   `json:"timestamp_uncertain,omitempty"`
	ExtractError       string `json:"extract_error,omitempty"`
}

// PerProfileReply 是单个模型对一轮问题的回复；某个模型失败时 Error 非空。
type PerProfileReply struct {
	ProfileID     int    `json:"profile_id"`
	ProfileName   string `json:"profile_name"`
	Model         string `json:"model"`
	Reply         string `json:"reply"`
	UsedEventIDs  []int  `json:"used_event_ids"`
	PromptVersion string `json:"prompt_version"`
	Error         string `json:"error,omitempty"`
}

// Turn 是一轮完整问答。Replies 存多模型回复（并行返回）。
// Candidates 是本轮（从用户输入+图片里）提取出的待审核事实。
// ImageData 存 base64 data URI（仅最新一轮前端会显示，历史轮不重复存以节省空间）。
type Turn struct {
	UserContent    string            `json:"user_content"`
	HadImage       bool              `json:"had_image"`
	ImageData      string            `json:"image_data,omitempty"` // data:image/jpeg;base64,...
	Replies        []PerProfileReply `json:"replies"`
	Candidates     []FactCandidate   `json:"candidates,omitempty"`
	CreatedAt      string            `json:"created_at"`

	// 兼容旧数据（历史 Turn 只有 AssistantReply / UsedEventIDs / PromptVersion）
	AssistantReply string `json:"assistant_reply,omitempty"`
	UsedEventIDs   []int  `json:"used_event_ids,omitempty"`
	PromptVersion  string `json:"prompt_version,omitempty"`
}

func normalizeLegacyTurn(t *Turn) {
	if len(t.Replies) > 0 {
		return
	}
	if t.AssistantReply == "" && len(t.UsedEventIDs) == 0 && t.PromptVersion == "" {
		return
	}
	t.Replies = []PerProfileReply{{
		ProfileID: 0, ProfileName: "(legacy)", Reply: t.AssistantReply,
		UsedEventIDs: t.UsedEventIDs, PromptVersion: t.PromptVersion,
	}}
	t.AssistantReply = ""
	t.UsedEventIDs = nil
	t.PromptVersion = ""
}

var validModes = map[string]bool{
	"pattern_query":       true,
	"contradiction_check": true,
	"hypothesis_only":     true,
	"review":              true,
	"response_draft":      true,
	"daily_connect":       true,
}

// modeSystemPrompt 按 mode 给出系统提示（prompt_version = "1.0" 固定版本，后续升级可记录）
const promptVersion = "1.0"

func modeSystemPrompt(mode string) string {
	switch mode {
	case "pattern_query":
		return `你是家庭/人际观察助理。只做事实汇总与模式罗列，不下任何定性评价，不贴人格标签。输出必须：(1) 按主题分组；(2) 每条结论标注 event_id 引用，如 "【引用 #3 #7】"；(3) 如证据不足直接写"证据不足，暂无法判断"。`
	case "contradiction_check":
		return `你是矛盾检测助理。针对用户的假设，分别列出"支持的事实"和"矛盾的事实"，每条标注 event_id。不替用户下最终结论。`
	case "hypothesis_only":
		return `你是假设构建助理。基于事实集合提出综合推断，遵守以下规则：

(1) 每条结论只给一个主要推断，用假设句式（"可能""倾向于"），不要为了显得客观而制造对立解释。

(2) 只有当证据本身存在真实分歧（不同 event_id 指向不同结论）时，才补充一句"其他可能读法"，控制在一行以内，不展开成完整段落。如果两种解读并不互斥（比如都能同时成立），不要把它们包装成对立项。

(3) 置信度不是自我感觉，必须基于引用的 event_id 数量和时间跨度给出：
    - 单一事件或同一天内的事件 → 低
    - 3条以上、跨至少3天，且并非全部来自同一次互动 → 中
    - 5条以上、跨至少1周，且来自不同互动场景 → 高
    在置信度后面注明依据，如"（低置信度：仅1条事件支持）"。

(4) 每条结论引用 event_id，格式【引用 #3 #7】。

(5) 禁止人格定性、禁止对当事人动机做确定性归因（"想控制""故意"这类词不用）。

(6) 输出格式：每条结论一段，不分正向/反向小标题，最后单独一句"不确定性："收尾。`
	case "review":
		return `你是复检助理。对照新事实，检验此前某条分析结论是否仍成立：分别列出"被印证的部分"、"被削弱的部分"、"建议修正方向"，每条引用 event_id。`
	case "response_draft":
		return `你是"当场回复草拟助手"。用户会提供对方发来的一条消息（以及用户自己写的回复草稿）。你的任务是帮用户想"这次该怎么回"，输出严格分成两个物理分离的部分，用固定标题分隔，绝对不能合并成一段话。

## 当场回复候选
给出 2-3 个版本的回复，供用户直接选用（可自行修改后发送）。每个版本：
- 只做情绪确认 + 延后表态，例如"我听到你了，这个我先放一放，晚点我们好好说"
- 绝对禁止：给建议、讲道理、辩解、引用历史事件、提解决方案、教育对方
- 每版不超过两句
- 用途是当场发出去降温，不是解决问题

## 复盘参考
仅供用户本人参考，事后或当面沟通时用，绝不直接转发给对方。包含：
- 该类矛盾的历史样本情况：基于下方提供的"历史互动模式样本"，说明该类矛盾出现次数；样本不足 3 次要显式标注"样本不足，谨慎参考"
- 过去尝试过的应对方式及结果：列出历史 response_tried / outcome；若历史记录为空，明确提示"暂无结果记录，建议本次事后补记"
- 谈判方向：关于具体分歧点的可能方向，每条标注引用的 event_id，并声明"基于单方面数据推断，非对方真实立场"

硬性约束：
- 两个标题必须严格出现且顺序固定，Block A 内出现任何解决方案倾向（建议、说理、该怎么做）都算不合格输出
- Block A 和 Block B 在使用时机上强制分开：A 是当场发的，B 是事后/当面才用的
- Block B 不得替用户下"关系整体如何"的判断（那是别的模式的事），只针对这次分歧点
- 若未提供历史互动模式样本，Block B 仍要输出，但明确写"暂无该人物历史样本，本次为首次记录，建议事后补记结果"`
	case "daily_connect":
		return `你是"日常联结素材助手"。用户没有和对方发生冲突，只是想主动联系一下，但担心自己组织的语言带着情绪或说教。

你的任务是提供【原始素材】，绝对不是完整的话术、不是可以直接发送的句子。

## 可以提及的近况
从下方事实集合里，挑出最近的、性质=正面或中性的事件（如果有），列成关键词/短语，不成句。
例：孩子发烧、退烧、婆婆、班务 —— 只给名词短语，不写"我听说孩子退烧了真是太好了"这种完整句子。

## 近期敏感区（最好先避开）
从事实集合里，找出最近7天内性质=冲突、且严重度较高的事件涉及的话题关键词，列出来提醒用户"这几个话题这次先别碰"。
只给话题关键词，不给具体内容，不复述吵架细节。

硬性约束：
- 全文禁止出现完整主谓宾句子，只能是词、短语、要点列表
- 禁止使用"因为""其实""你可能""建议""应该"这类连接词或说教词
- 禁止生成任何可以原样复制发送的内容
- 如果近30天没有任何正面/中性事件记录，明确写"暂无近期正面互动记录，可参考行为模式样本或单纯表达陪伴"
- 如果没有历史互动模式样本，也如实写"暂无样本"`
	default:
		return `你是客观的事实整理助理。只做事实汇总，不下定性结论。每条结论标注对应 event_id。`
	}
}

// getEffectiveModePrompt 取生效的 mode 提示词：优先用 mode_prompts 表里用户自定义的，
// 没有记录则回退到 modeSystemPrompt（代码内置默认）。供 sessionMessagesHandler 拼 system msg 用。
func getEffectiveModePrompt(mode string) string {
	var custom string
	err := db.QueryRow("SELECT prompt_text FROM mode_prompts WHERE mode=?", mode).Scan(&custom)
	if err == nil && strings.TrimSpace(custom) != "" {
		return custom
	}
	return modeSystemPrompt(mode)
}

// ===== 事实提取专用 prompt（完全不做推断，只做"转录+结构化"，与分析prompt隔离）
const factExtractSystemPrompt = `你的任务是事实提取员。只做两件事：
1. 从用户输入/图片中提取可客观观察的事件；
2. 输出严格的结构化字段。

【核心原则：上下文优先】
- 同一张截图（尤其是聊天对话、连续事件），默认合并为一条事实，保留完整上下文和对话流程。
- 只有当截图里存在**明确不同的事件**（如不同时间点的两次独立争吵、或完全不同主题的场景）才拆成多条。
- 判断标准：如果拆开会让单条内容失去意义/看不懂，就应该合并。

硬性要求：
- 绝不做定性推断，绝不贴人格标签，绝不揣摩动机。看到"摔了碗"就写摔了碗，不要写"XX在生气"。
- 人物名只能写原文/图片里出现的称呼（如"我妈"、"小王"、"照片里的中年女性"），猜不出就留空。
- 时间：能从输入里读到（"昨天下午""2026-08-12 18:00"）就填；读不到就填当前时间，但要把 timestamp_uncertain=true。
- 严重度：不填。严重度是用户的主观自评，AI 没有资格代填。始终输出 0，由用户在审核面板里手动选择。
- valence（事件性质）：根据行为本身给出建议值，三选一：
    "conflict" = 冲突/对抗/负面情绪（争吵、指责、摔东西、冷战）
    "neutral"  = 中性客观事件（就医、吃药、日程安排，无明显情绪色彩）
    "positive" = 正面/积极互动（一起做事、关心、和解、轻松对话）
  注意：valence 是行为性质的客观分类，不是主观感受，AI 可以给建议值，但最终由用户确认。拿不准时填 "neutral"。
- 标签：从输入里抓几个关键词（逗号分隔），没有就留空。
- content 字段：对话类事实按"人物A：…；人物B：…"的格式串联完整对话，保留上下文顺序；非对话事件用简洁陈述句。
- 最终输出必须是纯 JSON，严格匹配如下格式（不要加任何前置解释、不要用代码块包裹也可以）：
{
  "facts": [
    {
      "timestamp": "YYYY-MM-DDTHH:MM:SS+08:00",
      "timestamp_uncertain": true,
      "people": "参与人物，逗号分隔",
      "tags": "关键词，逗号分隔",
      "severity": 0,
      "valence": "conflict",
      "content": "完整对话或事件描述，保留上下文"
    }
  ],
  "notes": "如果有无法归入的背景说明可以写在这里；没内容就写空字符串"
}

如果用户输入完全是在提问/闲聊、没有任何可提取的事实，返回 {"facts":[],"notes":""}。`

// factExtractOutput 是提取结果的 JSON 结构
type factExtractOutput struct {
	Facts []struct {
		Timestamp          string `json:"timestamp"`
		TimestampUncertain bool   `json:"timestamp_uncertain"`
		People             string `json:"people"`
		Tags               string `json:"tags"`
		Severity           int    `json:"severity"`
		Valence            string `json:"valence"`
		Content            string `json:"content"`
	} `json:"facts"`
	Notes string `json:"notes"`
}

// runFactExtract 调用指定 profile 对本轮用户输入做事实提取。
// profileID > 0 时用指定模型（事实提取专用），否则降级用第一个活跃 profile。
// 输入支持多模态（imageData=data URI 非空时会把图也发过去）。提取失败返回空 candidates + error；调用方可以决定是否忽略。
func runFactExtract(userText, imageData string, profileID int) ([]FactCandidate, error) {
	var p LLMProfile
	if profileID > 0 {
		pp, err := getProfileByID(profileID)
		if err != nil || pp == nil {
			// 指定的模型不存在，降级用第一个活跃
			profiles, _ := listActiveProfiles()
			if len(profiles) == 0 {
				return nil, fmt.Errorf("没有可用 profile 做事实提取")
			}
			p = profiles[0]
		} else {
			p = *pp
		}
	} else {
		profiles, err := listActiveProfiles()
		if err != nil || len(profiles) == 0 {
			if len(profiles) == 0 {
				return nil, fmt.Errorf("没有可用 profile 做事实提取")
			}
			return nil, err
		}
		p = profiles[0]
	}
	var userMsg APIMessage
	if imageData != "" {
		parts := []MultiModalContentPart{{Type: "text", Text: userText}}
		parts = append(parts, MultiModalContentPart{
			Type:     "image_url",
			ImageURL: map[string]string{"url": imageData},
		})
		userMsg = APIMessage{Role: "user", ContentParts: parts}
	} else {
		userMsg = APIMessage{Role: "user", Content: userText}
	}
	msgs := []APIMessage{
		{Role: "system", Content: factExtractSystemPrompt},
		userMsg,
	}
	var out factExtractOutput
	if err := callLLMForJSON(p, msgs, &out); err != nil {
		return nil, err
	}
	cands := make([]FactCandidate, 0, len(out.Facts))
	for _, f := range out.Facts {
		ts := f.Timestamp
		uncertain := f.TimestampUncertain
		if strings.TrimSpace(ts) == "" {
			ts = time.Now().Format(time.RFC3339)
			uncertain = true
		}
		// severity 不预填：AI 返回 0 或负值时设为 nil，前端显示默认值 1 但标记为"待用户选择"
		var sev *int
		if f.Severity > 0 {
			s := f.Severity
			if s > 5 {
				s = 5
			}
			sev = &s
		}
		// valence 归一化：只接受 conflict/neutral/positive，非法值置空
		valence := strings.ToLower(strings.TrimSpace(f.Valence))
		switch valence {
		case "conflict", "neutral", "positive":
		default:
			valence = ""
		}
		cands = append(cands, FactCandidate{
			SuggestedTimestamp: ts,
			People:             f.People,
			Tags:               f.Tags,
			Severity:           sev,
			Valence:            valence,
			Content:            strings.TrimSpace(f.Content),
			Status:             "pending",
			TimestampUncertain: uncertain,
		})
	}
	return cands, nil
}

// =====================================================
//  事件修正（审计式：先读旧值 → 写 corrections 留痕 → 再写 events）
// =====================================================
func correctEventHandler(c *gin.Context) {
	idStr := c.Param("id")
	var body struct {
		Field    string `json:"field" binding:"required"`
		NewValue string `json:"new_value"`
		Reason   string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	allowedFields := map[string]bool{"content": true, "people": true, "tags": true, "timestamp": true, "severity_self": true, "valence": true}
	if !allowedFields[body.Field] {
		c.JSON(400, gin.H{"error": "不允许修改的字段"})
		return
	}
	// 读旧值（统一用 *string；severity 数字也先按字符串扫出来，后续修正记录保持文本即可）
	var oldVal *string
	q := "SELECT CAST(" + body.Field + " AS TEXT) FROM events WHERE id = ?"
	err := db.QueryRow(q, idStr).Scan(&oldVal)
	if err != nil {
		c.JSON(404, gin.H{"error": "事件不存在"})
		return
	}
	oldValStr := ""
	if oldVal != nil {
		oldValStr = *oldVal
	}
	// 写 corrections 留痕
	_, err = db.Exec(
		"INSERT INTO corrections (event_id, field, old_value, new_value, reason) VALUES (?,?,?,?,?)",
		idStr, body.Field, oldValStr, body.NewValue, body.Reason,
	)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	// 更新 events
	_, err = db.Exec("UPDATE events SET "+body.Field+" = ? WHERE id = ?", body.NewValue, idStr)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func listCorrectionsHandler(c *gin.Context) {
	rows, err := db.Query(
		"SELECT id, event_id, field, old_value, new_value, reason, corrected_at FROM corrections WHERE event_id = ? ORDER BY corrected_at DESC",
		c.Param("id"),
	)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()
	var res []map[string]interface{}
	for rows.Next() {
		var id, eid int
		var field, reason, ts string
		var oldVal, newVal *string
		rows.Scan(&id, &eid, &field, &oldVal, &newVal, &reason, &ts)
		ov, nv := "", ""
		if oldVal != nil {
			ov = *oldVal
		}
		if newVal != nil {
			nv = *newVal
		}
		res = append(res, map[string]interface{}{
			"id": id, "event_id": eid, "field": field,
			"old_value": ov, "new_value": nv,
			"reason": reason, "corrected_at": ts,
		})
	}
	c.JSON(200, res)
}

// =====================================================
//  会话管理
// =====================================================
func createSessionHandler(c *gin.Context) {
	var s struct {
		Mode          string `json:"mode" binding:"required"`
		People        string `json:"people"`
		Tags          string `json:"tags"`
		ProfileIDs    []int  `json:"profile_ids"`     // 选哪些模型并发；空 = 全部活跃
		FactProfileID *int   `json:"fact_profile_id"` // 事实提取专用模型 ID；nil = 用第一个活跃模型
		FirstMsg      *string `json:"first_message"`
	}
	if err := c.ShouldBindJSON(&s); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if !validModes[s.Mode] {
		s.Mode = "pattern_query"
	}
	profJSON, _ := json.Marshal(s.ProfileIDs)
	now := time.Now().UTC().Format(time.RFC3339)
	var fid interface{}
	if s.FactProfileID != nil {
		fid = *s.FactProfileID
	}
	res, err := db.Exec(
		"INSERT INTO sessions (mode, filter_people, filter_tags, profile_ids, fact_profile_id, messages, created_at, updated_at) VALUES (?,?,?,?,?,?,?,?)",
		s.Mode, s.People, s.Tags, string(profJSON), fid, "[]", now, now,
	)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	id, _ := res.LastInsertId()
	c.JSON(201, gin.H{"id": id, "mode": s.Mode})
}

func listSessionsHandler(c *gin.Context) {
	rows, err := db.Query(
		"SELECT id, mode, filter_people, filter_tags, profile_ids, fact_profile_id, created_at, updated_at FROM sessions ORDER BY updated_at DESC LIMIT 100")
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()
	var res []map[string]interface{}
	for rows.Next() {
		var id int
		var factPID sql.NullInt64
		var mode, fp, ft, profs, createdAt, updatedAt *string
		rows.Scan(&id, &mode, &fp, &ft, &profs, &factPID, &createdAt, &updatedAt)
		row := map[string]interface{}{
			"id": id,
			"mode": sval(mode), "filter_people": sval(fp), "filter_tags": sval(ft),
			"profile_ids": parseJSONInts(sval(profs)),
			"created_at": sval(createdAt), "updated_at": sval(updatedAt),
		}
		if factPID.Valid {
			row["fact_profile_id"] = int(factPID.Int64)
		}
		res = append(res, row)
	}
	c.JSON(200, res)
}

func sval(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func parseJSONInts(s string) []int {
	if s == "" {
		return nil
	}
	var xs []int
	json.Unmarshal([]byte(s), &xs)
	return xs
}

func getSessionHandler(c *gin.Context) {
	var id int
	var factPID sql.NullInt64
	var mode, fp, ft, profs, messagesJSON, createdAt, updatedAt *string
	err := db.QueryRow(
		"SELECT id, mode, filter_people, filter_tags, profile_ids, fact_profile_id, messages, created_at, updated_at FROM sessions WHERE id = ?",
		c.Param("id"),
	).Scan(&id, &mode, &fp, &ft, &profs, &factPID, &messagesJSON, &createdAt, &updatedAt)
	if err != nil {
		c.JSON(404, gin.H{"error": "会话不存在"})
		return
	}
	var turns []Turn
	if messagesJSON != nil && *messagesJSON != "" {
		json.Unmarshal([]byte(*messagesJSON), &turns)
		for i := range turns {
			normalizeLegacyTurn(&turns[i])
		}
	}
	resp := gin.H{
		"id": id,
		"mode": sval(mode), "filter_people": sval(fp), "filter_tags": sval(ft),
		"profile_ids": parseJSONInts(sval(profs)),
		"turns":       turns,
		"created_at":  sval(createdAt), "updated_at": sval(updatedAt),
	}
	if factPID.Valid {
		resp["fact_profile_id"] = int(factPID.Int64)
	}
	c.JSON(200, resp)
}

// =====================================================
//  会话内发送消息：检索事实 → 按 profile 并发调模型 → 保存结果
// =====================================================
func sessionMessagesHandler(c *gin.Context) {
	idStr := c.Param("id")
	var payload struct {
		UserContent     string `json:"user_content" binding:"required"`
		HadImage        bool   `json:"had_image"`
		ImageData       string `json:"image_data"`          // data:image/jpeg;base64,... 前端传入
		SkipFactExtract bool   `json:"skip_fact_extract"`   // 前端开关控制是否跳过事实提取
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	payload.HadImage = payload.HadImage || payload.ImageData != ""

	// 读会话
	var sid int
	var factProfileID sql.NullInt64
	var mode, filterPeople, filterTags, profileIDsJSON, messagesJSON *string
	err := db.QueryRow(
		"SELECT id, mode, filter_people, filter_tags, profile_ids, fact_profile_id, messages FROM sessions WHERE id = ?",
		idStr,
	).Scan(&sid, &mode, &filterPeople, &filterTags, &profileIDsJSON, &factProfileID, &messagesJSON)
	if err != nil {
		c.JSON(404, gin.H{"error": "会话不存在"})
		return
	}
	modeV := sval(mode)
	fpV := sval(filterPeople)
	ftV := sval(filterTags)
	chosenProfiles := parseJSONInts(sval(profileIDsJSON))

	var turns []Turn
	if messagesJSON != nil && *messagesJSON != "" {
		json.Unmarshal([]byte(*messagesJSON), &turns)
		for i := range turns {
			normalizeLegacyTurn(&turns[i])
		}
	}

	// 1) 检索事实（按会话筛选 + 用户 query 做 FTS 检索取并集，最多 30 条）
	events := fetchContextEvents(fpV, ftV, payload.UserContent, 30)
	usedEventIDs := make([]int, 0, len(events))
	for _, e := range events {
		usedEventIDs = append(usedEventIDs, e.ID)
	}

	// 2) 拼 facts_block + 系统 prompt + 历史对话（仅 user / assistant 最近 6 轮，避免超长）
	factsBlock := buildFactsBlock(events)
	systemMsg := APIMessage{Role: "system",
		Content: getEffectiveModePrompt(modeV) + "\n\n以下是当前可参考的事实集合（按时间倒序）：\n" + factsBlock}
	// 采样偏差检测：若事实集合几乎全是高严重度冲突，强制拼元认知警示到 system prompt
	systemMsg.Content += samplingBiasNotice(events)
	// response_draft 模式：额外拼该人物的历史互动模式样本（供 Block B 复盘参考）
	if modeV == "response_draft" {
		systemMsg.Content += buildPatternsContext(fpV)
	}
	// daily_connect 模式：拼近30天正面/中性事件摘要 + 历史互动模式样本
	if modeV == "daily_connect" {
		systemMsg.Content += buildRecentPositiveContext(fpV)
		systemMsg.Content += buildPatternsContext(fpV)
	}

	historyMsgs := buildHistoryMessages(turns, 6, modeV)

	// 3) 选 profiles：会话指定了哪些用哪些，否则用全部活跃
	profiles := resolveProfiles(chosenProfiles)
	if len(profiles) == 0 {
		// 兜底：读环境变量
		envBase := getEnv("LLM_API_BASE", "")
		envKey := getEnv("LLM_API_KEY", "")
		envModel := getEnv("LLM_MODEL", "")
		if envBase != "" && envKey != "" && envModel != "" {
			profiles = []LLMProfile{{ID: 0, Name: "(默认)", APIBase: envBase, APIKey: envKey, Model: envModel, IsActive: 1}}
		}
	}
	if len(profiles) == 0 {
		c.JSON(400, gin.H{"error": "没有可用的 LLM 配置：请在「AI 分析-模型配置」里新增并激活，或在部署环境变量里填 LLM_API_BASE / LLM_API_KEY / LLM_MODEL"})
		return
	}

	// 4) 两条独立并发路径：
	//    A. 正常分析回复（按所选模型并行）
	//    B. 事实提取（只跑一次，使用第一个活跃profile）
	replies := make([]PerProfileReply, len(profiles))
	var candidates []FactCandidate
	var extractErrStr string
	var wg sync.WaitGroup

	// Path A
	for i, p := range profiles {
		wg.Add(1)
		go func(idx int, prof LLMProfile) {
			defer wg.Done()
			msgs := []APIMessage{systemMsg}
			msgs = append(msgs, historyMsgs...)
			// 这一轮 user message：有图片就走多模态结构发给分析模型（如果是视觉模型）
			if payload.ImageData != "" {
				parts := []MultiModalContentPart{{Type: "text", Text: payload.UserContent}}
				parts = append(parts, MultiModalContentPart{
					Type:     "image_url",
					ImageURL: map[string]string{"url": payload.ImageData},
				})
				msgs = append(msgs, APIMessage{Role: "user", ContentParts: parts})
			} else {
				msgs = append(msgs, APIMessage{Role: "user", Content: payload.UserContent})
			}
			reply, err := callLLMByProfile(prof, msgs)
			r := PerProfileReply{
				ProfileID: prof.ID, ProfileName: prof.Name, Model: prof.Model,
				Reply: reply, UsedEventIDs: usedEventIDs, PromptVersion: promptVersion,
			}
			if err != nil {
				r.Error = err.Error()
			}
			replies[idx] = r
		}(i, p)
	}

	// Path B：事实提取（可通过前端开关跳过）
	if !payload.SkipFactExtract {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cs, e := runFactExtract(payload.UserContent, payload.ImageData, int(factProfileID.Int64))
			if e != nil {
				extractErrStr = e.Error()
				return
			}
			candidates = cs
		}()
	}

	wg.Wait()

	// 5) 追加 turn、写 analyses、更新 sessions
	now := time.Now().UTC().Format(time.RFC3339)
	// 如果提取失败，把错误作为一条特殊 candidate 存进去（status=pending 带 ExtractError，前端会灰色提示但不影响正常写入）
	if extractErrStr != "" && len(candidates) == 0 {
		candidates = []FactCandidate{{
			Status:       "extract_error",
			ExtractError: extractErrStr,
			Content:      payload.UserContent, // 兜底让用户仍然可以手动编辑
		}}
	}
	turn := Turn{
		UserContent: payload.UserContent,
		HadImage:    payload.HadImage,
		ImageData:   payload.ImageData,
		Replies:     replies,
		Candidates:  candidates,
		CreatedAt:   now,
	}
	turns = append(turns, turn)
	newMessagesJSON, _ := json.Marshal(turns)

	// 5) 更新 sessions
	db.Exec(
		"UPDATE sessions SET messages = ?, updated_at = ? WHERE id = ?",
		string(newMessagesJSON), now, sid,
	)

	c.JSON(200, gin.H{"turn": turn})
}

// =====================================================
//  辅助函数
// =====================================================

// fetchContextEvents 同时做"按筛选条件全量 + FTS 相关性"检索，去重后限 topN。
func fetchContextEvents(filterPeople, filterTags, userQuery string, topN int) []EventRow {
	rowsMap := map[int]EventRow{}
	// 路径 A：按筛选条件扫最近 30 条
	qa := "SELECT id, timestamp, people, tags, severity_self, valence, content FROM events WHERE 1=1"
	var args []interface{}
	if filterPeople != "" {
		qa += " AND people LIKE ?"
		args = append(args, "%"+filterPeople+"%")
	}
	if filterTags != "" {
		qa += " AND tags LIKE ?"
		args = append(args, "%"+filterTags+"%")
	}
	qa += " ORDER BY datetime(timestamp) DESC LIMIT 30"
	rows, err := db.Query(qa, args...)
	if err == nil {
		for rows.Next() {
			var r EventRow
			rows.Scan(&r.ID, &r.Timestamp, &r.People, &r.Tags, &r.Severity, &r.Valence, &r.Content)
			rowsMap[r.ID] = r
		}
		rows.Close()
	}
	// 路径 B：FTS 按用户 query 检索相关
	if userQuery != "" {
		rows2, err := db.Query(
			"SELECT events.id, events.timestamp, events.people, events.tags, events.severity_self, events.valence, events.content "+
				"FROM events JOIN events_fts ON events.id = events_fts.rowid WHERE events_fts MATCH ? ORDER BY rank LIMIT 20", userQuery)
		if err == nil {
			for rows2.Next() {
				var r EventRow
				rows2.Scan(&r.ID, &r.Timestamp, &r.People, &r.Tags, &r.Severity, &r.Valence, &r.Content)
				rowsMap[r.ID] = r
			}
			rows2.Close()
		}
	}
	// 转 slice + 按时间倒序 + 截断
	out := make([]EventRow, 0, len(rowsMap))
	for _, r := range rowsMap {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Timestamp > out[j].Timestamp })
	if len(out) > topN {
		out = out[:topN]
	}
	return out
}

func valenceLabel(v string) string {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "conflict":
		return "冲突"
	case "neutral":
		return "中性"
	case "positive":
		return "正面"
	default:
		return "未分类"
	}
}

func buildFactsBlock(events []EventRow) string {
	if len(events) == 0 {
		return "（暂无事实记录）"
	}
	var sb strings.Builder
	for _, e := range events {
		fmt.Fprintf(&sb, "- #%d [%s] 人物=%s 标签=%s 严重度=%d 性质=%s\n  %s\n\n",
			e.ID, e.Timestamp,
			emptyIf(e.People, "(未填)"), emptyIf(e.Tags, "(未填)"),
			e.Severity, valenceLabel(e.Valence), strings.TrimSpace(e.Content))
	}
	return sb.String()
}

// samplingBiasNotice 检测事实集合是否存在采样偏差（几乎全是高严重度冲突，
// 缺少平和/中性/正面事件）。若存在，返回强制拼到 system prompt 的元认知警示。
// 注意：基于 severity 近似判定（severity_self 是主观自评，不能完全代表 valence），
// 只有"至少3条高严重度且没有任何1-2严重度事件"才触发，避免误报。
func samplingBiasNotice(events []EventRow) string {
	if len(events) < 3 {
		return ""
	}
	high, low := 0, 0
	for _, e := range events {
		switch {
		case e.Severity >= 3:
			high++
		case e.Severity >= 1:
			low++ // 1-2 算相对低/平和
		}
	}
	if high >= 3 && low == 0 {
		return fmt.Sprintf(
			"\n\n【采样局限提示：当前事实集合共 %d 条，其中 %d 条为高严重度(≥3)事件，"+
				"未包含任何严重度≤2 的相对平和互动。这是数据采集层的偏差——人通常只在冲突时才记录，"+
				"平和/正面瞬间很少被记下。任何关于「关系整体状态/是否值得维系」的推断必须声明这一局限，"+
				"不得仅凭冲突样本下整体性结论。】", len(events), high)
	}
	return ""
}

// buildPatternsContext 读 interaction_patterns 表，按 person 匹配历史互动模式样本，
// 拼成一段 context 供 response_draft 模式的 Block B（复盘参考）使用。
// 带 low_sample 标注（observation_count<3）。person 为空则返回空串。
func buildPatternsContext(person string) string {
	if strings.TrimSpace(person) == "" {
		return ""
	}
	rows, err := db.Query(
		"SELECT id, person, trigger_context, observed_pattern, ref_event_ids, response_tried, outcome, observation_count "+
			"FROM interaction_patterns WHERE person = ? ORDER BY datetime(updated_at) DESC LIMIT 10", person)
	if err != nil {
		return ""
	}
	defer rows.Close()
	var sb strings.Builder
	cnt := 0
	for rows.Next() {
		var id, obs int
		var person2, trigger, observed string
		var refP, respP, outP *string
		if err := rows.Scan(&id, &person2, &trigger, &observed, &refP, &respP, &outP, &obs); err != nil {
			continue
		}
		refs, responded, outcome := "", "", ""
		if refP != nil {
			refs = *refP
		}
		if respP != nil {
			responded = *respP
		}
		if outP != nil {
			outcome = *outP
		}
		cnt++
		if cnt == 1 {
			sb.WriteString("\n\n【该人物的历史互动模式样本（供「复盘参考」使用）】\n")
		}
		flag := ""
		if obs < 3 {
			flag = "  [样本不足，谨慎参考]"
		}
		fmt.Fprintf(&sb, "- trigger=%s  已记录 %d 次%s\n", trigger, obs, flag)
		if strings.TrimSpace(observed) != "" {
			fmt.Fprintf(&sb, "  观察到的模式: %s\n", observed)
		}
		if strings.TrimSpace(responded) != "" {
			fmt.Fprintf(&sb, "  过去尝试: %s\n", responded)
		}
		if strings.TrimSpace(outcome) != "" {
			fmt.Fprintf(&sb, "  结果: %s\n", outcome)
		}
		if strings.TrimSpace(refs) != "" {
			fmt.Fprintf(&sb, "  ref_event_ids: %s\n", refs)
		}
	}
	if cnt == 0 {
		return "\n\n【该人物的历史互动模式样本】暂无记录（本次为首次记录，建议事后补记 response_tried 与 outcome）"
	}
	return sb.String()
}

// buildRecentPositiveContext 取该人物最近30天 valence=positive/neutral 的事件，
// 供 daily_connect 模式"可以提及的近况"使用。只给标签+摘要片段，不给完整句子。
func buildRecentPositiveContext(person string) string {
	if strings.TrimSpace(person) == "" {
		return ""
	}
	rows, err := db.Query(
		`SELECT tags, content, timestamp FROM events
		 WHERE people LIKE ? AND valence IN ('positive','neutral')
		 AND datetime(timestamp) > datetime('now','-30 days')
		 ORDER BY datetime(timestamp) DESC LIMIT 15`,
		"%"+person+"%")
	if err != nil {
		return ""
	}
	defer rows.Close()
	var sb strings.Builder
	cnt := 0
	for rows.Next() {
		var tagsP, contentP *string
		var ts string
		if err := rows.Scan(&tagsP, &contentP, &ts); err != nil {
			continue
		}
		tags, content := "", ""
		if tagsP != nil {
			tags = *tagsP
		}
		if contentP != nil {
			content = *contentP
		}
		cnt++
		if cnt == 1 {
			sb.WriteString("\n\n【近30天正面/中性事件（原始摘要，不是话术）】\n")
		}
		fmt.Fprintf(&sb, "- [%s] 标签=%s 摘要=%s\n", ts, emptyIf(tags, "无"), truncate(content, 60))
	}
	if cnt == 0 {
		return "\n\n【近30天正面/中性事件】暂无记录"
	}
	return sb.String()
}

func emptyIf(s, def string) string {
	if strings.TrimSpace(s) == "" {
		return def
	}
	return s
}

// buildHistoryMessages 把最近 keepTurns 轮对话转成 OpenAI 历史 message 格式。
// 多 profile 时把回复拼成 "[模型名 A] \n...\n\n[模型名 B] \n..."，
// 避免一个 user 对应多条 assistant（很多 OpenAI 兼容实现对这种不鲁棒）。
func buildHistoryMessages(turns []Turn, keepTurns int, mode string) []APIMessage {
	n := len(turns)
	start := 0
	if n > keepTurns {
		start = n - keepTurns
	}
	out := []APIMessage{}
	for i := start; i < n; i++ {
		t := turns[i]
		out = append(out, APIMessage{Role: "user", Content: t.UserContent})
		// replies 拼到一个 assistant content
		var sb strings.Builder
		for j, r := range t.Replies {
			if r.Error != "" {
				continue
			}
			if j > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(fmt.Sprintf("[模型：%s (%s)]\n%s", r.ProfileName, r.Model, r.Reply))
		}
		if sb.Len() > 0 {
			out = append(out, APIMessage{Role: "assistant", Content: sb.String()})
		}
	}
	return out
}

// resolveProfiles 根据前端选择的 profile_ids 返回对应 LLMProfile；
// 空列表 = 全部活跃 profile。
func resolveProfiles(chosenIDs []int) []LLMProfile {
	all, err := listActiveProfiles()
	if err != nil || len(all) == 0 {
		return nil
	}
	if len(chosenIDs) == 0 {
		return all
	}
	idSet := map[int]struct{}{}
	for _, x := range chosenIDs {
		idSet[x] = struct{}{}
	}
	out := []LLMProfile{}
	for _, p := range all {
		if _, ok := idSet[p.ID]; ok {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		// 选中的 id 都不在活跃表里，降级返回全部活跃
		return all
	}
	return out
}

// =====================================================
//  候选事实确认/丢弃（用户审核面板调用）
// =====================================================
// confirmFactCandidateHandler: 把 turn 的某个 candidate 写入 events 表，并回写状态/ID
func confirmFactCandidateHandler(c *gin.Context) {
	sidStr := c.Param("id")
	turnIdxStr := c.Param("tidx")
	candIdxStr := c.Param("cidx")
	sid, err1 := strconv.Atoi(sidStr)
	turnIdx, err2 := strconv.Atoi(turnIdxStr)
	candIdx, err3 := strconv.Atoi(candIdxStr)
	if err1 != nil || err2 != nil || err3 != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}
	var body struct {
		Timestamp string  `json:"timestamp"`
		People    string  `json:"people"`
		Tags      string  `json:"tags"`
		Severity  *int    `json:"severity"`
		Valence   string  `json:"valence"`
		Content   string  `json:"content" binding:"required"`
		Status    *string `json:"status"` // "confirmed" / "dismissed"
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	status := "confirmed"
	if body.Status != nil && *body.Status == "dismissed" {
		status = "dismissed"
	}

	// 读会话和 turn
	var messagesJSON *string
	err := db.QueryRow("SELECT messages FROM sessions WHERE id = ?", sid).Scan(&messagesJSON)
	if err != nil {
		c.JSON(404, gin.H{"error": "会话不存在"})
		return
	}
	var turns []Turn
	if messagesJSON != nil && *messagesJSON != "" {
		json.Unmarshal([]byte(*messagesJSON), &turns)
	}
	if turnIdx < 0 || turnIdx >= len(turns) {
		c.JSON(404, gin.H{"error": "turn index out of range"})
		return
	}
	cs := turns[turnIdx].Candidates
	if candIdx < 0 || candIdx >= len(cs) {
		c.JSON(404, gin.H{"error": "candidate index out of range"})
		return
	}

	now := time.Now().UTC().Format(time.RFC3339)

	if status == "confirmed" {
		sev := 1
		if body.Severity != nil && *body.Severity >= 1 && *body.Severity <= 5 {
			sev = *body.Severity
		} else if cs[candIdx].Severity != nil {
			sev = *cs[candIdx].Severity
		}
		// valence：用户提交优先，否则用候选自带的 AI 建议，非法值置空
		valence := strings.ToLower(strings.TrimSpace(body.Valence))
		if valence == "" {
			valence = strings.ToLower(strings.TrimSpace(cs[candIdx].Valence))
		}
		switch valence {
		case "conflict", "neutral", "positive":
		default:
			valence = ""
		}
		ts := body.Timestamp
		if strings.TrimSpace(ts) == "" {
			ts = cs[candIdx].SuggestedTimestamp
		}
		if strings.TrimSpace(ts) == "" {
			ts = now
		}
		// 写入 events.status = "reviewed"（已审核写入），severity_self 存用户确认的值，valence 存事件性质
		res, e := db.Exec(
			`INSERT INTO events (timestamp, people, tags, severity_self, valence, content, status, created_at) VALUES (?,?,?,?,?,?,"reviewed",?)`,
			ts, body.People, body.Tags, sev, valence, body.Content, now,
		)
		if e != nil {
			c.JSON(500, gin.H{"error": e.Error()})
			return
		}
		lid, _ := res.LastInsertId()
		cs[candIdx].ID = int(lid)
		cs[candIdx].Status = "confirmed"
		cs[candIdx].SuggestedTimestamp = ts
		cs[candIdx].People = body.People
		cs[candIdx].Tags = body.Tags
		cs[candIdx].Severity = &sev
		cs[candIdx].Valence = valence
		cs[candIdx].Content = body.Content
	} else {
		cs[candIdx].Status = "dismissed"
	}
	turns[turnIdx].Candidates = cs

	newJSON, _ := json.Marshal(turns)
	db.Exec("UPDATE sessions SET messages = ?, updated_at = ? WHERE id = ?", string(newJSON), now, sid)
	c.JSON(200, gin.H{"ok": true, "candidate": cs[candIdx]})
}

// =====================================================
//  AI 识图记录：独立于会话，直接上传图片 → 提取事实 → 审核 → 入库
// =====================================================
// visionExtractHandler: 接收图片（可附文字），调用 runFactExtract 返回候选事实列表
func visionExtractHandler(c *gin.Context) {
	var payload struct {
		ImageData string `json:"image_data" binding:"required"`
		UserText  string `json:"user_text"` // 可选的文字补充描述
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	if len(payload.ImageData) > 5*1024*1024 {
		c.JSON(400, gin.H{"error": "图片需小于 5MB"})
		return
	}
	cands, err := runFactExtract(payload.UserText, payload.ImageData, 0)
	if err != nil {
		// 提取失败仍返回一条 draft，让用户可以手动编辑后入库
		cands = []FactCandidate{{
			Status:       "extract_error",
			ExtractError: err.Error(),
			Content:      "[AI 未能识别图片内容，请手动描述事件]",
			SuggestedTimestamp: time.Now().UTC().Format(time.RFC3339),
			TimestampUncertain: true,
		}}
	}
	c.JSON(200, gin.H{"candidates": cands})
}

// batchConfirmHandler: 批量确认候选事实 → 直接写入 events 表
// 请求体：{ "candidates": [ { "timestamp","people","tags","severity","valence","content" } ] }
func batchConfirmHandler(c *gin.Context) {
	var body struct {
		Candidates []struct {
			Timestamp string `json:"timestamp"`
			People    string `json:"people"`
			Tags      string `json:"tags"`
			Severity  *int   `json:"severity"`
			Valence   string `json:"valence"`
			Content   string `json:"content" binding:"required"`
		} `json:"candidates" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	var ids []int64
	for _, c2 := range body.Candidates {
		sev := 1
		if c2.Severity != nil && *c2.Severity >= 1 && *c2.Severity <= 5 {
			sev = *c2.Severity
		}
		// valence 归一化：只接受 conflict/neutral/positive，非法值置空
		valence := strings.ToLower(strings.TrimSpace(c2.Valence))
		switch valence {
		case "conflict", "neutral", "positive":
		default:
			valence = ""
		}
		ts := c2.Timestamp
		if strings.TrimSpace(ts) == "" {
			ts = now
		}
		res, err := db.Exec(
			`INSERT INTO events (timestamp, people, tags, severity_self, valence, content, status, created_at) VALUES (?,?,?,?,?,?,"reviewed",?)`,
			ts, c2.People, c2.Tags, sev, valence, c2.Content, now,
		)
		if err != nil {
			continue
		}
		lid, _ := res.LastInsertId()
		ids = append(ids, lid)
	}
	c.JSON(200, gin.H{"ok": true, "inserted_ids": ids})
}
