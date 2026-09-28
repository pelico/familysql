package main

// 扩展能力模块：访问口令、图片文件存储、数据导出、在线备份、统计、回复回填追踪。
// 设计原则：
//   - 全部通过 registerExtrasRoutes(r) 注册，main.go 只调一行，方便后续继续扩展
//   - authMiddleware 是全局中间件，只拦截 /api/*（静态页无需口令），新加的路由自动受保护
//   - 图片存 data/images/（DB 只存路径引用），兼容旧数据里内嵌的 base64

import (
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
)

const (
	imagesDir  = "./data/images"
	backupsDir = "./data/backups"
)

// appToken 访问口令：环境变量 APP_TOKEN，空 = 不启用（保持单机无感）
var appToken = getEnv("APP_TOKEN", "")

// ensureDataDirs 建目录并收紧权限（隐私数据目录不给其他人读）
func ensureDataDirs() {
	os.MkdirAll("./data", 0700)
	os.MkdirAll(imagesDir, 0700)
	os.MkdirAll(backupsDir, 0700)
	os.Chmod("./data", 0700)
	os.Chmod(imagesDir, 0700)
	os.Chmod(backupsDir, 0700)
}

// =====================================================
//  访问口令（可选）：APP_TOKEN 非空时，所有 /api/* 需要 Bearer token
// =====================================================
func authMiddleware(c *gin.Context) {
	if appToken == "" {
		c.Next()
		return
	}
	p := c.Request.URL.Path
	if !strings.HasPrefix(p, "/api/") || p == "/api/auth/verify" {
		c.Next()
		return
	}
	tok := c.GetHeader("Authorization")
	if strings.HasPrefix(tok, "Bearer ") {
		tok = strings.TrimSpace(strings.TrimPrefix(tok, "Bearer "))
	}
	if tok == "" {
		tok = c.GetHeader("X-Auth-Token")
	}
	if tok == "" {
		tok = c.Query("token")
	}
	if tok != "" && tok == appToken {
		c.Next()
		return
	}
	c.AbortWithStatusJSON(401, gin.H{"error": "unauthorized", "auth": true})
}

// POST /api/auth/verify —— 前端登录校验 + 探测口令是否启用
func authVerifyHandler(c *gin.Context) {
	if appToken == "" {
		c.JSON(200, gin.H{"enabled": false, "ok": true})
		return
	}
	var body struct {
		Token string `json:"token"`
	}
	c.ShouldBindJSON(&body)
	if body.Token == appToken {
		c.JSON(200, gin.H{"enabled": true, "ok": true})
		return
	}
	c.JSON(401, gin.H{"enabled": true, "ok": false, "error": "访问口令错误"})
}

// =====================================================
//  图片存储：base64 → data/images/ 文件，DB/会话只存路径引用
// =====================================================
func mimeByExt(ext string) string {
	switch strings.ToLower(ext) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".webp":
		return "image/webp"
	case ".gif":
		return "image/gif"
	default:
		return "application/octet-stream"
	}
}

func extByMime(mime string) string {
	switch mime {
	case "image/png":
		return "png"
	case "image/jpeg":
		return "jpg"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	default:
		return "bin"
	}
}

func randHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// safeImageName 只允许文件名白名单字符，防路径穿越
func safeImageName(name string) bool {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") || strings.Contains(name, "..") {
		return false
	}
	for _, r := range name {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}

// POST /api/images —— 接收 data URI，落盘，返回可访问路径
func uploadImageHandler(c *gin.Context) {
	var body struct {
		ImageData string `json:"image_data" binding:"required"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	// 已经是指向了已有文件的路径（幂等：前端重复调用时直接透传）
	if strings.HasPrefix(body.ImageData, "/api/images/") {
		c.JSON(200, gin.H{"path": body.ImageData})
		return
	}
	if !strings.HasPrefix(body.ImageData, "data:") {
		c.JSON(400, gin.H{"error": "image_data 必须是 data URI 或 /api/images/ 路径"})
		return
	}
	comma := strings.Index(body.ImageData, ",")
	if comma < 0 {
		c.JSON(400, gin.H{"error": "data URI 格式错误"})
		return
	}
	meta := body.ImageData[:comma] // data:image/png;base64
	mime := strings.TrimPrefix(meta, "data:")
	if semi := strings.Index(mime, ";"); semi >= 0 {
		mime = mime[:semi]
	}
	raw, err := base64.StdEncoding.DecodeString(body.ImageData[comma+1:])
	if err != nil {
		c.JSON(400, gin.H{"error": "base64 解码失败"})
		return
	}
	if len(raw) > 8*1024*1024 {
		c.JSON(400, gin.H{"error": "图片需小于 8MB"})
		return
	}
	name := fmt.Sprintf("%d-%s.%s", time.Now().UnixNano(), randHex(6), extByMime(mime))
	path := filepath.Join(imagesDir, name)
	if err := os.WriteFile(path, raw, 0600); err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(201, gin.H{"path": "/api/images/" + name})
}

// GET /api/images/:name —— 读图片文件（受访问口令保护，因为截图是隐私）
func serveImageHandler(c *gin.Context) {
	name := c.Param("name")
	if !safeImageName(name) {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	path := filepath.Join(imagesDir, name)
	if _, err := os.Stat(path); err != nil {
		c.JSON(404, gin.H{"error": "not found"})
		return
	}
	c.File(path)
}

// resolveImageData 把 /api/images/ 路径引用还原成 data URI（给 LLM 用）。
// 已是 data URI 或空串则原样返回。失败时返回原值，让调用方按原样继续。
func resolveImageData(img string) (string, error) {
	if img == "" || !strings.HasPrefix(img, "/api/images/") {
		return img, nil
	}
	name := strings.TrimPrefix(img, "/api/images/")
	if !safeImageName(name) {
		return img, fmt.Errorf("非法图片引用: %q", img)
	}
	path := filepath.Join(imagesDir, name)
	raw, err := os.ReadFile(path)
	if err != nil {
		return img, err
	}
	mime := mimeByExt(filepath.Ext(name))
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(raw), nil
}

// =====================================================
//  数据导出（JSON 全量 / CSV 事件）
// =====================================================
func exportHandler(c *gin.Context) {
	format := c.DefaultQuery("format", "json")
	now := time.Now().Format("20060102-150405")
	if format == "csv" {
		rows, err := db.Query("SELECT id, timestamp, people, tags, severity_self, valence, content, status, created_at FROM events ORDER BY id")
		if err != nil {
			c.JSON(500, gin.H{"error": err.Error()})
			return
		}
		defer rows.Close()
		var buf strings.Builder
		w := csv.NewWriter(&buf)
		w.Write([]string{"id", "timestamp", "people", "tags", "severity_self", "valence", "content", "status", "created_at"})
		for rows.Next() {
			var id, sev int
			var ts, createdAt string
			var pP, tP, valP, conP, statP *string
			rows.Scan(&id, &ts, &pP, &tP, &sev, &valP, &conP, &statP, &createdAt)
			pp, tp, val, con, st := "", "", "", "", ""
			if pP != nil { pp = *pP }
			if tP != nil { tp = *tP }
			if valP != nil { val = *valP }
			if conP != nil { con = *conP }
			if statP != nil { st = *statP }
			w.Write([]string{fmt.Sprint(id), ts, pp, tp, fmt.Sprint(sev), val, con, st, createdAt})
		}
		w.Flush()
		c.Header("Content-Disposition", "attachment; filename=events-"+now+".csv")
		c.Data(200, "text/csv; charset=utf-8", []byte(buf.String()))
		return
	}
	// JSON 全量导出
	type dump struct {
		ExportedAt          string                   `json:"exported_at"`
		Events              []map[string]interface{} `json:"events"`
		Corrections         []map[string]interface{} `json:"corrections"`
		Analyses            []map[string]interface{} `json:"analyses"`
		Sessions            []map[string]interface{} `json:"sessions"`
		InteractionPatterns []map[string]interface{} `json:"interaction_patterns"`
		ModePrompts         []map[string]interface{} `json:"mode_prompts"`
		People              []map[string]interface{} `json:"people"`
	}
	d := dump{ExportedAt: time.Now().UTC().Format(time.RFC3339)}
	loadAll := func(q string, cols []string) []map[string]interface{} {
		rows, err := db.Query(q)
		if err != nil {
			return nil
		}
		defer rows.Close()
		colNames, _ := rows.Columns()
		_ = cols
		var out []map[string]interface{}
		for rows.Next() {
			vals := make([]interface{}, len(colNames))
			ptrs := make([]interface{}, len(colNames))
			for i := range vals {
				ptrs[i] = &vals[i]
			}
			if rows.Scan(ptrs...) == nil {
				m := map[string]interface{}{}
				for i, n := range colNames {
					switch v := vals[i].(type) {
					case nil:
						m[n] = nil
					case []byte:
						m[n] = string(v)
					default:
						m[n] = v
					}
				}
				out = append(out, m)
			}
		}
		return out
	}
	d.Events = loadAll("SELECT * FROM events ORDER BY id", nil)
	d.Corrections = loadAll("SELECT * FROM corrections ORDER BY id", nil)
	d.Analyses = loadAll("SELECT * FROM analyses ORDER BY id", nil)
	d.Sessions = loadAll("SELECT * FROM sessions ORDER BY id", nil)
	d.InteractionPatterns = loadAll("SELECT * FROM interaction_patterns ORDER BY id", nil)
	d.ModePrompts = loadAll("SELECT * FROM mode_prompts", nil)
	d.People = loadAll("SELECT * FROM people ORDER BY id", nil)
	c.Header("Content-Disposition", "attachment; filename=familysql-export-"+now+".json")
	c.JSON(200, d)
}

// =====================================================
//  在线备份：VACUUM INTO 生成一致性快照，保留最近 14 份
// =====================================================
func backupNow() (string, int64, error) {
	os.MkdirAll(backupsDir, 0700)
	dst := fmt.Sprintf("%s/backup-%s.db", backupsDir, time.Now().Format("20060102-150405"))
	if _, err := db.Exec("VACUUM INTO '" + dst + "'"); err != nil {
		return "", 0, err
	}
	st, _ := os.Stat(dst)
	var size int64
	if st != nil {
		size = st.Size()
	}
	// 只保留最近 14 份，防止无限堆积
	entries, _ := filepath.Glob(backupsDir + "/backup-*.db")
	sort.Strings(entries)
	for len(entries) > 14 {
		os.Remove(entries[0])
		entries = entries[1:]
	}
	return dst, size, nil
}

// autoBackupToday 启动时若今天还没有备份则自动备份一次
func autoBackupToday() {
	today := time.Now().Format("20060102")
	entries, _ := filepath.Glob(backupsDir + "/backup-" + today + "-*.db")
	if len(entries) > 0 {
		return
	}
	if p, sz, err := backupNow(); err == nil {
		fmt.Printf("[backup] 启动自动备份完成: %s (%d bytes)\n", p, sz)
	} else {
		fmt.Printf("[backup] 自动备份失败: %v\n", err)
	}
}

// POST /api/backup —— 手动触发在线备份
func backupHandler(c *gin.Context) {
	p, sz, err := backupNow()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"path": p, "size": sz})
}

// GET /api/backups —— 列出历史备份
func listBackupsHandler(c *gin.Context) {
	entries, _ := filepath.Glob(backupsDir + "/backup-*.db")
	sort.Sort(sort.Reverse(sort.StringSlice(entries)))
	out := []gin.H{}
	for _, e := range entries {
		st, err := os.Stat(e)
		if err != nil {
			continue
		}
		out = append(out, gin.H{
			"name": filepath.Base(e),
			"size": st.Size(),
			"mtime": st.ModTime().Format(time.RFC3339),
		})
	}
	c.JSON(200, out)
}

// =====================================================
//  统计（校准页卡片 + 采样偏差提醒）
// =====================================================
func statsHandler(c *gin.Context) {
	res := gin.H{}
	var total int
	db.QueryRow("SELECT COUNT(*) FROM events").Scan(&total)
	var reviewed int
	db.QueryRow("SELECT COUNT(*) FROM events WHERE status='reviewed'").Scan(&reviewed)
	var incomplete int
	db.QueryRow("SELECT COUNT(*) FROM events WHERE people='' OR tags='' OR severity_self IS NULL OR severity_self=0").Scan(&incomplete)
	res["total_events"] = total
	res["reviewed_events"] = reviewed
	res["incomplete_events"] = incomplete

	// valence 分布
	valDist := map[string]int{"conflict": 0, "neutral": 0, "positive": 0, "empty": 0}
	if rows, err := db.Query("SELECT valence, COUNT(*) FROM events GROUP BY valence"); err == nil {
		for rows.Next() {
			var v sql.NullString
			var n int
			rows.Scan(&v, &n)
			key := "empty"
			if v.Valid && v.String != "" {
				key = v.String
				if _, ok := valDist[key]; !ok {
					key = "empty"
				}
			}
			valDist[key] += n
		}
		rows.Close()
	}
	res["valence_distribution"] = valDist

	// severity 分布 1..5
	sevDist := map[string]int{}
	if rows, err := db.Query("SELECT severity_self, COUNT(*) FROM events WHERE severity_self > 0 GROUP BY severity_self"); err == nil {
		for rows.Next() {
			var s, n int
			rows.Scan(&s, &n)
			sevDist[fmt.Sprint(s)] = n
		}
		rows.Close()
	}
	res["severity_distribution"] = sevDist

	// 近 7 天 / 近 30 天分布（timestamp 带 +08:00，SQLite datetime 可解析）
	windowCount := func(days int, valence string) int {
		q := "SELECT COUNT(*) FROM events WHERE datetime(timestamp) >= datetime('now', ?)"
		args := []interface{}{fmt.Sprintf("-%d days", days)}
		if valence != "" {
			q += " AND valence = ?"
			args = append(args, valence)
		}
		var n int
		db.QueryRow(q, args...).Scan(&n)
		return n
	}
	res["total_7d"] = windowCount(7, "")
	res["conflict_7d"] = windowCount(7, "conflict")
	res["neutral_7d"] = windowCount(7, "neutral")
	res["positive_7d"] = windowCount(7, "positive")
	res["total_30d"] = windowCount(30, "")
	res["conflict_30d"] = windowCount(30, "conflict")
	res["positive_30d"] = windowCount(30, "positive")

	// 人物 / 标签 Top（events.people 是逗号分隔，拉回来在 Go 里统计）
	peopleCnt := map[string]int{}
	tagsCnt := map[string]int{}
	if rows, err := db.Query("SELECT people, tags FROM events"); err == nil {
		for rows.Next() {
			var p, t sql.NullString
			rows.Scan(&p, &t)
			if p.Valid {
				for _, name := range splitAndTrim(p.String) {
					peopleCnt[name]++
				}
			}
			if t.Valid {
				for _, tag := range splitAndTrim(t.String) {
					tagsCnt[tag]++
				}
			}
		}
		rows.Close()
	}
	topN := func(m map[string]int, n int) []gin.H {
		type kv struct {
			k string
			v int
		}
		arr := make([]kv, 0, len(m))
		for k, v := range m {
			arr = append(arr, kv{k, v})
		}
		sort.Slice(arr, func(i, j int) bool { return arr[i].v > arr[j].v })
		out := []gin.H{}
		for i, a := range arr {
			if i >= n {
				break
			}
			out = append(out, gin.H{"name": a.k, "count": a.v})
		}
		return out
	}
	res["people_top"] = topN(peopleCnt, 5)
	res["tags_top"] = topN(tagsCnt, 8)

	// 时间跨度 + 分析数 + DB 大小
	var earliest, latest sql.NullString
	db.QueryRow("SELECT MIN(timestamp), MAX(timestamp) FROM events").Scan(&earliest, &latest)
	res["earliest_event"] = earliest.String
	res["latest_event"] = latest.String
	var analysesCnt int
	db.QueryRow("SELECT COUNT(*) FROM analyses").Scan(&analysesCnt)
	res["analyses_count"] = analysesCnt
	if st, err := os.Stat("./data/database.db"); err == nil {
		res["db_size"] = st.Size()
	}
	c.JSON(200, res)
}

// =====================================================
//  回复回填追踪：哪些 response_draft turn 还没事后回填
// =====================================================
// GET /api/draft-pending —— 扫描所有 response_draft 会话，列出未回填的 turn
func draftPendingHandler(c *gin.Context) {
	rows, err := db.Query("SELECT id, messages, created_at FROM sessions WHERE mode='response_draft' ORDER BY updated_at DESC")
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	defer rows.Close()
	var out []gin.H
	out = make([]gin.H, 0) // 保证空结果返回 [] 而不是 null，前端可直接 .length
	for rows.Next() {
		var sid int
		var messages, createdAt sql.NullString
		rows.Scan(&sid, &messages, &createdAt)
		if !messages.Valid {
			continue
		}
		var turns []Turn
		if json.Unmarshal([]byte(messages.String), &turns) != nil {
			continue
		}
		for i, t := range turns {
			if len(t.Replies) == 0 || t.DraftFilled {
				continue
			}
			snippet := strings.TrimSpace(t.UserContent)
			if len(snippet) > 40 {
				snippet = snippet[:40] + "…"
			}
			out = append(out, gin.H{
				"session_id":  sid,
				"turn_idx":    i,
				"created_at":  t.CreatedAt,
				"snippet":     snippet,
				"session_created": createdAt.String,
			})
		}
	}
	c.JSON(200, out)
}

// PATCH /api/sessions/:id/turn/:idx —— 标记该 turn 已回填（draft_filled）
func markTurnFilledHandler(c *gin.Context) {
	idStr := c.Param("id")
	idxStr := c.Param("idx")
	var idx int
	if _, err := fmt.Sscanf(idxStr, "%d", &idx); err != nil {
		c.JSON(400, gin.H{"error": "invalid turn index"})
		return
	}
	var body struct {
		DraftFilled bool `json:"draft_filled"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	var messagesJSON *string
	if err := db.QueryRow("SELECT messages FROM sessions WHERE id=?", idStr).Scan(&messagesJSON); err != nil {
		c.JSON(404, gin.H{"error": "会话不存在"})
		return
	}
	if messagesJSON == nil || *messagesJSON == "" {
		c.JSON(404, gin.H{"error": "会话无消息"})
		return
	}
	var turns []Turn
	if err := json.Unmarshal([]byte(*messagesJSON), &turns); err != nil || idx < 0 || idx >= len(turns) {
		c.JSON(400, gin.H{"error": "turn 索引无效"})
		return
	}
	turns[idx].DraftFilled = body.DraftFilled
	newJSON, _ := json.Marshal(turns)
	_, err := db.Exec("UPDATE sessions SET messages=?, updated_at=? WHERE id=?",
		newJSON, time.Now().UTC().Format(time.RFC3339), idStr)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true, "draft_filled": body.DraftFilled})
}

// registerExtrasRoutes 注册所有扩展路由（main.go 调用一行）
func registerExtrasRoutes(r *gin.Engine) {
	r.GET("/api/auth/verify", authVerifyHandler)
	r.POST("/api/auth/verify", authVerifyHandler) // 登录校验（前端 POST token）
	r.POST("/api/images", uploadImageHandler)
	r.GET("/api/images/:name", serveImageHandler)
	r.GET("/api/export", exportHandler)
	r.POST("/api/backup", backupHandler)
	r.GET("/api/backups", listBackupsHandler)
	r.GET("/api/stats", statsHandler)
	r.GET("/api/draft-pending", draftPendingHandler)
	r.PATCH("/api/sessions/:id/turn/:idx", markTurnFilledHandler)
}

var _ = http.StatusOK // 保留 http 导入，避免后续扩展时反复增删 import
