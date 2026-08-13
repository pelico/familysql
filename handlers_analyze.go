package main

import (
	"encoding/json"
	"fmt"
	"sort"
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
	Content   string
}

// FactCandidate 是某一轮对话中被提取出来、等待人工确认的候选事实。
type FactCandidate struct {
	SuggestedTimestamp string `json:"suggested_timestamp"`
	People             string `json:"people"`
	Tags               string `json:"tags"`
	Content            string `json:"content"`
	Status             string `json:"status"`
	EventID            *int   `json:"event_id,omitempty"`
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
type Turn struct {
	UserContent    string            `json:"user_content"`
	HadImage       bool              `json:"had_image"`
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
		return `你是假设构建助理。允许提出综合推断，但所有结论必须：(1) 用假设句式；(2) 标注置信度（低/中/高）；(3) 给出正反两种解释；(4) 引用 event_id。禁止人格定性。`
	case "review":
		return `你是复检助理。对照新事实，检验此前某条分析结论是否仍成立：分别列出"被印证的部分"、"被削弱的部分"、"建议修正方向"，每条引用 event_id。`
	default:
		return `你是客观的事实整理助理。只做事实汇总，不下定性结论。每条结论标注对应 event_id。`
	}
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
	allowedFields := map[string]bool{"content": true, "people": true, "tags": true, "timestamp": true, "severity_self": true}
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
		Mode       string  `json:"mode" binding:"required"`
		People     string  `json:"people"`
		Tags       string  `json:"tags"`
		ProfileIDs []int   `json:"profile_ids"` // 选哪些模型并发；空 = 全部活跃
		FirstMsg   *string `json:"first_message"`
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
	res, err := db.Exec(
		"INSERT INTO sessions (mode, filter_people, filter_tags, profile_ids, messages, created_at, updated_at) VALUES (?,?,?,?, '[]', ?, ?)",
		s.Mode, s.People, s.Tags, string(profJSON), now, now,
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
		"SELECT id, mode, filter_people, filter_tags, profile_ids, created_at, updated_at FROM sessions ORDER BY updated_at DESC LIMIT 100")
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()
	var res []map[string]interface{}
	for rows.Next() {
		var id int
		var mode, fp, ft, profs, createdAt, updatedAt *string
		rows.Scan(&id, &mode, &fp, &ft, &profs, &createdAt, &updatedAt)
		res = append(res, map[string]interface{}{
			"id": id,
			"mode": sval(mode), "filter_people": sval(fp), "filter_tags": sval(ft),
			"profile_ids": parseJSONInts(sval(profs)),
			"created_at": sval(createdAt), "updated_at": sval(updatedAt),
		})
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
	var mode, fp, ft, profs, messagesJSON, createdAt, updatedAt *string
	err := db.QueryRow(
		"SELECT id, mode, filter_people, filter_tags, profile_ids, messages, created_at, updated_at FROM sessions WHERE id = ?",
		c.Param("id"),
	).Scan(&id, &mode, &fp, &ft, &profs, &messagesJSON, &createdAt, &updatedAt)
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
	c.JSON(200, gin.H{
		"id": id,
		"mode": sval(mode), "filter_people": sval(fp), "filter_tags": sval(ft),
		"profile_ids": parseJSONInts(sval(profs)),
		"turns":       turns,
		"created_at":  sval(createdAt), "updated_at": sval(updatedAt),
	})
}

// =====================================================
//  会话内发送消息：检索事实 → 按 profile 并发调模型 → 保存结果
// =====================================================
func sessionMessagesHandler(c *gin.Context) {
	idStr := c.Param("id")
	var payload struct {
		UserContent string `json:"user_content" binding:"required"`
		HadImage    bool   `json:"had_image"`
	}
	if err := c.ShouldBindJSON(&payload); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	// 读会话
	var sid int
	var mode, filterPeople, filterTags, profileIDsJSON, messagesJSON *string
	err := db.QueryRow(
		"SELECT id, mode, filter_people, filter_tags, profile_ids, messages FROM sessions WHERE id = ?",
		idStr,
	).Scan(&sid, &mode, &filterPeople, &filterTags, &profileIDsJSON, &messagesJSON)
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
		Content: modeSystemPrompt(modeV) + "\n\n以下是当前可参考的事实集合（按时间倒序）：\n" + factsBlock}

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

	// 4) 每个 profile 并发调用
	replies := make([]PerProfileReply, len(profiles))
	var wg sync.WaitGroup
	for i, p := range profiles {
		wg.Add(1)
		go func(idx int, prof LLMProfile) {
			defer wg.Done()
			msgs := []APIMessage{systemMsg}
			msgs = append(msgs, historyMsgs...)
			msgs = append(msgs, APIMessage{Role: "user", Content: payload.UserContent})
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
	wg.Wait()

	// 5) 追加 turn、写 analyses、更新 sessions
	now := time.Now().UTC().Format(time.RFC3339)
	turn := Turn{
		UserContent: payload.UserContent,
		HadImage:    payload.HadImage,
		Replies:     replies,
		CreatedAt:   now,
	}
	turns = append(turns, turn)
	newMessagesJSON, _ := json.Marshal(turns)

	// 5.1 每个成功的 reply 各存一条 analyses（方便校准仪表盘逐条复检）
	for _, r := range replies {
		if r.Reply == "" {
			continue
		}
		agent := fmt.Sprintf("session#%d profile#%d(%s)", sid, r.ProfileID, r.ProfileName)
		idsStr := intJoin(r.UsedEventIDs, ",")
		db.Exec(
			"INSERT INTO analyses (based_on_event_ids, agent_used, output, mode, prompt_version, session_id) VALUES (?,?,?,?,?,?)",
			idsStr, agent, r.Reply, modeV, promptVersion, sid,
		)
	}

	// 5.2 更新 sessions
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
	qa := "SELECT id, timestamp, people, tags, severity_self, content FROM events WHERE 1=1"
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
			rows.Scan(&r.ID, &r.Timestamp, &r.People, &r.Tags, &r.Severity, &r.Content)
			rowsMap[r.ID] = r
		}
		rows.Close()
	}
	// 路径 B：FTS 按用户 query 检索相关
	if userQuery != "" {
		rows2, err := db.Query(
			"SELECT events.id, events.timestamp, events.people, events.tags, events.severity_self, events.content "+
				"FROM events JOIN events_fts ON events.id = events_fts.rowid WHERE events_fts MATCH ? ORDER BY rank LIMIT 20", userQuery)
		if err == nil {
			for rows2.Next() {
				var r EventRow
				rows2.Scan(&r.ID, &r.Timestamp, &r.People, &r.Tags, &r.Severity, &r.Content)
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

func buildFactsBlock(events []EventRow) string {
	if len(events) == 0 {
		return "（暂无事实记录）"
	}
	var sb strings.Builder
	for _, e := range events {
		fmt.Fprintf(&sb, "- #%d [%s] 人物=%s 标签=%s 严重度=%d\n  %s\n\n",
			e.ID, e.Timestamp,
			emptyIf(e.People, "(未填)"), emptyIf(e.Tags, "(未填)"),
			e.Severity, strings.TrimSpace(e.Content))
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
