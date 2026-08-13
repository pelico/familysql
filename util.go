package main

import (
	"os"
	"strings"
)

// getEnv 读环境变量，缺省回退到 fallback。
// 部署时填在 docker-compose environment / k8s ConfigMap 里，前端通过 llm_status 拿到"是否已配置"的信号。
func getEnv(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && strings.TrimSpace(v) != "" {
		return v
	}
	return fallback
}

// uniq 字符串切片去重，保持首次出现顺序
func uniq(ss []string) []string {
	seen := make(map[string]struct{}, len(ss))
	out := make([]string, 0, len(ss))
	for _, s := range ss {
		if _, ok := seen[s]; !ok {
			seen[s] = struct{}{}
			out = append(out, s)
		}
	}
	return out
}

// intJoin 把 []int 用分隔符拼成字符串（event_id 列表展示）
func intJoin(xs []int, sep string) string {
	if len(xs) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, x := range xs {
		if i > 0 {
			sb.WriteString(sep)
		}
		sb.WriteString(intToStr(x))
	}
	return sb.String()
}

func intToStr(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	pos := len(buf)
	for n > 0 {
		pos--
		buf[pos] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}
