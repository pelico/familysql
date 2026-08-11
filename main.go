package main

import (
	"database/sql"
	"os"
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
			if t := strings.TrimSpace(cur); t != "" { res = append(res, t) }
			cur = ""
		} else {
			cur += string(r)
		}
	}
	if t := strings.TrimSpace(cur); t != "" { res = append(res, t) }
	return res
}

func initDB() {
	os.MkdirAll("./data", 0755)
	var err error
	db, err = sql.Open("sqlite3", "./data/database.db?_foreign_keys=on")
	if err != nil { panic(err) }

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
}

func main() {
	initDB()
	r := gin.Default()

	r.POST("/api/events", func(c *gin.Context) {
		var e struct { Timestamp string `json:"timestamp" binding:"required"`; People, Tags, Content string; Severity int `json:"severity_self"` }
		if err := c.ShouldBindJSON(&e); err != nil { c.JSON(400, gin.H{"error": err.Error()}); return }
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil { c.JSON(400, gin.H{"error": "Must be RFC3339/ISO8601 with offset"}); return }
		res, err := db.Exec("INSERT INTO events (timestamp, people, tags, severity_self, content) VALUES (?,?,?,?,?)", 
			t.UTC().Format(time.RFC3339), e.People, e.Tags, e.Severity, e.Content)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		id, _ := res.LastInsertId()
		c.JSON(201, gin.H{"id": id})
	})

	r.GET("/api/events", func(c *gin.Context) {
		query := "SELECT id, timestamp, people, tags, severity_self, content, status FROM events WHERE 1=1"
		var args []interface{}
		if t := c.Query("tags"); t != "" { query += " AND tags LIKE ?"; args = append(args, "%"+t+"%") }
		if p := c.Query("people"); p != "" { query += " AND people LIKE ?"; args = append(args, "%"+p+"%") }
		if sMin := c.Query("severity_min"); sMin != "" { query += " AND severity_self >= ?"; args = append(args, sMin) }
		if sMax := c.Query("severity_max"); sMax != "" { query += " AND severity_self <= ?"; args = append(args, sMax) }
		
		rows, err := db.Query(query, args...)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var res []map[string]interface{}
		for rows.Next() {
			var id, sev int; var ts, p, t, con, stat string
			rows.Scan(&id, &ts, &p, &t, &sev, &con, &stat)
			res = append(res, map[string]interface{}{"id": id, "timestamp": ts, "people": p, "tags": t, "severity_self": sev, "content": con, "status": stat})
		}
		c.JSON(200, res)
	})

	r.GET("/api/metadata", func(c *gin.Context) {
		// 聚合已存在的人物与标签（按逗号分隔的字符串拆分去重）
		peopleSet := map[string]struct{}{}
		tagsSet := map[string]struct{}{}
		rows, err := db.Query("SELECT people, tags FROM events WHERE people != '' OR tags != ''")
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		for rows.Next() {
			var p, t string
			rows.Scan(&p, &t)
			for _, s := range splitAndTrim(p) { peopleSet[s] = struct{}{} }
			for _, s := range splitAndTrim(t) { tagsSet[s] = struct{}{} }
		}
		people := make([]string, 0, len(peopleSet))
		for k := range peopleSet { people = append(people, k) }
		tags := make([]string, 0, len(tagsSet))
		for k := range tagsSet { tags = append(tags, k) }
		c.JSON(200, gin.H{"people": people, "tags": tags})
	})

	r.GET("/api/events/search", func(c *gin.Context) {
		q := c.Query("q")
		rows, err := db.Query("SELECT events.id, events.timestamp, events.people, events.tags, events.severity_self, events.content, events.status FROM events JOIN events_fts ON events.id = events_fts.rowid WHERE events_fts MATCH ? ORDER BY rank", q)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var res []map[string]interface{}
		for rows.Next() {
			var id, sev int; var ts, p, t, con, stat string
			rows.Scan(&id, &ts, &p, &t, &sev, &con, &stat)
			res = append(res, map[string]interface{}{"id": id, "timestamp": ts, "people": p, "tags": t, "severity_self": sev, "content": con, "status": stat})
		}
		c.JSON(200, res)
	})

	r.POST("/api/event_links", func(c *gin.Context) {
		var l struct { EID int `json:"event_id"`; LEID int `json:"linked_event_id"`; Rel string `json:"relation"` }
		if err := c.ShouldBindJSON(&l); err != nil { c.JSON(400, gin.H{"error": "Invalid data"}); return }
		_, err := db.Exec("INSERT INTO event_links (event_id, linked_event_id, relation) VALUES (?,?,?)", l.EID, l.LEID, l.Rel)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		c.JSON(201, gin.H{"success": true})
	})

	r.POST("/api/analyses", func(c *gin.Context) {
		var a struct { EIDs string `json:"based_on_event_ids"`; Agent string `json:"agent_used"`; Output string `json:"output"` }
		if err := c.ShouldBindJSON(&a); err != nil || a.Output == "" { c.JSON(400, gin.H{"error": "Invalid data"}); return }
		_, err := db.Exec("INSERT INTO analyses (based_on_event_ids, agent_used, output) VALUES (?,?,?)", a.EIDs, a.Agent, a.Output)
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		c.JSON(201, gin.H{"success": true})
	})

	r.GET("/api/analyses", func(c *gin.Context) {
		rows, err := db.Query("SELECT id, based_on_event_ids, agent_used, output FROM analyses ORDER BY created_at DESC")
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		defer rows.Close()
		var res []map[string]interface{}
		for rows.Next() {
			var id int; var ids, ag, out string
			rows.Scan(&id, &ids, &ag, &out)
			res = append(res, map[string]interface{}{"id": id, "based_on_event_ids": ids, "agent_used": ag, "output": out})
		}
		c.JSON(200, res)
	})

	r.PATCH("/api/events/:id/status", func(c *gin.Context) {
		var body map[string]interface{}
		if err := c.ShouldBindJSON(&body); err != nil { c.JSON(400, gin.H{"error": "Invalid JSON"}); return }
		if _, ok := body["content"]; ok || body["people"] != nil || body["tags"] != nil || body["severity_self"] != nil {
			c.JSON(400, gin.H{"error": "Forbidden modification"})
			return
		}
		if _, ok := body["status"]; !ok { c.JSON(400, gin.H{"error": "Status required"}); return }
		res, err := db.Exec("UPDATE events SET status = ? WHERE id = ?", body["status"], c.Param("id"))
		if err != nil { c.JSON(500, gin.H{"error": err.Error()}); return }
		rows, _ := res.RowsAffected()
		if rows == 0 { c.JSON(404, gin.H{"error": "Not found"}); return }
		c.JSON(200, gin.H{"success": true})
	})

	r.Run(":18080")
}
