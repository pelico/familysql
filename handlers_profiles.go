package main

import (
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// GET /api/llm_status —— 前端启动时轮询一次，决定"分析页签"显示"请先配置模型"还是正常操作
func llmStatusHandler(c *gin.Context) {
	profiles, err := listActiveProfiles()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	// 兼容旧部署：如果 llm_profiles 空表，回退到环境变量里填的单模型配置（以 profile_id=0 表示）
	if len(profiles) == 0 {
		envBase := getEnv("LLM_API_BASE", "")
		envKey := getEnv("LLM_API_KEY", "")
		envModel := getEnv("LLM_MODEL", "")
		if envBase != "" && envKey != "" && envModel != "" {
			c.JSON(200, gin.H{
				"configured": true,
				"profiles": []LLMProfile{{
					ID: 0, Name: "(环境变量默认)", APIBase: envBase, APIKey: "***", Model: envModel, IsActive: 1,
				}},
				"fallback_env": true,
			})
			return
		}
	}
	c.JSON(200, gin.H{
		"configured":    len(profiles) > 0,
		"profiles":      sanitizeProfiles(profiles),
		"fallback_env":  false,
	})
}

func sanitizeProfiles(ps []LLMProfile) []LLMProfile {
	out := make([]LLMProfile, len(ps))
	for i, p := range ps {
		p.APIKey = sanitizeKey(p.APIKey)
		out[i] = p
	}
	return out
}

// listProfilesHandler GET /api/llm_profiles
func listProfilesHandler(c *gin.Context) {
	list, err := listAllProfiles()
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, list)
}

// createProfileHandler POST /api/llm_profiles
// body: {name, api_base, api_key, model, is_active}
func createProfileHandler(c *gin.Context) {
	var body struct {
		Name     string `json:"name" binding:"required"`
		APIBase  string `json:"api_base" binding:"required"`
		APIKey   string `json:"api_key" binding:"required"`
		Model    string `json:"model" binding:"required"`
		IsActive int    `json:"is_active"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.Exec(
		"INSERT INTO llm_profiles (name, api_base, api_key, model, is_active, created_at, updated_at) VALUES (?,?,?,?,?,?,?)",
		body.Name, body.APIBase, body.APIKey, body.Model, body.IsActive, now, now,
	)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	id, _ := res.LastInsertId()
	// 如果新 profile 标记为 active，按项目约定：任何时刻 active 的可能有多个（多模型并行），不互斥取消
	c.JSON(201, gin.H{"id": id, "name": body.Name})
}

// updateProfileHandler PATCH /api/llm_profiles/:id
// 允许部分字段更新；api_key 传空字符串表示不修改
func updateProfileHandler(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}
	var body map[string]interface{}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	allowed := map[string]bool{"name": true, "api_base": true, "api_key": true, "model": true, "is_active": true}
	sets := []string{}
	args := []interface{}{}
	for k, v := range body {
		if !allowed[k] {
			continue
		}
		// api_key 传空串 = 不修改
		if k == "api_key" {
			s, _ := v.(string)
			if s == "" || s == "***" {
				continue
			}
		}
		sets = append(sets, k+" = ?")
		args = append(args, v)
	}
	if len(sets) == 0 {
		c.JSON(400, gin.H{"error": "无有效字段更新"})
		return
	}
	sets = append(sets, "updated_at = ?")
	args = append(args, time.Now().UTC().Format(time.RFC3339))
	args = append(args, id)
	_, err = db.Exec("UPDATE llm_profiles SET "+joinComma(sets)+" WHERE id = ?", args...)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

// deleteProfileHandler DELETE /api/llm_profiles/:id
func deleteProfileHandler(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.JSON(400, gin.H{"error": "invalid id"})
		return
	}
	_, err = db.Exec("DELETE FROM llm_profiles WHERE id = ?", id)
	if err != nil {
		c.JSON(500, gin.H{"error": err.Error()})
		return
	}
	c.JSON(200, gin.H{"ok": true})
}

func joinComma(xs []string) string {
	s := ""
	for i, x := range xs {
		if i > 0 {
			s += ", "
		}
		s += x
	}
	return s
}
