//go:build unit

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestResponsesDeepSeekTextTools(t *testing.T) {
	const valid = `<｜DSML｜tool_calls><｜DSML｜invoke name="functions__exec"><｜DSML｜parameter name="input" string="true">text(42)</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>`
	const broken = `<parameter name="input">{"cell_id":"21631"}</parameter>`
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, model, text        string
			tools, failed, recovered bool
		}{
			{"mapped_custom", "deepseek-v4.1-flash", valid, true, false, true},
			{"namespaced_model", "deepseek/deepseek-v4.1-flash", valid, true, false, true},
			{"thinking_before_tool", "deepseek-v4.1-flash", "<think>private analysis</think>" + valid, true, false, true},
			{"thinking_without_tools", "deepseek-v4.1-flash", "<think>private analysis</think>Answer.", false, false, false},
			{"incomplete_thinking", "deepseek-v4.1-flash", "<think>unfinished", false, true, false},
			{"no_tools_dsml", "deepseek-v4.1-flash", valid + "\n<think>PRIVATE</think>Answer.", false, true, false},
			{"nested_thinking", "deepseek-v4.1-flash", "<think>A<think>B</think>PRIVATE</think>Answer.", false, true, false},
			{"orphan_parameter", "deepseek-v4.1-flash", broken, true, true, false},
			{"malformed_dsml", "deepseek-v4.1-flash", `</｜｜DSML｜｜ calls>`, true, true, false},
			{"other_model", "other-model", broken, true, false, false},
			{"no_tools", "deepseek-v4.1-flash", broken, false, false, false},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				body := map[string]any{"model": "gpt-5.4", "input": "use the provided tool", "stream": stream}
				if strings.Contains(tc.text, "private analysis") {
					body["reasoning"] = map[string]any{"summary": "auto"}
				}
				if tc.tools {
					body["tools"] = []any{map[string]any{"type": "namespace", "name": "functions", "tools": []any{map[string]any{"type": "custom", "name": "exec"}}}}
				}
				raw, _ := json.Marshal(body)
				account := forceChatResponsesFallbackAccount()
				account.Credentials["model_mapping"] = map[string]any{"gpt-5.4": tc.model}
				var upstreamBody strings.Builder
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					// Emit one Unicode character per upstream delta so every SSE
					// frame is independently valid while all delimiters are split.
					for _, part := range strings.Split(tc.text, "") {
						encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": part}}}})
						_, _ = fmt.Fprintf(&upstreamBody, "data: %s\n\n", encoded)
					}
					_, _ = upstreamBody.WriteString("data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
				} else {
					encoded, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": tc.text}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
					_, _ = upstreamBody.Write(encoded)
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(raw))
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(upstreamBody.String()))}}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				result, err := svc.forwardResponsesViaRawChatCompletions(context.Background(), c, account, raw, time.Now())
				require.NoError(t, err)
				require.NotNil(t, result)
				require.Equal(t, 2, result.Usage.OutputTokens)
				output := rec.Body.String()
				require.NotContains(t, output, "<think>")
				require.NotContains(t, output, "</think>")
				if strings.Contains(tc.text, "private analysis") {
					require.Contains(t, output, `"type":"reasoning"`)
					require.Contains(t, output, "private analysis")
				}
				if tc.failed {
					require.Contains(t, output, "upstream_tool_protocol_error")
					require.NotContains(t, output, "response.completed")
					require.NotContains(t, output, "custom_tool_call")
					require.NotContains(t, output, "21631")
				} else if tc.recovered {
					require.Contains(t, output, `"custom_tool_call"`)
					require.Contains(t, output, `"namespace":"functions"`)
					require.Contains(t, output, `"input":"text(42)"`)
					require.NotContains(t, output, "DSML")
				} else {
					require.NotContains(t, output, "upstream_tool_protocol_error")
				}
			})
		}
	}
}

func TestMessagesDeepSeekTextTranslation(t *testing.T) {
	const call = `<｜DSML｜tool_calls><｜DSML｜invoke name="exec"><｜DSML｜parameter name="input" string="true">text(42)</｜DSML｜parameter></｜DSML｜invoke></｜DSML｜tool_calls>`
	for _, stream := range []bool{false, true} {
		for _, tc := range []struct {
			name, text, finish string
			tools, failed      bool
		}{
			{"thinking and tool", "<think>private analysis</think>" + call, "stop", true, false},
			{"thinking without tools", "<think>private analysis</think>Answer.", "stop", false, false},
			{"incomplete thinking", "<think>unfinished", "stop", false, true},
			{"nested thinking", "<think>A<think>B</think>PRIVATE</think>Answer.", "stop", false, true},
			{"no tools DSML", call + "\n<think>PRIVATE</think>Answer.", "stop", false, true},
			{"no terminal marker", "<think>R</think>partial", "", false, true},
			{"invalid terminal reason", "<think>R</think>partial", "aborted", false, true},
			{"native reasoning only", "", "stop", false, true},
			{"incomplete tool", strings.TrimSuffix(call, "</｜DSML｜tool_calls>"), "stop", true, true},
			{"undeclared tool", strings.Replace(call, `name="exec"`, `name="unknown"`, 1), "stop", true, true},
			{"truncated tool", call, "length", true, true},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, stream), func(t *testing.T) {
				if (tc.name == "no terminal marker" && !stream) || (tc.name == "native reasoning only" && stream) {
					t.Skip("case applies to the other response transport")
				}
				body := map[string]any{"model": "alias", "max_tokens": 32, "messages": []any{map[string]any{"role": "user", "content": "use the tool"}}, "stream": stream}
				if tc.tools {
					body["tools"] = []any{map[string]any{"name": "exec", "input_schema": map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}}}}
				}
				raw, err := json.Marshal(body)
				require.NoError(t, err)
				account := forceChatMessagesFallbackAccount()
				account.Credentials["model_mapping"] = map[string]any{"alias": "deepseek/deepseek-v4.1-flash"}
				var upstreamBody strings.Builder
				contentType := "application/json"
				if stream {
					contentType = "text/event-stream"
					for _, part := range strings.Split(tc.text, "") {
						encoded, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": part}}}})
						require.NoError(t, err)
						_, _ = fmt.Fprintf(&upstreamBody, "data: %s\n\n", encoded)
					}
					if tc.name == "no terminal marker" {
						_, _ = upstreamBody.WriteString("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\n")
					} else {
						_, _ = fmt.Fprintf(&upstreamBody, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":%q}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2,\"total_tokens\":5}}\n\ndata: [DONE]\n\n", tc.finish)
					}
				} else {
					message := map[string]any{"role": "assistant", "content": tc.text}
					if tc.name == "native reasoning only" {
						message["reasoning_content"] = "PRIVATE"
					}
					encoded, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"message": message, "finish_reason": tc.finish}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}})
					require.NoError(t, err)
					_, _ = upstreamBody.Write(encoded)
				}
				rec := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(rec)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(raw))
				upstream := &httpUpstreamRecorder{resp: &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{contentType}}, Body: io.NopCloser(strings.NewReader(upstreamBody.String()))}}
				svc := &OpenAIGatewayService{cfg: rawChatCompletionsTestConfig(), httpUpstream: upstream}
				result, err := svc.forwardAnthropicViaRawChatCompletions(context.Background(), c, account, raw, "")
				require.NotNil(t, result)
				require.Equal(t, 3, result.Usage.InputTokens)
				require.Equal(t, 2, result.Usage.OutputTokens)
				output := rec.Body.String()
				require.NotContains(t, output, "DSML")
				require.NotContains(t, output, "<think>")
				require.NotContains(t, output, "</think>")
				if tc.failed {
					require.Error(t, err)
					require.Contains(t, output, "upstream_tool_protocol_error")
					require.NotContains(t, output, `"type":"message_stop"`)
					require.NotContains(t, output, `"type":"tool_use"`)
					require.NotContains(t, output, "PRIVATE")
					if !stream {
						require.Equal(t, http.StatusBadGateway, rec.Code)
					}
					return
				}
				require.NoError(t, err)
				require.Contains(t, output, "private analysis")
				require.NotContains(t, output, `"text":"private analysis"`)
				if tc.tools {
					require.Contains(t, output, `"type":"tool_use"`)
					require.Contains(t, output, `"name":"exec"`)
					require.Contains(t, output, `"stop_reason":"tool_use"`)
				} else {
					require.Contains(t, output, `"text":"Answer."`)
					require.Contains(t, output, `"stop_reason":"end_turn"`)
				}
			})
		}
	}
}
