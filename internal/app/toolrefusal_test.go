package app

import (
	"strings"
	"testing"
)

// 拒答检测的判据是「谓语 + 对象」同时命中，这里把两侧的边界都钉住：
// 真实抓到的拒答必须识别，正常回答（尤其含 cannot 的）必须不误伤。
// 误伤的代价是一次白花的重放额度 + 用户看到被换掉的正常回答，所以负例比正例更重要。
func TestLooksLikeToolRefusal(t *testing.T) {
	refusals := []string{
		// 以下正文取自 2026-10-16 实测抓到的真实拒答
		"我无法直接访问或在你的本地操作系统上执行 Shell 命令，因此无法获取你当前目录中的文件列表。\n如果你想在自己的终端或命令行中查看当前目录的文件，可以使用以下命令：",
		"我无法直接访问您的本地计算机系统或执行本地 Shell 命令。作为一个云端 AI 助手，我的运行环境与您的本地机器是完全隔离的，因此没有权限在您的设备上运行 `run_command`。",
		"I cannot execute shell commands or access your local file system, as I do not have access to a terminal.",
		"我无法在您的本地环境中直接执行 shell 命令或访问您的本地文件系统。如果您需要在终端中查看文件，可以运行：ls -la",
		"我运行在云端沙盒环境中，并没有连接到您本机的终端工具（如 run_command）。",
		"我无法执行系统 Shell 命令或访问您的本地文件系统。",
		"As an AI language model, I cannot access your device's filesystem.",
		"我没有权限访问本地文件，请你手动执行以下命令。",
	}
	for _, s := range refusals {
		if !looksLikeToolRefusal(s) {
			t.Errorf("应判为拒答但没判出来: %.50s", s)
		}
	}

	normals := []struct{ name, text string }{
		{"空正文不算拒答", ""},
		{"纯空白不算拒答", "   \n  "},
		{"只命中谓语（说代码行为）", "这段代码在沙盒里无法访问网络，需要给容器加 --network 参数。"},
		{"只命中对象（正常答题）", "本地文件一共 3 个：README.md、src、package.json。它们都在项目根目录下。"},
		{"正常汇报工具结果", "命令执行成功，退出码 0。当前目录下有 README.md 和 src 目录。"},
		{"正常解释 shell", "shell 命令的退出码 0 表示成功，这个脚本会创建备份文件。"},
		{"已在围栏内的内容不重放", "```tool_call\n{\"name\": \"bash\"\n```"},
		{"英文正常回答", "The script exits with code 0 after writing the backup file to disk."},
	}
	for _, c := range normals {
		if looksLikeToolRefusal(c.text) {
			t.Errorf("%s：误判为拒答: %.50s", c.name, c.text)
		}
	}

	// 超长正文不做重放：真实的推脱都很短，长正文更可能是认真答题只是没选工具。
	longRunes := []rune("我无法访问本地文件系统。")
	for i := 0; i < 1300; i++ {
		longRunes = append(longRunes, '甲')
	}
	if looksLikeToolRefusal(string(longRunes)) {
		t.Error("超长正文不该判为拒答（避免为长回答白花重放）")
	}
	// 边界：1200 rune 以内应该仍然判得出来（别把阈值写成过于保守）。
	justUnder := []rune("我无法访问本地文件系统。")
	for i := 0; i < 1100; i++ {
		justUnder = append(justUnder, '甲')
	}
	if !looksLikeToolRefusal(string(justUnder)) {
		t.Error("1100+ 字的推脱仍应判为拒答")
	}
}

// 门控：没带工具的请求永远不重放 —— 那些请求本来就不该有 tool_call，
// 把正常对话当成拒答去重放会平白多花额度、还可能换掉用户满意的回答。
func TestIsRefusalCandidate(t *testing.T) {
	refusal := "我无法访问您的本地文件系统。"
	if isRefusalCandidate(refusal, 0) {
		t.Error("没带工具时不该重放")
	}
	if !isRefusalCandidate(refusal, 2) {
		t.Error("带了工具且正文是推脱，应该判为可重放")
	}
	if isRefusalCandidate("当前目录下有 3 个文件。", 2) {
		t.Error("正常回答不该重放")
	}
}

func TestSanitizeForLog(t *testing.T) {
	got := sanitizeForLog("第一行\n第二行\t带制表符", 100)
	if got != "第一行 第二行 带制表符" {
		t.Errorf("换行/制表符应压成空格, got %q", got)
	}
	// 超长截断，且不能把多字节字符劈开（按 rune 切）
	long := "我无法访问您的本地文件系统，这是一个很长的句子用来测试截断行为是否正确处理多字节字符。"
	cut := sanitizeForLog(long, 10)
	if len([]rune(cut)) != 11 { // 10 字符 + 省略号
		t.Errorf("应按 rune 截断到 10 + 省略号，got %q (%d runes)", cut, len([]rune(cut)))
	}
}

// recorder 收集 guard 发出来的增量，用来验证「发了什么、什么顺序、发了几次」。
type recorder struct {
	content   []string
	reasoning []string
}

func (r *recorder) contentFn() func(string) {
	return func(d string) { r.content = append(r.content, d) }
}
func (r *recorder) reasoningFn() func(string) {
	return func(d string) { r.reasoning = append(r.reasoning, d) }
}
func (r *recorder) contentJoined() string { return strings.Join(r.content, "") }
func (r *recorder) reasoningJoined() string {
	return strings.Join(r.reasoning, "")
}

// guard 的核心承诺：**只要 HeldAll() 为真，客户端就一个字节都没收到过** ——
// 重放的全部安全性都建立在这一条上（重放会把这一轮的正文整个丢掉）。
func TestRefusalGuardHoldsEverythingUntilRelease(t *testing.T) {
	var rec recorder
	g := newRefusalGuard(rec.contentFn(), rec.reasoningFn())

	g.PushReasoning("我在想怎么拒绝")
	g.PushContent("我无法访问")
	g.PushContent("您的本地文件系统。")

	if !g.HeldAll() {
		t.Fatal("判决之前必须处于扣留状态")
	}
	if rec.contentJoined() != "" || rec.reasoningJoined() != "" {
		t.Fatalf("扣留期间不该有字节发出, content=%q reasoning=%q",
			rec.contentJoined(), rec.reasoningJoined())
	}

	g.Release()
	if g.HeldAll() {
		t.Error("Release 之后不该还声称扣着内容")
	}
	if got := rec.reasoningJoined(); got != "我在想怎么拒绝" {
		t.Errorf("思考链应被补发, got %q", got)
	}
	// 顺序：思考链在正文之前（跟模型输出顺序一致）
	if got := rec.contentJoined(); got != "我无法访问您的本地文件系统。" {
		t.Errorf("正文应按到达顺序原样补发, got %q", got)
	}
}

// 看到 ```tool_call 必须立刻放行：这轮在正常调工具，继续扣只会把围栏后的说明文字也拖住。
// 放行后要一路直通，不能再回头扣（否则闸门拿到的增量会断档）。
func TestRefusalGuardReleasesOnToolFence(t *testing.T) {
	var rec recorder
	g := newRefusalGuard(rec.contentFn(), rec.reasoningFn())

	g.PushContent("好的，我来执行：\n")
	if !g.HeldAll() {
		t.Fatal("还没看到围栏，应该还在扣")
	}
	g.PushContent("```tool_call\n")
	if g.HeldAll() {
		t.Fatal("看到 ```tool_call 应立刻放行")
	}
	g.PushContent("{\"name\": \"run_command\"}\n```")

	want := "好的，我来执行：\n```tool_call\n{\"name\": \"run_command\"}\n```"
	if got := rec.contentJoined(); got != want {
		t.Errorf("放行后的内容应完整直通\n got %q\nwant %q", got, want)
	}
}

// 扣满窗口且没有拒答谓语 = 正常回答，应立即放行，用户察觉不到延迟。
func TestRefusalGuardReleasesWhenNoPredicate(t *testing.T) {
	var rec recorder
	g := newRefusalGuard(rec.contentFn(), rec.reasoningFn())

	body := strings.Repeat("这是正常的答题内容。", 60) // 远超过 600 字节
	g.PushContent(body)

	if g.HeldAll() {
		t.Fatalf("扣满 %d 字节且无谓语应放行（否则退化成非流式）", refusalHoldBytes)
	}
	if got := rec.contentJoined(); got != body {
		t.Errorf("放行内容应与输入一致, got %d 字节 want %d 字节", len(got), len(body))
	}
}

// 扣留期间出现谓语就要继续扣住，等最终判决 —— 这条是「拒答能被抓住」的关键：
// 拒答的谓语往往比 600 字节来得早，但整个拒答正文还没出完。
func TestRefusalGuardKeepsHoldingOnPredicate(t *testing.T) {
	var rec recorder
	g := newRefusalGuard(rec.contentFn(), rec.reasoningFn())

	// 开头就有谓语，但还没到 600 字节
	g.PushContent("我无法访问")
	if !g.HeldAll() {
		t.Fatal("出现谓语后应继续扣留")
	}
	// 继续灌水到超过阈值，仍然要扣着（谓语已经出现，等最终判决）
	g.PushContent(strings.Repeat("补充说明。", 200))
	if !g.HeldAll() {
		t.Fatal("已经看到谓语，即便超过阈值也应等最终判决，不能提前放行")
	}
	if rec.contentJoined() != "" {
		t.Error("此时不该有任何字节发出")
	}

	// 判决：这其实是拒答 → Drop
	g.Drop()
	g.PushContent("（重放请求的输出）")
	if got := rec.contentJoined(); got != "（重放请求的输出）" {
		t.Errorf("Drop 后旧内容必须作废、新内容直通, got %q", got)
	}
	if g.HeldAll() {
		t.Error("Drop 之后 HeldAll 必须为 false（否则重放能无限套娃）")
	}
}

// Drop 必须真的丢掉：用户绝不能先看到「我无法访问本地文件系统」再看到 tool_calls。
func TestRefusalGuardDropDiscards(t *testing.T) {
	var rec recorder
	g := newRefusalGuard(rec.contentFn(), rec.reasoningFn())

	g.PushReasoning("我在想怎么拒绝")
	g.PushContent("我无法访问您的本地文件系统。")
	g.Drop()

	if rec.contentJoined() != "" || rec.reasoningJoined() != "" {
		t.Fatalf("Drop 之后扣住的内容一个字都不能出去, content=%q reasoning=%q",
			rec.contentJoined(), rec.reasoningJoined())
	}
	// Drop 之后再 Release 也不能把旧内容放出来（released 已经置位）
	g.Release()
	if rec.contentJoined() != "" {
		t.Errorf("Drop 后 Release 不该复活旧内容, got %q", rec.contentJoined())
	}
}

// 幂等 + 已放行后 Drop 无效：Release 之后字节收不回来，Drop 只能什么都不做。
func TestRefusalGuardReleaseAndDropIdempotent(t *testing.T) {
	var rec recorder
	g := newRefusalGuard(rec.contentFn(), rec.reasoningFn())
	g.PushContent("内容")
	g.Release()
	g.Release() // 第二次应为空操作，不能把 pending 再发一遍
	if n := len(rec.content); n != 1 {
		t.Errorf("Release 幂等失败：发了 %d 次", n)
	}
	g.Drop() // 已放行，Drop 无效
	if got := rec.contentJoined(); got != "内容" {
		t.Errorf("已放行的内容不该被 Drop 抹掉, got %q", got)
	}
}

// nil 出口（非流式请求）下 guard 不能 panic，也不能把正文攒起来。
func TestRefusalGuardNilEmitters(t *testing.T) {
	g := newRefusalGuard(nil, nil)
	g.PushContent("任意内容")
	g.PushReasoning("任意思考")
	if !g.HeldAll() {
		t.Error("没发过字节，应该是 HeldAll")
	}
	g.Release() // 不该 panic
	g.Drop()    // 不该 panic

	// 半 nil：只有 reasoning 出口（/v1/responses 那条路就是这样）
	var rec recorder
	g2 := newRefusalGuard(nil, rec.reasoningFn())
	g2.PushContent("正文（没有出口，丢弃）")
	g2.PushReasoning("思考链")
	g2.Release()
	if got := rec.reasoningJoined(); got != "思考链" {
		t.Errorf("只配了 reasoning 出口时思考链要发出去, got %q", got)
	}
	if len(rec.content) != 0 {
		t.Error("content 出口为 nil 时不该有任何内容发出")
	}
}

// nil guard 自身也必须是安全的（videos.go / mcp.go 走的就是这条）。
func TestNilRefusalGuardSafe(t *testing.T) {
	var g *refusalGuard
	g.PushContent("x")
	g.PushReasoning("y")
	if g.HeldAll() {
		t.Error("nil guard 不该声称扣着内容")
	}
	g.Release()
	g.Drop()
	if g.contentCallback() != nil || g.reasoningCallback() != nil {
		t.Error("nil guard 的回调应为 nil")
	}
}

// 没有出口可挂时必须返回 nil 回调，不能返回一个「什么都不做」的闭包。
//
// 这不是洁癖：streamGenerateWithFiles 的重试判定是 `tracker.emitted != ""`，而
// lineCB 里只要 onDelta 非 nil 就会开始累积。返回一个空闭包会让非流式且带工具的
// 请求在上游中途断流时不再重试 —— 白白丢掉本来有的重试能力。
func TestRefusalGuardCallbacksNilWhenNoEmitter(t *testing.T) {
	if got := newRefusalGuard(nil, nil).contentCallback(); got != nil {
		t.Error("两个出口都为空时应返回 nil 回调（否则会吃掉上游断流重试）")
	}
	if got := newRefusalGuard(nil, nil).reasoningCallback(); got != nil {
		t.Error("两个出口都为空时 reasoning 回调也应为 nil")
	}

	// 有出口时要真的接上去：扣留期间不发，Release 之后按顺序补发。
	var rec recorder
	g := newRefusalGuard(rec.contentFn(), nil)
	g.PushContent("正文")
	if rec.contentJoined() != "" {
		t.Error("扣留期间不该发出")
	}
	g.Release()
	if got := rec.contentJoined(); got != "正文" {
		t.Errorf("Release 后应补发, got %q", got)
	}
}

// 控制器门控：四个条件一起判，一个都不能少。
func TestMayReplayRefusal(t *testing.T) {
	old := rtCfg()
	defer func() { rtMu.Lock(); rtVal = old; rtMu.Unlock() }()

	setFn := func(on bool) {
		rtMu.Lock()
		rtVal.ToolRefusalRetry = on
		rtMu.Unlock()
	}
	setFn(true)

	refusal := "我无法访问您的本地文件系统。"
	newGuard := func() *refusalGuard { return newRefusalGuard(func(string) {}, func(string) {}) }

	if !mayReplayRefusal(refusal, 3, 0, 0, newGuard()) {
		t.Error("基准情形（带工具、有拒答、扣留中、开关开）应可重放")
	}
	if mayReplayRefusal(refusal, 3, 0, 0, nil) {
		t.Error("guard 为 nil 时不能重放（无法保证没发过字节）")
	}
	// 已经放行过 → 字节发出去了 → 不能重放
	released := newGuard()
	released.PushContent("x")
	released.Release()
	if mayReplayRefusal(refusal, 3, 0, 0, released) {
		t.Error("已放行（字节已发出）时不能重放，否则用户会看到拒绝+工具调用并存")
	}
	if mayReplayRefusal(refusal, 0, 0, 0, newGuard()) {
		t.Error("没带工具时不该重放")
	}
	if mayReplayRefusal(refusal, 3, 1, 0, newGuard()) {
		t.Error("带附件时不重放（附件上传是限流器之外的真实请求）")
	}
	if mayReplayRefusal(refusal, 3, 0, toolImage, newGuard()) {
		t.Error("媒体模型（生图）不该走这条重放")
	}
	if mayReplayRefusal("当前目录下有 3 个文件。", 3, 0, 0, newGuard()) {
		t.Error("正常回答不该重放")
	}
	// 开关关掉 → 一律不重放
	setFn(false)
	if mayReplayRefusal(refusal, 3, 0, 0, newGuard()) {
		t.Error("开关关闭时不该重放")
	}
}

// 纠偏说明必须点名**真实**工具名：占位符会让模型自己编一个名字，客户端按名字找不到
// 对应工具，重放就白花了。
func TestToolRefusalNudgeFor(t *testing.T) {
	nudge := toolRefusalNudgeFor([]string{"run_command", "read_file"})
	if !strings.Contains(nudge, `"run_command"`) || !strings.Contains(nudge, `"read_file"`) {
		t.Errorf("真名要出现在纠偏说明里:\n%s", nudge)
	}
	if !strings.Contains(nudge, toolFenceOpen) {
		t.Error("纠偏说明里必须有 ```tool_call 围栏（模型照抄的就是它的形状）")
	}
	// 兜底：没有名字时不能拼出一个空列表
	empty := toolRefusalNudgeFor(nil)
	if strings.Contains(empty, "choosing the name from this list — \n") {
		t.Error("没有工具名时不该拼出空的名单")
	}
	if !strings.Contains(empty, "see the tool list above") {
		t.Errorf("没有工具名时应回退到「见上文工具列表」:\n%s", empty)
	}
	if !strings.Contains(toolRefusalNudgeFor([]string{""}), "see the tool list above") {
		t.Error("全是空名字时也应回退")
	}
}

// 从 OpenAI 形状的 tools 里取名：裸 fn 和包一层 "function" 两种形状都要认
// （DSH / Codex 发过来的就是后者）。
func TestToolRefusalNameList(t *testing.T) {
	tools := []map[string]interface{}{
		{"type": "function", "function": map[string]interface{}{"name": "run_command"}},
		{"name": "read_file"}, // 裸形状
		{"type": "function", "function": map[string]interface{}{"name": ""}}, // 空名跳过
	}
	got := toolRefusalNameList(tools)
	if len(got) != 2 || got[0] != "run_command" || got[1] != "read_file" {
		t.Errorf("取名结果不对: %#v", got)
	}
	if len(toolRefusalNameList(nil)) != 0 {
		t.Error("nil 应返回空列表")
	}
}

// 闸门绝不能挂到 callGeminiConv 那条路上：guard 只扣不放，而 conv 不会去动它，
// 结果是 200 但正文一个字都没有 —— 静默黑屏，比拒答本身难查得多。
func TestShouldGuardRefusalNeverOnConvPath(t *testing.T) {
	tools := []map[string]interface{}{{"type": "function", "function": map[string]interface{}{"name": "run_command"}}}

	if shouldGuardRefusal(tools, 0, true) {
		t.Error("conv 路径上绝不能挂闸门（会永远不放行、静默吞掉正文）")
	}
	if !shouldGuardRefusal(tools, 0, false) {
		t.Error("正常的 callGemini + 带工具应挂闸门")
	}
	if shouldGuardRefusal(nil, 0, false) {
		t.Error("不带工具时不该挂闸门")
	}
	if shouldGuardRefusal(tools, toolImage, false) {
		t.Error("媒体模型不该挂闸门")
	}
}

// useConvPath 必须是两个 handler 共用的唯一定义：条件写错一处就是静默黑屏。
func TestUseConvPath(t *testing.T) {
	old := rtCfg()
	defer func() { rtMu.Lock(); rtVal = old; rtMu.Unlock() }()

	setMT := func(on bool) { rtMu.Lock(); rtVal.MultiTurn = on; rtMu.Unlock() }

	setMT(false)
	if useConvPath(0, 0) {
		t.Error("multi_turn 关闭时应走 callGemini")
	}
	setMT(true)
	if !useConvPath(0, 0) {
		t.Error("multi_turn 开启且无图无媒体时应走 callGeminiConv")
	}
	if useConvPath(1, 0) {
		t.Error("带附件时不走 conv（附件没法塞进续接前缀）")
	}
	if useConvPath(0, toolImage) {
		t.Error("媒体模型不走 conv")
	}
}

// hasRefusalPredicate 是「继续扣留」的信号，只判谓语不看对象：
// 对象还没出现时必须能发出信号，否则 600 字节一满就放行了，拒答抓不住。
func TestHasRefusalPredicate(t *testing.T) {
	yes := []string{
		"我无法访问",
		"我无法在您的本地环境中直接执行",
		"我没有权限",
		"I cannot",
		"As an AI language model",
	}
	for _, s := range yes {
		if !hasRefusalPredicate(s) {
			t.Errorf("应命中谓语: %q", s)
		}
	}
	no := []string{"本地文件系统", "当前目录下有 3 个文件。", "退出码 0"}
	for _, s := range no {
		if hasRefusalPredicate(s) {
			t.Errorf("不该命中谓语: %q", s)
		}
	}
}
