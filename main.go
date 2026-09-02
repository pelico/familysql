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
		severity_self INTEGER, valence TEXT, content TEXT NOT NULL, status TEXT DEFAULT 'raw', created_at TEXT DEFAULT CURRENT_TIMESTAMP
	);
	CREATE VIRTUAL TABLE IF NOT EXISTS events_fts USING fts5(content, tags, people, content=events, content_rowid=id, tokenize='trigram');
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
	CREATE TABLE IF NOT EXISTS mode_prompts (
		mode TEXT PRIMARY KEY, prompt_text TEXT NOT NULL,
		updated_at TEXT DEFAULT CURRENT_TIMESTAMP
	);
	CREATE TABLE IF NOT EXISTS people (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		created_at TEXT DEFAULT CURRENT_TIMESTAMP
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

	// ---- FTS 分词器迁移：从默认 unicode61 切换到 trigram（支持中文子串匹配）----
	// 检查现有 events_fts 的 tokenize 参数，如果不是 trigram 就重建
	var ftsTokenize string
	err = db.QueryRow("SELECT tokenize FROM events_fts_config").Scan(&ftsTokenize)
	if err == nil && ftsTokenize != "trigram" {
		// 旧表存在且不是 trigram：DROP → 重建 → 回填数据
		db.Exec("DROP TABLE events_fts")
		db.Exec(`CREATE VIRTUAL TABLE events_fts USING fts5(content, tags, people, content=events, content_rowid=id, tokenize='trigram')`)
		db.Exec(`INSERT INTO events_fts(rowid, content, tags, people) SELECT id, content, tags, people FROM events`)
	}

	// ---- 迁移：从 events.people 逗号分隔字段自动导入已有人物到 people 表 ----
	peopleRows, _ := db.Query("SELECT DISTINCT people FROM events WHERE people != ''")
	if peopleRows != nil {
		for peopleRows.Next() {
			var p string
			peopleRows.Scan(&p)
			for _, name := range splitAndTrim(p) {
				if name != "" {
					db.Exec("INSERT OR IGNORE INTO people(name) VALUES(?)", name)
				}
			}
		}
		peopleRows.Close()
	}

	// ---- 幂等列迁移（列已存在时 ALTER TABLE 会报错，静默忽略即可）----
	// analyses 表：溯源字段 — 这条分析是哪个模式、哪个 prompt 版本、属于哪个会话
	db.Exec(`ALTER TABLE analyses ADD COLUMN mode TEXT`)
	db.Exec(`ALTER TABLE analyses ADD COLUMN prompt_version TEXT`)
	db.Exec(`ALTER TABLE analyses ADD COLUMN session_id INTEGER`)
	// sessions 表：profile_ids JSON 数组（多模型并行时记录"这次会话选了哪几个模型"）
	db.Exec(`ALTER TABLE sessions ADD COLUMN profile_ids TEXT`)
	// sessions 表：fact_profile_id 指定事实提取专用模型（空=用第一个活跃模型）
	db.Exec(`ALTER TABLE sessions ADD COLUMN fact_profile_id INTEGER`)
	// events 表：valence 事件性质（conflict/neutral/positive），用于检测采样偏差
	db.Exec(`ALTER TABLE events ADD COLUMN valence TEXT`)
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
			Valence   string `json:"valence"`
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
			"INSERT INTO events (timestamp, people, tags, severity_self, valence, content, status) VALUES (?,?,?,?,?,?,?)",
			t.UTC().Format(time.RFC3339), e.People, e.Tags, e.Severity, e.Valence, e.Content, status,
		)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		// 自动把 people 字段里的人物写入 people 表
		for _, name := range splitAndTrim(e.People) {
			if name != "" {
				db.Exec("INSERT OR IGNORE INTO people(name) VALUES(?)", name)
			}
		}
		id, _ := res.LastInsertId()
		c.JSON(201, gin.H{"id": id})
	})

	r.GET("/api/events", func(c *gin.Context) {
		query := "SELECT id, timestamp, people, tags, severity_self, valence, content, status, created_at FROM events WHERE 1=1"
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
		if v := c.Query("valence"); v != "" {
			query += " AND valence = ?"
			args = append(args, v)
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
			var ts, createdAt string
			var pP, tP, valP, conP, statP *string
			if err := rows.Scan(&id, &ts, &pP, &tP, &sev, &valP, &conP, &statP, &createdAt); err != nil {
				continue
			}
			pp, tp, val, con, st := "", "", "", "", ""
			if pP != nil { pp = *pP }
			if tP != nil { tp = *tP }
			if valP != nil { val = *valP }
			if conP != nil { con = *conP }
			if statP != nil { st = *statP }
			res = append(res, map[string]interface{}{
				"id": id, "timestamp": ts, "people": pp, "tags": tp,
				"severity_self": sev, "valence": val, "content": con, "status": st, "created_at": createdAt,
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
			"SELECT events.id, events.timestamp, events.people, events.tags, events.severity_self, events.valence, events.content, events.status "+
				"FROM events JOIN events_fts ON events.id = events_fts.rowid WHERE events_fts MATCH ? ORDER BY rank LIMIT 200", q)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var res []map[string]interface{}
		for rows.Next() {
			var id, sev int
			var ts string
			var pP, tP, valP, conP, statP *string
			if err := rows.Scan(&id, &ts, &pP, &tP, &sev, &valP, &conP, &statP); err != nil {
				continue
			}
			pp, tp, val, con, st := "", "", "", "", ""
			if pP != nil { pp = *pP }
			if tP != nil { tp = *tP }
			if valP != nil { val = *valP }
			if conP != nil { con = *conP }
			if statP != nil { st = *statP }
			res = append(res, map[string]interface{}{
				"id": id, "timestamp": ts, "people": pp, "tags": tp,
				"severity_self": sev, "valence": val, "content": con, "status": st,
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

	// ---- 人物管理 CRUD ----
	r.GET("/api/people", func(c *gin.Context) {
		rows, err := db.Query("SELECT id, name FROM people ORDER BY name COLLATE NOCASE")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		type Person struct {
			ID   int    `json:"id"`
			Name string `json:"name"`
		}
		people := []Person{}
		for rows.Next() {
			var p Person
			rows.Scan(&p.ID, &p.Name)
			people = append(people, p)
		}
		c.JSON(200, gin.H{"people": people})
	})
	r.POST("/api/people", func(c *gin.Context) {
		var body struct {
			Name string `json:"name" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "name is required"})
			return
		}
		name := strings.TrimSpace(body.Name)
		if name == "" {
			c.JSON(400, gin.H{"error": "name cannot be empty"})
			return
		}
		res, err := db.Exec("INSERT OR IGNORE INTO people(name) VALUES(?)", name)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		// 查回 id（无论是新建还是已存在的）
		var id int
		db.QueryRow("SELECT id FROM people WHERE name=?", name).Scan(&id)
		affected, _ := res.RowsAffected()
		c.JSON(200, gin.H{"id": id, "name": name, "created": affected > 0})
	})
	r.DELETE("/api/people/:id", func(c *gin.Context) {
		idStr := c.Param("id")
		_, err := db.Exec("DELETE FROM people WHERE id=?", idStr)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"deleted": true})
	})

	// 人物重命名 / 合并：
	// - 把旧名字 old_name 在所有表中出现的地方替换成 new_name
	// - 如果 people 表里已经有 new_name（合并场景），删除旧记录；否则把原记录 UPDATE 名字
	// - events.people 是逗号分隔字符串，用 token 级精确替换（避免 "you" 错换掉 "youyou"）
	// - interaction_patterns.person 是整字段匹配
	// - sessions.filter_people 同理整字段匹配
	r.POST("/api/people/:id/rename", func(c *gin.Context) {
		idStr := c.Param("id")
		var body struct {
			NewName string `json:"new_name" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": "new_name is required"})
			return
		}
		newName := strings.TrimSpace(body.NewName)
		if newName == "" {
			c.JSON(400, gin.H{"error": "new_name cannot be empty"})
			return
		}

		// 1. 拿旧名字
		var oldName string
		err := db.QueryRow("SELECT name FROM people WHERE id=?", idStr).Scan(&oldName)
		if err != nil {
			c.JSON(404, gin.H{"error": "person not found"})
			return
		}
		if oldName == newName {
			c.JSON(200, gin.H{"skipped": true, "reason": "same name"})
			return
		}

		tx, err := db.Begin()
		if err != nil { c.JSON(500, gin.H{"error":err.Error()}); return }
		defer func() { _ = tx.Rollback() }()

		// 2. 替换 events.people —— 逗号分隔，按 token 精确匹配
		//    用 SQLite 无法直接做 token 级替换，所以把需要处理的行读出来，Go 里 split 后替换再写回去
		type evRow struct {
			id     int
			people string
		}
		evRows := []evRow{}
		{
			like := "%"+oldName+"%"
			rows, qerr := tx.Query("SELECT id, people FROM events WHERE people LIKE ? AND people IS NOT NULL", like)
			if qerr != nil { c.JSON(500, gin.H{"error": qerr.Error()}); return }
			for rows.Next() {
				var r evRow
				var pp *string
				rows.Scan(&r.id, &pp)
				if pp != nil { r.people = *pp }
				evRows = append(evRows, r)
			}
			rows.Close()
		}
		for _, r := range evRows {
			tokens := splitAndTrim(r.people)
			changed := false
			for i, t := range tokens {
				if t == oldName {
					tokens[i] = newName
					changed = true
				}
			}
			if changed {
				// 去重（合并时可能出现同名重复 token）
				seen := map[string]struct{}{}
				uniq := make([]string, 0, len(tokens))
				for _, t := range tokens {
					if _, ok := seen[t]; !ok {
						seen[t] = struct{}{}
						uniq = append(uniq, t)
					}
				}
				newPeople := strings.Join(uniq, ", ")
				tx.Exec("UPDATE events SET people=? WHERE id=?", newPeople, r.id)
			}
		}

		// 3. 替换 interaction_patterns.person（整字段匹配）
		{
			_, uerr := tx.Exec("UPDATE interaction_patterns SET person=? WHERE person=?", newName, oldName)
			if uerr != nil { c.JSON(500, gin.H{"error": uerr.Error()}); return }
		}

		// 4. 替换 sessions.filter_people（整字段匹配）
		{
			_, uerr := tx.Exec("UPDATE sessions SET filter_people=? WHERE filter_people=?", newName, oldName)
			if uerr != nil { c.JSON(500, gin.H{"error": uerr.Error()}); return }
		}

		// 5. 处理 people 表本身
		var newID int
		existsErr := tx.QueryRow("SELECT id FROM people WHERE name=?", newName).Scan(&newID)
		if existsErr == nil && newID > 0 {
			// people 表里已存在同名记录（合并场景）→ 删除旧 id
			tx.Exec("DELETE FROM people WHERE id=?", idStr)
		} else {
			// 没同名 → 直接 UPDATE 原记录 name
			tx.Exec("UPDATE people SET name=? WHERE id=?", newName, idStr)
		}

		if cerr := tx.Commit(); cerr != nil {
			c.JSON(500, gin.H{"error": cerr.Error()})
			return
		}
		c.JSON(200, gin.H{
			"ok":            true,
			"old_name":      oldName,
			"new_name":      newName,
			"events_updated": len(evRows),
			"merged":        existsErr == nil && newID > 0,
		})
	})

	// 事件单条查询
	r.GET("/api/events/:id", func(c *gin.Context) {
		var id, sev int
		var ts, createdAt string
		var pP, tP, valP, conP, statP *string
		err := db.QueryRow(
			"SELECT id, timestamp, people, tags, severity_self, valence, content, status, created_at FROM events WHERE id=?", c.Param("id"),
		).Scan(&id, &ts, &pP, &tP, &sev, &valP, &conP, &statP, &createdAt)
		if err != nil {
			c.JSON(404, gin.H{"error": "事件不存在"})
			return
		}
		pp, tp, val, con, st := "", "", "", "", ""
		if pP != nil { pp = *pP }
		if tP != nil { tp = *tP }
		if valP != nil { val = *valP }
		if conP != nil { con = *conP }
		if statP != nil { st = *statP }
		c.JSON(200, gin.H{
			"id": id, "timestamp": ts, "people": pp, "tags": tp,
			"severity_self": sev, "valence": val, "content": con, "status": st, "created_at": createdAt,
		})
	})

	// 修正事件：审计式（先读旧值，写 corrections 留痕，再写 events）
	r.POST("/api/events/:id/correct", correctEventHandler)
	r.GET("/api/events/:id/corrections", listCorrectionsHandler)

	// AI 识图记录：独立接口，不经过会话，直接图片 → 提取 → 审核 → 入库
	r.POST("/api/events/vision-extract", visionExtractHandler)
	r.POST("/api/events/batch-confirm", batchConfirmHandler)

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
	r.POST("/api/sessions/:id/turns/:tidx/candidates/:cidx/confirm", confirmFactCandidateHandler)
	r.DELETE("/api/sessions/:id", func(c *gin.Context) {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil { c.JSON(400, gin.H{"error": "invalid id"}); return }
		db.Exec("DELETE FROM session_turns WHERE session_id = ?", id)
		db.Exec("DELETE FROM analyses WHERE session_id = ?", id)
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

	// interaction_patterns upsert：按 person+trigger_context 匹配，已存在则 observation_count+1
	// 并追加 response_tried/outcome（用 " | " 分隔，保留历次记录），不存在则新建。
	// 供 response_draft 模式事后回填"发出去的回复+对方实际反应"用。
	r.POST("/api/interaction_patterns/upsert", func(c *gin.Context) {
		var body struct {
			Person        string `json:"person" binding:"required"`
			Trigger       string `json:"trigger_context" binding:"required"`
			Observed      string `json:"observed_pattern"`
			RefEventIDs   string `json:"ref_event_ids"`
			ResponseTried string `json:"response_tried"`
			Outcome       string `json:"outcome"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		// 查 person+trigger_context 是否已有记录
		var existingID, obs int
		var existingObs, existingRefs, existingResp, existingOut string
		err := db.QueryRow(
			"SELECT id, observed_pattern, ref_event_ids, response_tried, outcome, observation_count "+
				"FROM interaction_patterns WHERE person=? AND trigger_context=? ORDER BY updated_at DESC LIMIT 1",
			body.Person, body.Trigger,
		).Scan(&existingID, &existingObs, &existingRefs, &existingResp, &existingOut, &obs)
		if err == sql.ErrNoRows {
			// 新建
			res, e := db.Exec(
				`INSERT INTO interaction_patterns (person, trigger_context, observed_pattern, ref_event_ids, response_tried, outcome, observation_count, created_at, updated_at) VALUES (?,?,?,?,?,?,?,1,?,?)`,
				body.Person, body.Trigger, nullStr(body.Observed), nullStr(body.RefEventIDs), nullStr(body.ResponseTried), nullStr(body.Outcome), now, now,
			)
			if e != nil {
				c.JSON(500, gin.H{"error": e.Error()})
				return
			}
			id, _ := res.LastInsertId()
			c.JSON(201, gin.H{"id": id, "created": true, "observation_count": 1})
			return
		}
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		// 已存在：observation_count+1，response_tried/outcome/ref_event_ids 追加，observed_pattern 有新值则覆盖
		newObs := existingObs
		if strings.TrimSpace(body.Observed) != "" {
			newObs = body.Observed
		}
		newResp := existingResp
		if strings.TrimSpace(body.ResponseTried) != "" {
			if existingResp != "" {
				newResp = existingResp + " | " + body.ResponseTried
			} else {
				newResp = body.ResponseTried
			}
		}
		newOut := existingOut
		if strings.TrimSpace(body.Outcome) != "" {
			if existingOut != "" {
				newOut = existingOut + " | " + body.Outcome
			} else {
				newOut = body.Outcome
			}
		}
		newRefs := existingRefs
		if strings.TrimSpace(body.RefEventIDs) != "" {
			if existingRefs != "" {
				newRefs = existingRefs + "," + body.RefEventIDs
			} else {
				newRefs = body.RefEventIDs
			}
		}
		_, e := db.Exec(
			"UPDATE interaction_patterns SET observed_pattern=?, ref_event_ids=?, response_tried=?, outcome=?, observation_count=?, updated_at=? WHERE id=?",
			nullStr(newObs), nullStr(newRefs), nullStr(newResp), nullStr(newOut), obs+1, now, existingID,
		)
		if e != nil {
			c.JSON(500, gin.H{"error": e.Error()})
			return
		}
		c.JSON(200, gin.H{"id": existingID, "created": false, "observation_count": obs + 1})
	})

	// =====================================================
	// mode 提示词管理：允许在设置里覆盖代码内置的默认 prompt，改完不用重新部署
	// =====================================================
	r.GET("/api/mode-prompts", func(c *gin.Context) {
		modes := []string{"pattern_query", "contradiction_check", "hypothesis_only", "review", "response_draft", "daily_connect"}
		var res []map[string]interface{}
		for _, m := range modes {
			var custom, updatedAt string
			hasCustom := false
			err := db.QueryRow("SELECT prompt_text, updated_at FROM mode_prompts WHERE mode=?", m).Scan(&custom, &updatedAt)
			if err == nil {
				hasCustom = true
			}
			prompt := custom
			if !hasCustom || strings.TrimSpace(custom) == "" {
				prompt = modeSystemPrompt(m)
			}
			res = append(res, map[string]interface{}{
				"mode": m, "prompt": prompt, "is_default": !hasCustom, "updated_at": updatedAt,
			})
		}
		c.JSON(200, res)
	})
	r.PUT("/api/mode-prompts/:mode", func(c *gin.Context) {
		mode := c.Param("mode")
		if !validModes[mode] {
			c.JSON(400, gin.H{"error": "未知 mode"})
			return
		}
		var body struct {
			PromptText string `json:"prompt_text" binding:"required"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(400, gin.H{"error": err.Error()})
			return
		}
		now := time.Now().UTC().Format(time.RFC3339)
		_, err := db.Exec("INSERT OR REPLACE INTO mode_prompts (mode, prompt_text, updated_at) VALUES (?,?,?)",
			mode, body.PromptText, now)
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		c.JSON(200, gin.H{"ok": true, "updated_at": now})
	})
	r.DELETE("/api/mode-prompts/:mode", func(c *gin.Context) {
		mode := c.Param("mode")
		if !validModes[mode] {
			c.JSON(400, gin.H{"error": "未知 mode"})
			return
		}
		db.Exec("DELETE FROM mode_prompts WHERE mode=?", mode)
		c.JSON(200, gin.H{"ok": true, "restored_default": true})
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
