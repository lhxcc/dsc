package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/option"
)

// TestOmitZeroMaxTokensKeepsMinusOne 三态在 HTTP 层必须严格区分：
// 0 = 禁用（字段被摘除）、-1 = 无限（原样上送）、N = 指定。
//
// 这条测试挡的是「顺手把负值也归一成零」这类改动：-1 一旦被吃掉，「无限」就悄悄退化成
// 「服务端默认」（llama.cpp 兼容口自填 4096，长回答被腰斩），而调用方看不出任何异常。
func TestOmitZeroMaxTokensKeepsMinusOne(t *testing.T) {
	for _, c := range []struct {
		name    string
		payload string
		want    string // "" = 字段应被摘除
	}{
		{"禁用（0）", `{"model":"m","max_tokens":0}`, ""},
		{"无限（-1）", `{"model":"m","max_tokens":-1}`, "-1"},
		{"指定（8192）", `{"model":"m","max_tokens":8192}`, "8192"},
	} {
		t.Run(c.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "http://x/v1/messages", strings.NewReader(c.payload))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := omitZeroMaxTokens(req, func(r *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(r.Body)
				var m map[string]json.RawMessage
				if err := json.Unmarshal(body, &m); err != nil {
					t.Fatalf("中间件吐出的不是 JSON: %v (%s)", err, body)
				}
				raw, ok := m["max_tokens"]
				if c.want == "" {
					if ok {
						t.Fatalf("0 应被摘除，实际 %s", raw)
					}
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
				}
				if !ok {
					t.Fatalf("max_tokens 被摘掉了，实际缺字段（期望 %s）", c.want)
				}
				if got := string(raw); got != c.want {
					t.Fatalf("max_tokens = %s, want %s", got, c.want)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(""))}, nil
			}); err != nil {
				t.Fatalf("中间件返回错误: %v", err)
			}
		})
	}
}

var _ option.MiddlewareNext // 保持与生产签名一致的编译期约束
