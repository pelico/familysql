package main

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	_ "github.com/mattn/go-sqlite3"
)

var db *sql.DB

// splitAndTrim 按逗号拆分字符串并去除空白，跳过空项
func splitAndTrim(s string) []string {
	var res []string
	cur := ""
	for _, r := range s {
		if r == ',' {
			if t := strings.TrimSpace(cur); t != "" {
				res = append(res, t)
			}
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if t := strings.TrimSpace(cur); t != "" {
		res = append(res, t)
	}
	return res
}

func initDB() {
	os.MkdirAll("./data", 0755)
	var err error
	db, err = sql.Open("sqlite3", "./data/database.db?_foreign_keys=on&_journal_mode=WAL")
	if err != nil {
		panic(err)
	}
	// W: SQLite 用 write-ahead log 配合 .backup 在线导出最稳妥
	schema := `
	CREATE TABLE IF NOT EXISTS events (
		id INTEGER PRIMARY KEY AUTOINCREMENT, timestamp TEXT NOT NULL, people TEXT, tags TEXT, 
		severity_self INTEGER, content TEXT NOT NULL, status TEXT DEFAULT 'raw', created_at TEXT DEFAULT CURRENT_TIMESTAMP
	);
	CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(content, tags, people, content=events, content_rowid=id);
	CREATE TABLE IF NOT EXISTS event_links (
		id INTEGER PRIMARY KEY AUTOINCREMENT, event_id INTEGER, linked_event_id INTEGER, relation TEXT,
		FOREIGN KEY(event_id) REFERENCES events(id) ON DELETE CASCADE, FOREIGN KEY(linked_event_id) REFERENCES events(id) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS analyses (
		id INTEGER PRIMARY KEY AUTOINCREMENT, created_at TEXT DEFAULT CURRENT_TIMESTAMP, 
		based_on_event_ids TEXT, agent_used TEXT, output TEXT NOT NULL
	);
	CREATE TABLE IF NOT EXISTS corrections (
		id INTEGER PRIMARY KEY AUTOINCREMENT, event_id INTEGER NOT NULL, field TEXT NOT NULL,
		old_value TEXT, new_value TEXT, reason TEXT NOT NULL, corrected_at TEXT DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(event_id) REFERENCES events(id)
	);
	CREATE TABLE IF NOT EXISTS sessions (
		id INTEGER PRIMARY KEY AUTOINCREMENT, mode TEXT NOT NULL,
		filter_people TEXT, filter_tags TEXT, messages TEXT NOT NULL DEFAULT '[]',
		created_at TEXT DEFAULT CURRENT_TIMESTAMP, updated_at TEXT DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS interaction_patterns (
		id INTEGER PRIMARY KEY AUTOINCREMENT, person TEXT NOT NULL, trigger_context TEXT NOT NULL,
		observed_pattern TEXT NOT NULL, ref_event_ids TEXT, response_tried TEXT, outcome TEXT,
		observation_count INTEGER DEFAULT 1,
		created_at TEXT DEFAULT CURRENT_TIMESTAMP, updated_at TEXT DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS llm_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT NOT NULL UNIQUE,
		api_base TEXT NOT NULL, api_key TEXT NOT NULL, model TEXT NOT NULL,
		is_active INTEGER DEFAULT 0,
		created_at TEXT DEFAULT CURRENT_TIMESTAMP, updated_at TEXT DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TRIGGER IF NOT EXISTS events_ai AFTER INSERT ON events BEGIN
		INSERT INTO events_fts(rowid, content, tags, people) VALUES (new.id, new.content, new.tags, new.people);
	END;
	CREATE TRIGGER IF NOT EXISTS events_ad AFTER DELETE ON events BEGIN
		INSERT INTO events_fts(events_fts, rowid, content, tags, people) VALUES('delete', old.id, old.content, old.tags, old.people);
	END;
	CREATE TRIGGER IF NOT EXISTS events_au AFTER UPDATE ON events BEGIN
		INSERT INTO events_fts(events_fts, rowid, content, tags, people) VALUES('delete', old.id, old.content, old.tags, old.people);
		INSERT INTO events_fts(rowid, content, tags, people) VALUES (new.id, new.content, new.tags, new.people);
	END;`
	db.Exec(schema)

	// ---- 幂等列迁移（列已存在时 ALTER TABLE 会报错，静默忽略即可）----
	// analyses 表：溯源字段 — 这条分析是哪个模式、哪个 prompt 版本、属于哪个会话
	db.Exec(`ALTER TABLE analyses ADD COLUMN mode TEXT`)
	db.Exec(`ALTER TABLE analyses ADD COLUMN prompt_version TEXT`)
	db.Exec(`ALTER TABLE analyses ADD COLUMN session_id INTEGER`)
	// sessions 表：profile_ids JSON 数组（多模型并行时记录"这次会话选了哪几个模型"）
	db.Exec(`ALTER TABLE sessions ADD COLUMN profile_ids TEXT`)
}

func main() {
	initDB()
	r := gin.Default()
	// 静态资源：public/index.html + 其他文件由 Gin 托管
	r.StaticFS("/public", http.Dir("./public"))
	r.StaticFile("/", "./public/index.html")

	// =====================================================
	//  事件记录
	// =====================================================
	r.POST("/api/events", func(c *gin.Context) {
		var e struct {
			Timestamp string `json:"timestamp" binding:"required"`
			People    string `json:"people"`
			Tags      string `json:"tags"`
			Content   string `json:"content" binding:"required"`
			Severity  int    `json:"severity_self"`
			Status    string `json:"status"`
		}
		if err := c.ShouldBindJSON(&e); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil {
			c.JSON(400, gin.H{"error": "Must be RFC3339/ISO8601 with offset, e.g. 2026-01-02T03:04:05+08:00"})
			return
		}
		status := e.Status
		if status == "" {
			status = "raw"
		}
		res, err := db.Exec(
			"INSERT INTO events (timestamp, people, tags, severity_self, content, status) VALUES (?,?,?,?,?,?)",
			t.UTC().Format(time.RFC3339), e.People, e.Tags, e.Severity, e.Content, status,
		)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		id, _ := res.LastInsertId()
		c.JSON(201, gin.H{"id": id})
	})

	r.GET("/api/events", func(c *gin.Context) {
		query := "SELECT id, timestamp, people, tags, severity_self, content, status, created_at FROM events WHERE 1=1"
		var args []interface{}
		if t := c.Query("tags"); t != "" {
			query += " AND tags LIKE ?"
			args = append(args, "%"+t+"%")
		}
		if p := c.Query("people"); p != "" {
			query += " AND people LIKE ?"
			args = append(args, "%"+p+"%")
		}
		if sMin := c.Query("severity_min"); sMin != "" {
			query += " AND severity_self >= ?"
			args = append(args, sMin)
		}
		if sMax := c.Query("severity_max"); sMax != "" {
			query += " AND severity_self <= ?"
			args = append(args, sMax)
		}
		query += " ORDER BY datetime(timestamp) DESC"
		rows, err := db.Query(query, args...)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var res []map[string]interface{}
		for rows.Next() {
			var id, sev int
			var ts, p, t, con, stat, createdAt string
			rows.Scan(&id, &ts, &p, &t, &sev, &con, &stat, &createdAt)
			res = append(res, map[string]interface{}{
				"id": id, "timestamp": ts, "people": p, "tags": t,
				"severity_self": sev, "content": con, "status": stat, "created_at": createdAt,
			})
		}
		c.JSON(200, res)
	})

	r.GET("/api/events/search", func(c *gin.Context) {
		q := c.Query("q")
		if q == "" {
			c.JSON(200, []interface{}{})
			return
		}
		rows, err := db.Query(
			"SELECT events.id, events.timestamp, events.people, events.tags, events.severity_self, events.content, events.status "+
				"FROM events JOIN events_fts ON events.id = events_fts.rowid WHERE events_fts MATCH ? ORDER BY rank LIMIT 200", q)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var res []map[string]interface{}
		for rows.Next() {
			var id, sev int
			var ts, p, t, con, stat string
			rows.Scan(&id, &ts, &p, &t, &sev, &con, &stat)
			res = append(res, map[string]interface{}{
				"id": id, "timestamp": ts, "people": p, "tags": t,
				"severity_self": sev, "content": con, "status": stat,
			})
		}
		c.JSON(200, res)
	})

	// 元数据：人物+标签去重（前端下拉/快记补全用）
	r.GET("/api/metadata", func(c *gin.Context) {
		peopleSet := map[string]struct{}{}
		tagsSet := map[string]struct{}{}
		rows, err := db.Query("SELECT people, tags FROM events WHERE people != '' OR tags != ''")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		for rows.Next() {
			var p, t string
			rows.Scan(&p, &t)
			for _, s := range splitAndTrim(p) {
				peopleSet[s] = struct{}{}
			}
			for _, s := range splitAndTrim(t) {
				tagsSet[s] = struct{}{}
			}
		}
		people := make([]string, 0, len(peopleSet))
		for k := range peopleSet {
			people = append(people, k)
		}
		tags := make([]string, 0, len(tagsSet))
		for k := range tagsSet {
			tags = append(tags, k)
		}
		c.JSON(200, gin.H{"people": people, "tags": tags})
	})

	// 事件单条查询
	r.GET("/api/events/:id", func(c *gin.Context) {
		var id, sev int
		var ts, p, t, con, stat, createdAt string
		err := db.QueryRow(
			"SELECT id, timestamp, people, tags, severity_self, content, status, created_at FROM events WHERE id=?", c.Param("id"),
		).Scan(&id, &ts, &p, &t, &sev, &con, &stat, &createdAt)
		if err != nil {
			c.JSON(404, gin.H{"error": "事件不存在"})
			return
		}
		c.JSON(200, gin.H{
			"id": id, "timestamp": ts, "people": p, "tags": t,
			"severity_self": sev, "content": con, "status": stat, "created_at": createdAt,
		})
	})

	// 修正事件：审计式（先读旧值，写 corrections 留痕，再写 events）
	r.POST("/api/events/:id/correct", correctEventHandler)
	r.GET("/api/events/:id/corrections", listCorrectionsHandler)

	r.PATCH("/api/events/:id/status", func(c *gin.Context) {
		var body map[string]interface{}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "Invalid JSON"})
			return
		}
		if _, ok := body["content"]; ok || body["people"] != nil || body["tags"] != nil || body["severity_self"] != nil {
			c.JSON(400, gin.H{"error": "Forbidden modification: content/people/tags/severity_self must go through /correct endpoint"})
			return
		}
		if _, ok := body["status"]; !ok {
			c.JSON(400, gin.H{"error": "Status required"})
			return
		}
		res, err := db.Exec("UPDATE events SET status = ? WHERE id = ?", body["status"], c.Param("id"))
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		rows, _ := res.RowsAffected()
		if rows == 0 {
			c.JSON(404, gin.H{"error": "Not found"})
			return
		}
		c.JSON(200, gin.H{"success": true})
	})

	r.POST("/api/event_links", func(c *gin.Context) {
		var l struct {
			EID  int    `json:"event_id"`
			LEID int    `json:"linked_event_id"`
			Rel  string `json:"relation"`
		}
		if err := c.ShouldBindJSON(&l); err != nil || l.EID == 0 || l.LEID == 0 {
			c.JSON(400, gin.H{"error": "Invalid data"})
			return
		}
		_, err := db.Exec("INSERT INTO event_links (event_id, linked_event_id, relation) VALUES (?,?,?)", l.EID, l.LEID, l.Rel)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"success": true})
	})

	// =====================================================
	//  历史分析（简单 CRUD + 校准仪表盘 API）
	// =====================================================
	r.POST("/api/analyses", func(c *gin.Context) {
		var a struct {
			EIDs         string `json:"based_on_event_ids"`
			Agent        string `json:"agent_used"`
			Output       string `json:"output"`
			Mode         string `json:"mode"`
			PromptVersion string `json:"prompt_version"`
			SessionID    *int   `json:"session_id"`
		}
		if err := c.ShouldBindJSON(&a); err != nil || a.Output == "" {
			c.JSON(400, gin.H{"error": "Invalid data"})
			return
		}
		sidVal := interface{}(nil)
		if a.SessionID != nil {
			sidVal = *a.SessionID
		}
		_, err := db.Exec(
			"INSERT INTO analyses (based_on_event_ids, agent_used, output, mode, prompt_version, session_id) VALUES (?,?,?,?,?,?)",
			a.EIDs, a.Agent, a.Output, a.Mode, a.PromptVersion, sidVal,
		)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(201, gin.H{"success": true})
	})

	r.GET("/api/analyses", func(c *gin.Context) {
		rows, err := db.Query(
			"SELECT id, based_on_event_ids, agent_used, output, mode, prompt_version, session_id, created_at FROM analyses ORDER BY created_at DESC")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var res []map[string]interface{}
		for rows.Next() {
			var id int
			var ids, ag, out, mode, pv, createdAt string
			var sid *int
			rows.Scan(&id, &ids, &ag, &out, &mode, &pv, &sid, &createdAt)
			res = append(res, map[string]interface{}{
				"id": id, "based_on_event_ids": ids, "agent_used": ag, "output": out,
				"mode": mode, "prompt_version": pv, "session_id": sid, "created_at": createdAt,
			})
		}
		c.JSON(200, res)
	})

	// 校准仪表盘：时间线（每条分析之后新增了多少事实）+ 聚合统计
	r.GET("/api/analyses/timeline", func(c *gin.Context) {
		rows, err := db.Query(
			"SELECT id, based_on_event_ids, agent_used, output, mode, prompt_version, session_id, created_at FROM analyses ORDER BY created_at DESC")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var res []map[string]interface{}
		for rows.Next() {
			var id int
			var ids, ag, out, mode, pv, createdAt string
			var sid *int
			rows.Scan(&id, &ids, &ag, &out, &mode, &pv, &sid, &createdAt)
			var newEventCount int
			db.QueryRow("SELECT COUNT(*) FROM events WHERE created_at > ?", createdAt).Scan(&newEventCount)
			res = append(res, map[string]interface{}{
				"id": id, "based_on_event_ids": ids, "agent_used": ag, "output": out,
				"mode": mode, "prompt_version": pv, "session_id": sid, "created_at": createdAt,
				"events_since": newEventCount,
			})
		}
		c.JSON(200, res)
	})

	r.GET("/api/analyses/stats", func(c *gin.Context) {
		var total int
		db.QueryRow("SELECT COUNT(*) FROM analyses").Scan(&total)

		modeRows, _ := db.Query("SELECT COALESCE(mode,'(未标)') as m, COUNT(*) as c FROM analyses GROUP BY m ORDER BY c DESC")
		modes := []map[string]interface{}{}
		if modeRows != nil {
			for modeRows.Next() {
				var m string
				var cc int
				modeRows.Scan(&m, &cc)
				modes = append(modes, map[string]interface{}{"mode": m, "count": cc})
			}
			modeRows.Close()
		}
		agentRows, _ := db.Query("SELECT COALESCE(agent_used,'(未标)') as a, COUNT(*) as c FROM analyses GROUP BY a ORDER BY c DESC LIMIT 20")
		agents := []map[string]interface{}{}
		if agentRows != nil {
			for agentRows.Next() {
				var a string
				var cc int
				agentRows.Scan(&a, &cc)
				agents = append(agents, map[string]interface{}{"agent": a, "count": cc})
			}
			agentRows.Close()
		}
		var earliest, latest string
		db.QueryRow("SELECT COALESCE(MIN(created_at),'') FROM analyses").Scan(&earliest)
		db.QueryRow("SELECT COALESCE(MAX(created_at),'') FROM analyses").Scan(&latest)
		c.JSON(200, gin.H{
			"total": total, "modes": modes, "agents": agents,
			"earliest": earliest, "latest": latest,
		})
	})

	r.DELETE("/api/analyses/:id", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil { c.JSON(400, gin.H{"error": "invalid id"}); return }
		_, err = db.Exec("DELETE FROM analyses WHERE id = ?", id)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		c.JSON(200, gin.H{"ok": true})
	})

	// =====================================================
	//  LLM 配置管理 + 状态查询
	// =====================================================
	r.GET("/api/llm_status", llmStatusHandler)
	r.GET("/api/llm_profiles", listProfilesHandler)
	r.POST("/api/llm_profiles", createProfileHandler)
	r.PATCH("/api/llm_profiles/:id", updateProfileHandler)
	r.DELETE("/api/llm_profiles/:id", deleteProfileHandler)

	// =====================================================
	//  会话式 AI 分析（多轮上下文 + 多模型并行回复）
	// =====================================================
	r.POST("/api/sessions", createSessionHandler)
	r.GET("/api/sessions", listSessionsHandler)
	r.GET("/api/sessions/:id", getSessionHandler)
	r.POST("/api/sessions/:id/messages", sessionMessagesHandler)
	// 候选事实确认/丢弃：把用户在审核面板里改完的 candidate 写入 events 或丢掉
	r.POST("/api/sessions/:sid/turns/:tidx/candidates/:cidx/confirm", confirmFactCandidateHandler)
	r.DELETE("/api/sessions/:id", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil { c.JSON(400, gin.H{"error": "invalid id"}); return }
		db.Exec("DELETE FROM session_turns WHERE session_id = ?", id)
		_, err = db.Exec("DELETE FROM sessions WHERE id = ?", id)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		c.JSON(200, gin.H{"ok": true})
	})

	// =====================================================
	//  情境-反应对照库（interaction_patterns）
	// =====================================================
	r.GET("/api/interaction_patterns", func(c *gin.Context) {
		query := "SELECT id, person, trigger_context, observed_pattern, ref_event_ids, response_tried, outcome, observation_count, created_at, updated_at FROM interaction_patterns WHERE 1=1"
		var args []interface{}
		if p := c.Query("person"); p != "" {
			query += " AND person = ?"
			args = append(args, p)
		}
		query += " ORDER BY datetime(updated_at) DESC"
		rows, err := db.Query(query, args...)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var res []map[string]interface{}
		for rows.Next() {
			var id, obs int
			var person, trigger, observed, refs, responded, outcome, createdAt, updatedAt string
			var refP, respP, outP *string
			rows.Scan(&id, &person, &trigger, &observed, &refP, &respP, &outP, &obs, &createdAt, &updatedAt)
			if refP != nil {
				refs = *refP
			}
			if respP != nil {
				responded = *respP
			}
			if outP != nil {
				outcome = *outP
			}
			res = append(res, map[string]interface{}{
				"id": id, "person": person, "trigger_context": trigger,
				"observed_pattern": observed, "ref_event_ids": refs,
				"response_tried": responded, "outcome": outcome,
				"observation_count": obs, "low_sample": obs < 3,
				"created_at": createdAt, "updated_at": updatedAt,
			})
		}
		c.JSON(200, res)
	})

	r.POST("/api/interaction_patterns", func(c *gin.Context) {
		var body struct {
			Person        string `json:"person" binding:"required"`
			Trigger       string `json:"trigger_context" binding:"required"`
			Observed      string `json:"observed_pattern" binding:"required"`
			RefEventIDs   string `json:"ref_event_ids"`
			ResponseTried string `json:"response_tried"`
			Outcome       string `json:"outcome"`
			ObsCount      int    `json:"observation_count"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		if body.ObsCount <= 0 {
			body.ObsCount = 1
		}
		now := time.Now().UTC().Format(time.RFC3339)
		res, err := db.Exec(
			`INSERT INTO interaction_patterns
			(person, trigger_context, observed_pattern, ref_event_ids, response_tried, outcome, observation_count, created_at, updated_at)
			VALUES (?,?,?,?,?,?,?,?,?)`,
			body.Person, body.Trigger, body.Observed,
			nullStr(body.RefEventIDs), nullStr(body.ResponseTried), nullStr(body.Outcome),
			body.ObsCount, now, now,
		)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		id, _ := res.LastInsertId()
		c.JSON(201, gin.H{"id": id})
	})

	_ = json.Marshal // 防止未使用 import 告警（后续 handlers_analyze.go 里要用，但这里也 import 了）
	_ = strconv.Itoa // 同上
	r.Run(":18080")
}

// nullStr 空串转 NULL，避免在 nullable TEXT 列里存空字符串
func nullStr(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}
