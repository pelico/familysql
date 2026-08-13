package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLMProfile 是一个可调用的大模型配置。
// 多模型并行时，从 llm_profiles 表取 is_active=1 的行，每个 profile 并发调一轮。
type LLMProfile struct {
	ID        int    `json:"id"`
	Name      string `json:"name"`
	APIBase   string `json:"api_base"`
	APIKey    string `json:"api_key,omitempty"` // 列表返回时会脱敏成 "***"
	Model     string `json:"model"`
	IsActive  int    `json:"is_active"`
	CreatedAt string `json:"created_at"`
}

// APIMessage 是 OpenAI 兼容的 ChatCompletion message 结构
type APIMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// sanitizeKey 脱敏：仅保留前 4 后 4，其余变 *
func sanitizeKey(k string) string {
	if len(k) <= 8 {
		return "***"
	}
	return k[:4] + strings.Repeat("*", len(k)-8) + k[len(k)-4:]
}

// listActiveProfiles 从 DB 取所有 is_active=1 的 profile，用于多模型并发
func listActiveProfiles() ([]LLMProfile, error) {
	rows, err := db.Query("SELECT id, name, api_base, api_key, model, is_active, created_at FROM llm_profiles WHERE is_active = 1 ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LLMProfile
	for rows.Next() {
		var p LLMProfile
		rows.Scan(&p.ID, &p.Name, &p.APIBase, &p.APIKey, &p.Model, &p.IsActive, &p.CreatedAt)
		out = append(out, p)
	}
	return out, nil
}

// listAllProfiles 返回所有配置（API 返回时 api_key 脱敏）
func listAllProfiles() ([]LLMProfile, error) {
	rows, err := db.Query("SELECT id, name, api_base, api_key, model, is_active, created_at FROM llm_profiles ORDER BY id ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LLMProfile
	for rows.Next() {
		var p LLMProfile
		rows.Scan(&p.ID, &p.Name, &p.APIBase, &p.APIKey, &p.Model, &p.IsActive, &p.CreatedAt)
		p.APIKey = sanitizeKey(p.APIKey)
		out = append(out, p)
	}
	return out, nil
}

// callLLMByProfile 按单个 profile 发起一次 ChatCompletion 调用。
func callLLMByProfile(p LLMProfile, messages []APIMessage) (string, error) {
	url := strings.TrimRight(p.APIBase, "/") + "/chat/completions"
	body := map[string]interface{}{
		"model":       p.Model,
		"messages":    messages,
		"temperature": 0.2,
	}
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+p.APIKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP 调用失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("模型 %s 返回 %d: %s", p.Name, resp.StatusCode, string(raw))
	}
	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error,omitempty"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		n := len(raw)
		if n > 500 {
			n = 500
		}
		return "", fmt.Errorf("解析响应失败: %w, 原始: %s", err, string(raw[:n]))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("模型 %s 报错: %s", p.Name, parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("模型 %s 无 choices 返回", p.Name)
	}
	return parsed.Choices[0].Message.Content, nil
}
