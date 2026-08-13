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

// MultiModalContentPart 是 OpenAI 多模态 messages[n].content 的一块（text 或 image_url）
type MultiModalContentPart struct {
	Type     string            `json:"type"` // "text" 或 "image_url"
	Text     string            `json:"text,omitempty"`
	ImageURL map[string]string `json:"image_url,omitempty"` // {"url":"data:image/jpeg;base64,..."}
}

// APIMessage 是 OpenAI 兼容的 ChatCompletion message 结构。
// 多模态时 ContentParts 非空（会在序列化时覆盖 Content 为数组结构）。
type APIMessage struct {
	Role         string                 `json:"role"`
	Content      string                 `json:"content,omitempty"`
	ContentParts []MultiModalContentPart `json:"-"` // 非空时调用 callLLMByProfileMulti
}

// toWireMessage 把 APIMessage 转成可序列化的 wire 结构（text 或 多模态数组二选一）
func (m APIMessage) toWire() interface{} {
	if len(m.ContentParts) == 0 {
		return struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		}{Role: m.Role, Content: m.Content}
	}
	return struct {
		Role         string                 `json:"role"`
		ContentParts []MultiModalContentPart `json:"content"`
	}{Role: m.Role, ContentParts: m.ContentParts}
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
// messages 里任何一条含有 ContentParts 都会自动走多模态消息结构。
func callLLMByProfile(p LLMProfile, messages []APIMessage) (string, error) {
	url := strings.TrimRight(p.APIBase, "/") + "/chat/completions"
	wire := make([]interface{}, len(messages))
	for i, m := range messages {
		wire[i] = m.toWire()
	}
	body := map[string]interface{}{
		"model":       p.Model,
		"messages":    wire,
		"temperature": 0.2,
	}
	return doLLMRequest(url, p.APIKey, body)
}

// callLLMForJSON 同上，但 temperature 降到更低（0.1），对响应尝试解析成 dst
func callLLMForJSON(p LLMProfile, messages []APIMessage, dst interface{}) error {
	url := strings.TrimRight(p.APIBase, "/") + "/chat/completions"
	wire := make([]interface{}, len(messages))
	for i, m := range messages {
		wire[i] = m.toWire()
	}
	body := map[string]interface{}{
		"model":       p.Model,
		"messages":    wire,
		"temperature": 0.1,
	}
	raw, err := doLLMRequest(url, p.APIKey, body)
	if err != nil {
		return err
	}
	// 宽松解析：先把 ```json ... ``` 这种包裹剥掉
	cleaned := stripFencedJSON(raw)
	if err := json.Unmarshal([]byte(cleaned), dst); err != nil {
		// 再尝试从文本里抠第一个 { ... }
		if i := strings.Index(cleaned, "{"); i >= 0 {
			if j := strings.LastIndex(cleaned, "}"); j > i {
				if err2 := json.Unmarshal([]byte(cleaned[i:j+1]), dst); err2 == nil {
					return nil
				}
			}
		}
		return fmt.Errorf("无法解析为JSON: %w, 原始片段: %s", err, truncate(raw, 400))
	}
	return nil
}

func doLLMRequest(url, apiKey string, body map[string]interface{}) (string, error) {
	buf, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url, bytes.NewReader(buf))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("HTTP 调用失败: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("模型返回 %d: %s", resp.StatusCode, truncate(string(raw), 600))
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
		return "", fmt.Errorf("解析响应失败: %w, 原始: %s", err, truncate(string(raw), 400))
	}
	if parsed.Error != nil && parsed.Error.Message != "" {
		return "", fmt.Errorf("模型报错: %s", parsed.Error.Message)
	}
	if len(parsed.Choices) == 0 {
		return "", fmt.Errorf("模型无 choices 返回")
	}
	return parsed.Choices[0].Message.Content, nil
}

func stripFencedJSON(s string) string {
	t := strings.TrimSpace(s)
	// 删除 ```json ... ```
	if strings.HasPrefix(t, "```") {
		t = strings.TrimPrefix(t, "```")
		t = strings.TrimPrefix(t, "json")
		t = strings.TrimPrefix(t, "JSON")
		t = strings.TrimSpace(t)
		if i := strings.LastIndex(t, "```"); i >= 0 {
			t = t[:i]
		}
	}
	return strings.TrimSpace(t)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(截断)"
}
