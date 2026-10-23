package app

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 这一组测的是 admin 配置 PUT 的**部分更新**语义。
//
// 背景（bug #27）：body 是整结构 json.Decode，而前端 saveRtCfg 只按 RT_GROUPS 拼字段。
// 两边一旦不同步（新加了 RuntimeConfig 字段忘了进 RT_GROUPS），缺失字段会被解成零值
// 悄悄冲掉 —— multi_turn 就这么被重置过。这种 bug 不报错、只表现为「设了不管用」。
//
// 修法：把 body 盖在**当前配置**之上（next := rtCfg()），缺字段等于不改。

// putRuntimeConfig 直接把 body 喂给 handleAdminConfig，返回解码后的响应。
func putRuntimeConfig(t *testing.T, body string) map[string]interface{} {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/admin/api/config", strings.NewReader(body))
	w := httptest.NewRecorder()
	handleAdminConfig(w, req)
	if w.Code != 200 {
		t.Fatalf("PUT 失败 code=%d body=%s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	return resp
}

// 保存现场：handleAdminConfig 会真的写 kv 并换掉 rtVal，测完必须还原。
//
// 顺带铺一个**合法**基线：测试进程从来没跑过 loadConfig/initRuntimeConfig，cfg 和 rtVal
// 都是零值，而零值过不了 validateRuntimeConfig（`retry_attempts=0 超出允许范围 [1,10]`）。
// 不铺的话这些用例只会撞到校验器的拒绝路径，测不到「缺字段保不保留」。
func withRuntimeConfigSaved(t *testing.T) {
	t.Helper()
	oldCfg := cfg
	old := rtCfg()
	oldRaw := kvGet(runtimeConfigKey)
	cfg = defaultConfig()
	_ = kvSet(runtimeConfigKey, "")
	initRuntimeConfig()
	t.Cleanup(func() {
		cfg = oldCfg
		rtMu.Lock()
		rtVal = old
		rtMu.Unlock()
		_ = kvSet(runtimeConfigKey, oldRaw)
	})
}

// TestRuntimeConfigPutKeepsAbsentFields 是本组的核心：
// body 里没提到的字段必须保持原值，不能被零值冲掉。
func TestRuntimeConfigPutKeepsAbsentFields(t *testing.T) {
	withRuntimeConfigSaved(t)

	// 先把不重放关掉、抓出当前值，再发一个**只含一个字段**的 body。
	before := rtCfg()
	putRuntimeConfig(t, `{"tool_refusal_retry": false}`)

	got := rtCfg()
	if got.ToolRefusalRetry {
		t.Errorf("body 明确写了 false，结果还是 true")
	}
	// 其余字段一个都不该动。
	if got.MultiTurn != before.MultiTurn {
		t.Errorf("MultiTurn 被冲掉了: %v -> %v", before.MultiTurn, got.MultiTurn)
	}
	if got.PerIPRPH != before.PerIPRPH {
		t.Errorf("PerIPRPH 被冲掉了: %d -> %d", before.PerIPRPH, got.PerIPRPH)
	}
	if got.RetryAttempts != before.RetryAttempts {
		t.Errorf("RetryAttempts 被冲掉了: %d -> %d", before.RetryAttempts, got.RetryAttempts)
	}
	if got.DefaultModel != before.DefaultModel {
		t.Errorf("DefaultModel 被冲掉了: %q -> %q", before.DefaultModel, got.DefaultModel)
	}
}

// 反过来也要成立：**空 body** 不该改动任何东西（旧实现在这里会把全部字段清零）。
func TestRuntimeConfigPutEmptyBodyChangesNothing(t *testing.T) {
	withRuntimeConfigSaved(t)

	before := rtCfg()
	putRuntimeConfig(t, `{}`)
	got := rtCfg()

	if got != before {
		t.Errorf("空 body 不该改动配置\n before=%+v\n 后=%+v", before, got)
	}
}

// 新字段必须留在 RT_GROUPS 里。
//
// 这是上面那个 bug 的**源头**检查：后端已经改成部分更新，所以就算漏了也不会再冲掉
// 别的字段，但漏了会让面板上根本没这个开关（用户想关都关不掉）。前端是个大 HTML
// 字符串常量，没有类型系统兜底，只能靠这条断言钉住。
func TestToolRefusalRetryIsInAdminUIGroups(t *testing.T) {
	html, err := adminUIFS.ReadFile("admin_ui/index.html")
	if err != nil {
		t.Fatalf("读不到内嵌面板: %v", err)
	}
	if !bytes.Contains(html, []byte("tool_refusal_retry")) {
		t.Fatal("admin_ui/index.html 的 RT_GROUPS 里没有 tool_refusal_retry —— 面板上不会出现这个开关")
	}
}

// 降级检测：付费号每日配额用完后 Google 静默把账号降成 Flash-Lite 继续应答
//（200、无报错，实测 2026-10-17）。只有「请求的是高级模型」才算降级 ——
// Lite 自己、以及生图/生乐/视频管线内部落 flash 出文本段，都不能算。
func TestServedModelDowngraded(t *testing.T) {
	argon := Models["gemini-4-argon"]
	flash := Models["gemini-3.8-flash"]
	lite := Models["gemini-3.5-flash-lite"]
	image := Models["gemini-image"]

	cases := []struct {
		mc    ModelConfig
		served string
		want  bool
		note  string
	}{
		{argon, "3.5 Flash-Lite", true, "argon 被降到 lite：必须报"},
		{flash, "3.5 Flash-Lite", true, "3.8-flash 被降到 lite：必须报"},
		{flash, "3.8 Flash", false, "正常服务：不报"},
		{argon, "4 Argon", false, "正常服务：不报"},
		{argon, "", false, "取不到模型名：不误报"},
		{lite, "3.5 Flash-Lite", false, "请求的就是 lite：不算降级"},
		{image, "3.5 Flash-Lite", false, "媒体管线内部落 flash：不算降级"},
	}
	for _, c := range cases {
		if got := servedModelDowngraded(c.mc, c.served); got != c.want {
			t.Errorf("%s: servedModelDowngraded(%s, %q) = %v, 要 %v", c.note, c.mc.Desc[:12], c.served, got, c.want)
		}
	}
}

// Argon 的默认思考档必须是「中等」（不开思考），高档只能显式用 -thinking 选。
//
// 2026-10-17 曾经把默认改成 High（inner[80]=2），结果 Argon 延迟从 14-30s 变成
// 3.7s~477s 的剧烈波动，慢的那些撞超时变成 502（2026-10-23 实测）。这条断言把
// 「默认档」钉死，防止再次被顺手改掉。
func TestArgonDefaultThinkingTier(t *testing.T) {
	plain, ok := Models["gemini-4-argon"]
	if !ok {
		t.Fatal("没有 gemini-4-argon")
	}
	if plain.Thinking {
		t.Error("gemini-4-argon 默认开了高档思考 —— 会把延迟推到几百秒并撞超时；高档请用 gemini-4-argon-thinking")
	}
	high, ok := Models["gemini-4-argon-thinking"]
	if !ok {
		t.Fatal("没有 gemini-4-argon-thinking")
	}
	if !high.Thinking {
		t.Error("gemini-4-argon-thinking 应该开高档思考")
	}
	if plain.HexID != high.HexID || plain.Mode != high.Mode {
		t.Error("Argon 两档的模型选择（hex/mode）应该一致，只差思考档")
	}
}
