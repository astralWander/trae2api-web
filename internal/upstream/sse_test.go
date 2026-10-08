package upstream

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPrepareBodyForcesStreamAndFunction(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["stream"] != true {
		t.Errorf("stream=%v", m["stream"])
	}
	if m["function"] != "solo_work_lite" {
		t.Errorf("function=%v", m["function"])
	}
	if m["config_name"] != "glm-5.2" || m["model"] != "glm-5.2" {
		t.Errorf("model fields=%v / %v", m["config_name"], m["model"])
	}
	msgs := m["messages"].([]any)
	first := msgs[0].(map[string]any)
	content := first["content"].([]any)
	if content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "hi" {
		t.Errorf("content rewrite=%v", content)
	}
}

func TestPrepareBodyKeepsArrayContent(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"glm-5.2","messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	msgs := m["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Errorf("array content should pass through: %v", content)
	}
}

func TestPrepareBodyToolChoiceFunctionObject(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"glm-5.2","tool_choice":{"type":"function","function":{"name":"get_weather"}},"tools":[{"type":"function","function":{"name":"get_weather"}}]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	if m["tool_choice"] != "get_weather" {
		t.Errorf("tool_choice=%v", m["tool_choice"])
	}
	if _, ok := m["tools"]; !ok {
		t.Error("tools should be kept for function choice")
	}
}

func TestPrepareBodyToolChoiceNone(t *testing.T) {
	out := PrepareBody([]byte(`{"model":"glm-5.2","tool_choice":"none","tools":[{}],"functions":[{}]}`))
	var m map[string]any
	json.Unmarshal(out, &m)
	if _, ok := m["tool_choice"]; ok {
		t.Error("tool_choice should be deleted")
	}
	if _, ok := m["tools"]; ok {
		t.Error("tools should be deleted")
	}
	if _, ok := m["functions"]; ok {
		t.Error("functions should be deleted")
	}
}

func TestPrepareBodyInvalidJSON(t *testing.T) {
	in := []byte(`{broken`)
	out := PrepareBody(in)
	if string(out) != string(in) {
		t.Error("invalid json should pass through unchanged")
	}
}

// 捕获的 SOLO llm_utils_chat SSE 样例（真实结构，token 无）。
const soloSSEFixture = "id:1\nevent:metadata\ndata:{\"model\":\"\",\"session_id\":\"897f0f3f-935a-4f42-a0fc-60f5140ccd02\",\"prompt_completion_id\":0}\n\n" +
	"id:2\nevent:timing_cost\ndata:{\"name\":\"llm_raw_chat_v2\",\"preprocess_timing\":71}\n\n" +
	"event:output\ndata:{\"response\":\"中国\",\"reasoning_content\":\"让我想想\",\"tool_calls\":null}\n\n" +
	"event:output\ndata:{\"response\":\"的首都是北京。\",\"reasoning_content\":\"\",\"tool_calls\":null}\n\n" +
	"event:extra_info\ndata:{\"reasoning_content\":\"让我想想\"}\n\n" +
	"event:token_usage\ndata:{\"prompt_tokens\":21,\"completion_tokens\":142,\"total_tokens\":163,\"reasoning_tokens\":135}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

func TestAggregateSOLO(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(soloSSEFixture))
	if err != nil {
		t.Fatal(err)
	}
	if resp["object"] != "chat.completion" {
		t.Errorf("object=%v", resp["object"])
	}
	choices := resp["choices"].([]any)
	msg := choices[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "中国的首都是北京。" {
		t.Errorf("content=%q", msg["content"])
	}
	if msg["reasoning_content"] != "让我想想" {
		t.Errorf("reasoning=%q", msg["reasoning_content"])
	}
	if choices[0].(map[string]any)["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v", choices[0].(map[string]any)["finish_reason"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["total_tokens"].(float64) != 163 {
		t.Errorf("usage=%v", usage)
	}
}

func TestAggregateSOLOError(t *testing.T) {
	raw := "event:error\ndata:{\"code\":4001,\"message\":\"We're sorry, the param is invalid.\",\"extra\":null}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"
	_, err := Aggregate(strings.NewReader(raw))
	if err == nil {
		t.Fatal("want error from event:error")
	}
}

func TestParseSOLOLine(t *testing.T) {
	ev, err := ParseSOLOLine("output", `{"response":"hi","reasoning_content":"think","tool_calls":null}`)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Response != "hi" || ev.Reasoning != "think" {
		t.Errorf("ev=%+v", ev)
	}
	if string(ev.ToolCalls) != "null" {
		t.Errorf("tool_calls=%s", ev.ToolCalls)
	}
	ev, err = ParseSOLOLine("done", `{"finish_reason":"stop"}`)
	if err != nil || ev.FinishReason != "stop" {
		t.Errorf("done: %+v %v", ev, err)
	}
}

func TestAggregateSOLOToolCalls(t *testing.T) {
	raw := "event:output\ndata:{\"response\":\"\",\"reasoning_content\":\"\",\"tool_calls\":[{\"id\":\"call_a\",\"type\":\"function\",\"function\":{\"name\":\"get_weather\",\"arguments\":\"{\\\"city\\\":\\\"北京\\\"}\"},\"index\":0}]}\n\n" +
		"event:done\ndata:{\"finish_reason\":\"tool_calls\"}\n\n"
	resp, err := Aggregate(strings.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	choice := resp["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Errorf("finish_reason=%v", choice["finish_reason"])
	}
	msg := choice["message"].(map[string]any)
	calls, ok := msg["tool_calls"].([]map[string]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool_calls=%#v", msg["tool_calls"])
	}
	if calls[0]["id"] != "call_a" {
		t.Errorf("call id=%v", calls[0]["id"])
	}
	fn := calls[0]["function"].(map[string]any)
	if fn["name"] != "get_weather" || fn["arguments"] != `{"city":"北京"}` {
		t.Errorf("fn=%v", fn)
	}
}

func TestStreamConvertsToOpenAIChunks(t *testing.T) {
	rec := httptest.NewRecorder()
	err := Stream(rec, strings.NewReader(soloSSEFixture))
	if err != nil {
		t.Fatal(err)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"object":"chat.completion.chunk"`) {
		t.Errorf("missing chunk object: %q", body)
	}
	if !strings.Contains(body, `"content":"中国"`) {
		t.Errorf("missing content delta: %q", body)
	}
	if !strings.Contains(body, `"reasoning_content"`) {
		t.Errorf("missing reasoning delta: %q", body)
	}
	if !strings.Contains(body, "data: [DONE]") {
		t.Errorf("missing [DONE]: %q", body)
	}
	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("content-type=%q", ct)
	}
}

func TestStreamGuaranteesDone(t *testing.T) {
	rec := httptest.NewRecorder()
	// 上游中断（只有 output，无 done）
	err := Stream(rec, strings.NewReader("event:output\ndata:{\"response\":\"x\",\"reasoning_content\":\"\",\"tool_calls\":null}\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Errorf("missing [DONE]: %q", rec.Body.String())
	}
}

func TestPrepareBodyToolsParametersStringified(t *testing.T) {
	src := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"get_weather","description":"weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]}`
	out := PrepareBody([]byte(src))
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		t.Fatalf("tools missing: %#v", obj["tools"])
	}
	tool, _ := tools[0].(map[string]any)
	fn, _ := tool["function"].(map[string]any)
	params, ok := fn["parameters"].(string)
	if !ok {
		t.Fatalf("parameters should be string, got %T", fn["parameters"])
	}
	if !strings.Contains(params, `"city"`) {
		t.Fatalf("parameters content missing: %s", params)
	}
}

func TestPrepareBodyToolsInvalidEntriesDropped(t *testing.T) {
	src := `{"model":"glm-5.2","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"ok","parameters":{"type":"object"}}},{"bad":1}]}`
	out := PrepareBody([]byte(src))
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	tools, ok := obj["tools"].([]any)
	if !ok {
		t.Fatalf("tools should exist")
	}
	if len(tools) != 1 {
		t.Fatalf("invalid entry should be dropped, got %d", len(tools))
	}
}

func TestMergeToolCallSOLOFunctionCall(t *testing.T) {
	// 模拟 SOLO 上游的 function_call 格式(流式两段: 先 name,再 arguments)
	toolCalls := map[int]map[string]any{}
	var order []int
	m1 := json.RawMessage(`[{"index":0,"id":"call_x","type":"function","function_call":{"name":"get_weather","arguments":"{\"city\":\"北京\""}}]`)
	m2 := json.RawMessage(`[{"index":0,"id":"","type":"function","function_call":{"name":"","arguments":"}"}}]`)
	mergeToolCallJSON(toolCalls, &order, m1)
	mergeToolCallJSON(toolCalls, &order, m2)
	if len(order) != 1 || order[0] != 0 {
		t.Fatalf("order=%v", order)
	}
	m := toolCalls[0]
	if m["id"] != "call_x" || m["type"] != "function" {
		t.Fatalf("id/type: %#v", m)
	}
	fn, ok := m["function"].(map[string]any)
	if !ok {
		t.Fatalf("function missing: %#v", m)
	}
	if fn["name"] != "get_weather" {
		t.Fatalf("name=%v", fn["name"])
	}
	args, _ := fn["arguments"].(string)
	if args != `{"city":"北京"}` {
		t.Fatalf("arguments=%q", args)
	}
}

func TestMergeToolCallStripsSOLOFields(t *testing.T) {
	toolCalls := map[int]map[string]any{}
	var order []int
	m := json.RawMessage(`[{"index":0,"id":"call_y","type":"function","function_call":{"name":"skill_view","arguments":"{\"name\":\"x\"}","namespace":"trae","partial_arguments":null}}]`)
	mergeToolCallJSON(toolCalls, &order, m)
	fn, _ := toolCalls[0]["function"].(map[string]any)
	if _, has := fn["namespace"]; has {
		t.Error("namespace should be stripped")
	}
	if _, has := fn["partial_arguments"]; has {
		t.Error("partial_arguments should be stripped")
	}
	if fn["name"] != "skill_view" {
		t.Errorf("name=%v", fn["name"])
	}
}

func TestPrepareBodyAssistantToolCallsToFunctionCall(t *testing.T) {
	src := `{"model":"glm-5.2","messages":[
	  {"role":"user","content":"hi"},
	  {"role":"assistant","content":null,"tool_calls":[{"id":"call_x","type":"function","function":{"name":"skill_view","arguments":"{\"name\":\"hermes-agent\"}"}}]},
	  {"role":"tool","tool_call_id":"call_x","content":"skill content"}
	],"tools":[{"type":"function","function":{"name":"skill_view"}}]}`
	out := PrepareBody([]byte(src))
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	tcs := assistant["tool_calls"].([]any)
	tc := tcs[0].(map[string]any)
	if _, has := tc["function"]; has {
		t.Error("function should be converted to function_call")
	}
	fc, ok := tc["function_call"].(map[string]any)
	if !ok {
		t.Fatalf("function_call missing: %#v", tc)
	}
	if fc["name"] != "skill_view" {
		t.Errorf("function_call.name=%v", fc["name"])
	}
}

func TestPrepareBodyToolCallWithoutNameDropped(t *testing.T) {
	src := `{"model":"glm-5.2","messages":[
	  {"role":"user","content":"hi"},
	  {"role":"assistant","tool_calls":[{"id":"call_bad","type":"function","function":{"arguments":"{}"}}]}
	]}`
	out := PrepareBody([]byte(src))
	var obj map[string]any
	_ = json.Unmarshal(out, &obj)
	msgs := obj["messages"].([]any)
	assistant := msgs[1].(map[string]any)
	if _, has := assistant["tool_calls"]; has {
		t.Error("tool_call without name should be dropped")
	}
}

// 上游 token_usage 实测样例（2026-10-08，本次命中上下文缓存 1792/2033）。
const soloSSECacheFixture = "event:output\ndata:{\"response\":\"好\",\"reasoning_content\":\"\",\"tool_calls\":null}\n\n" +
	"event:token_usage\ndata:{\"name\":\"\",\"prompt_tokens\":2033,\"completion_tokens\":123,\"total_tokens\":2156," +
	"\"cache_creation_input_tokens\":0,\"cache_read_input_tokens\":1792,\"reasoning_tokens\":121,\"cluster\":\"normal_context\"}\n\n" +
	"event:done\ndata:{\"finish_reason\":\"stop\"}\n\n"

// 缓存字段必须被翻译成 OpenAI / DeepSeek 两套口径，否则客户端恒显示「缓存 0」。
func TestNormalizeUsageAddsCompatibleCacheFields(t *testing.T) {
	u := normalizeUsage(map[string]any{
		"prompt_tokens":                float64(2033),
		"completion_tokens":            float64(123),
		"cache_read_input_tokens":      float64(1792),
		"cache_creation_input_tokens":  float64(0),
		"reasoning_tokens":             float64(121),
		"prompt_cache_hit_tokens_total": float64(0), // 上游自带字段，不得被改
	})
	// 原字段保留（Anthropic 口径的客户端仍能读）
	if u["cache_read_input_tokens"].(float64) != 1792 {
		t.Errorf("原始字段被改动: %v", u["cache_read_input_tokens"])
	}
	// OpenAI 规范
	det, ok := u["prompt_tokens_details"].(map[string]any)
	if !ok {
		t.Fatalf("prompt_tokens_details 缺失: %#v", u)
	}
	if det["cached_tokens"].(int64) != 1792 {
		t.Errorf("cached_tokens=%v", det["cached_tokens"])
	}
	// DeepSeek 口径
	if u["prompt_cache_hit_tokens"].(int64) != 1792 {
		t.Errorf("prompt_cache_hit_tokens=%v", u["prompt_cache_hit_tokens"])
	}
	if u["prompt_cache_miss_tokens"].(int64) != 241 { // 2033 - 1792
		t.Errorf("prompt_cache_miss_tokens=%v", u["prompt_cache_miss_tokens"])
	}
	// 思考链 token
	cd, ok := u["completion_tokens_details"].(map[string]any)
	if !ok || cd["reasoning_tokens"].(int64) != 121 {
		t.Errorf("completion_tokens_details=%v", u["completion_tokens_details"])
	}
}

// 未命中缓存（首次请求）也要给出一致的 0，避免客户端显示成「-」。
func TestNormalizeUsageColdCacheStillEmitsZero(t *testing.T) {
	u := normalizeUsage(map[string]any{
		"prompt_tokens":               float64(100),
		"cache_read_input_tokens":     float64(0),
		"cache_creation_input_tokens": float64(0),
	})
	if u["prompt_tokens_details"].(map[string]any)["cached_tokens"].(int64) != 0 {
		t.Errorf("cached_tokens=%v", u["prompt_tokens_details"])
	}
	if u["prompt_cache_hit_tokens"].(int64) != 0 || u["prompt_cache_miss_tokens"].(int64) != 100 {
		t.Errorf("hit/miss=%v/%v", u["prompt_cache_hit_tokens"], u["prompt_cache_miss_tokens"])
	}
	if _, has := u["completion_tokens_details"]; has {
		t.Error("无 reasoning_tokens 时不应凭空造 completion_tokens_details")
	}
}

// normalizeUsage 必须容忍 nil 与类型异常（上游字段可能缺失）。
func TestNormalizeUsageNilAndOddTypes(t *testing.T) {
	if got := normalizeUsage(nil); got != nil {
		t.Errorf("nil 应原样返回: %#v", got)
	}
	u := normalizeUsage(map[string]any{"cache_read_input_tokens": "1792"}) // 字符串而非数字
	if u["prompt_tokens_details"].(map[string]any)["cached_tokens"].(int64) != 0 {
		t.Errorf("类型异常应退化为 0: %#v", u["prompt_tokens_details"])
	}
	if u["prompt_cache_miss_tokens"].(int64) != 0 { // 不可为负
		t.Errorf("miss 不应为负: %v", u["prompt_cache_miss_tokens"])
	}
}

func TestAggregateCarriesCachedTokens(t *testing.T) {
	resp, err := Aggregate(strings.NewReader(soloSSECacheFixture))
	if err != nil {
		t.Fatal(err)
	}
	usage := resp["usage"].(map[string]any)
	if _, ok := usage["prompt_tokens_details"].(map[string]any); !ok {
		t.Fatalf("非流式 usage 缺 prompt_tokens_details: %#v", usage)
	}
	if got := usageInt(usage, "prompt_tokens_details", "cached_tokens"); got != 1792 {
		t.Errorf("cached_tokens=%v", got)
	}
	if got := usageInt(usage, "prompt_cache_hit_tokens"); got != 1792 {
		t.Errorf("prompt_cache_hit_tokens=%v", got)
	}
}

func TestStreamCarriesCachedTokens(t *testing.T) {
	rec := httptest.NewRecorder()
	if err := Stream(rec, strings.NewReader(soloSSECacheFixture)); err != nil {
		t.Fatal(err)
	}
	var usage map[string]any
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" || !strings.Contains(payload, `"usage"`) {
			continue
		}
		var chunk map[string]any
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
	}
	if usage == nil {
		t.Fatal("流式响应里没有带 usage 的 chunk")
	}
	// 注意：流式 usage 经过 json.Marshal/Unmarshal 往返，数字变成 float64，
	// 不能直接断言 int64（会 panic）。
	if got := usageInt(usage, "prompt_tokens_details", "cached_tokens"); got != 1792 {
		t.Errorf("流式 cached_tokens=%v（usage=%#v）", got, usage)
	}
	if got := usageInt(usage, "prompt_cache_hit_tokens"); got != 1792 {
		t.Errorf("流式 prompt_cache_hit_tokens=%v", got)
	}
}

// usageInt 从嵌套 usage 里取整数，兼容 int64（未经过 JSON 往返）与
// float64（经过往返）两种形态；路径不存在返回 -1。
func usageInt(u map[string]any, path ...string) int64 {
	var cur any = u
	for _, k := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return -1
		}
		cur, ok = m[k]
		if !ok {
			return -1
		}
	}
	switch v := cur.(type) {
	case int64:
		return v
	case float64:
		return int64(v)
	}
	return -1
}
