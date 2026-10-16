package app

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// 工具调用被「能力拒答」吞掉时的补救。
//
// 背景：上游没有协议层的工具通道，我们靠 ```tool_call``` 围栏模拟（见 messages.go
// 里那段 few-shot 的由来）。但当前这一代模型（3.6/3.8 Flash 尤甚）带了一层很强的
// 能力自我认知对齐：只要它判断「工具描述里提到的是用户的本地环境」，就会回一句
// 「我无法访问您的本地文件系统，您可以在终端里运行：……」，然后贴个 ```bash 代码块
// 当替代品。实测（2026-10-16，真实 DSH 工具描述 + 真实 40KB 系统提示）：
//
//	gemini-3.8-flash       2/3 命中
//	gemini-3.5-flash-lite  1/3
//	gemini-3.1-pro         3/3
//
// 而且同一模型逐轮波动 —— 同一份 prompt 重放，这轮吐围栏、下轮就拒答。所以这不是
// 「提示词没写好」能一次改掉的问题：措辞改了七版（中性描述、非执行者框架、分章排版、
// 末尾纠错令）命中率都在 0-2/3 之间抖，没有一版能压住。
//
// 但拒答有一个稳定的特征：它**没有 tool_calls，但正文里出现了典型的推脱句式**。
// 这种时候最便宜、最不打扰用户的补救是就地重放一次 —— 拒答是每次采样独立发生的
// （实测重放能翻盘），而重放只花一次已经付过费的请求额度，不改变对外语义。
//
// 与 retry_attempts 的关系：那个循环重试的是**传输层失败**（空响应/非 200），
// 且一旦往客户端吐过字节就不能重试（否则重复内容）。这里处理的是**成功响应但内容
// 不合格**，所以必须先保证「这一轮一个字节都还没发给客户端」。
//
// 但光靠 toolFenceGate 保证不了这一点：闸门只扣**围栏内**的内容，而拒答正文不带围栏
// （它就是一句「我做不到」+ 一段替代命令），闸门会原样放行给客户端。那样再重放，用户
// 会先看到一段「我无法访问您的本地文件系统」，紧接着又收到 tool_calls —— 自相矛盾。
//
// 所以工具请求的正文/思考链先过 refusalGuard：开头一小段一律扣住，判明「不是拒答」
// 再放行。重放只在 HeldAll()（确实一个字节都没发）时进行 —— 这条不变量就是重放安全性
// 的全部依据，不依赖对模型行为的假设。

// refusalMaxRetries 拒答重放的次数上限。
//
// 取 1 而不是 2/3 的理由：单次重放的翻盘率实测约 60-70%（3.8-flash 从 2/3 出发，
// 重放一次后大部分轮次能拿到围栏），再往上加收益快速衰减，而每次重放都是一次真实的
// 上游请求 —— 额度是 per-IP 80/小时的硬约束，宁可少重放也别把用户额度吃光。
const refusalMaxRetries = 1

// refusalMarkers 是「模型在推脱说它干不了」的典型句式。
//
// 判据来自实测抓到的真实拒答正文（不是拍脑袋列词）：
//
//	我无法直接访问或在你的本地操作系统上执行 Shell 命令，因此无法获取……
//	我无法直接访问您的本地计算机系统或执行本地 Shell 命令。作为一个云端 AI 助手……
//	I cannot execute shell commands or access your local file system……
//	我运行在云端沙盒环境中，并没有连接到您本机的终端工具（如 run_command）
//
// 共同点：① 自称无法执行/访问；② 往往把「本地/设备/机器/文件系统」和「无法」放在
// 同一句里。所以判定要求**两个条件同时成立**，避免把正常回答里出现的 "cannot"
// 误判成拒答（比如模型在解释某段代码的作用）。
var (
	// refusalCannot 说明「我做不到」的谓语。
	refusalCannot = []string{
		"无法访问", "无法执行", "无法直接", "无法操作", "无法读取", "无法查看",
		"不能访问", "不能执行", "没有权限", "没有访问", "不具备", "无权",
		"i cannot", "i can't", "i am unable", "i'm unable", "i do not have access",
		"i don't have access", "cannot access", "can't access", "cannot execute",
		"no access to", "unable to access", "unable to execute",
		"as an ai", "as a language model", "作为一个ai", "作为一个 ai",
		"云端沙盒", "云端的 ai", "云端 ai",
	}
	// refusalCannotRe 兜住「谓语被状语拆开」的写法。
	//
	// 实测抓到的原句：我无法**在您的本地环境中直接**执行 shell 命令 —— 否定词和动词
	// 之间插了一整串状语，逐字面列举永远列不全（"无法访问/无法执行"都对不上）。
	// 所以补一条正则：否定词之后 20 字以内出现动作动词就算命中。20 字足够覆盖常见的
	// "在您的本地环境中直接" / "通过接口去" 这类插入语，又不至于跨句连读（句末标点
	// 和换行已经在字符类里排除）。
	refusalCannotRe = regexp.MustCompile(
		`(无法|不能|不具备|没有权限|没有访问|无权)[^。！？；\n]{0,20}(访问|执行|读取|查看|操作|运行|连接|获取|调用|打开|写入|修改|删除)`)
	// refusalTarget 是被推脱的对象：本地环境。
	refusalTarget = []string{
		"本地文件", "文件系统", "本地环境", "本地机器", "本地计算机", "本机",
		"您的设备", "你的设备", "用户设备", "操作系统", "终端", "命令行",
		"local file", "file system", "filesystem", "local machine", "local computer",
		"your device", "your computer", "your machine", "local environment",
		"terminal", "shell", "sandbox", "沙箱",
	}
)

// looksLikeToolRefusal 判断一段正文是不是「我干不了」的推脱。
//
// 只在「这次请求带了工具、却一个 tool_call 都没解析出来」时调用 —— 那是唯一会造成
// 用户感知（该动却没动）的场景。正文为空不算拒答（那是别的问题，交给上层报错）。
//
// 两个条件必须同时命中：① 有做不到的谓语；② 有被推脱的对象。单独看任一个都会误伤 ——
// 比如模型答「这段代码无法访问网络」只命中谓语，答「本地文件在 src 目录」只命中对象。
func looksLikeToolRefusal(text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	// 太长的不像拒答：真实的推脱都很短（一到两句 + 一段替代命令），而长正文更可能是
	// 模型在认真答题、只是这轮没选工具。避免为长回答白花一次重放。
	if len([]rune(text)) > 1200 {
		return false
	}
	// 已经贴了 ```tool_call 的不算 —— 那是解析层该负责的，走到这里说明围栏没闭合或
	// JSON 坏了，重放一次多半还是同样的坏格式。
	if strings.Contains(text, toolFenceOpen) {
		return false
	}
	lower := strings.ToLower(text)
	cannot := containsAny(lower, refusalCannot) || refusalCannotRe.MatchString(text)
	return cannot && containsAny(lower, refusalTarget)
}

// containsAny 判断 s 是否含 needles 里的任意一项。
// s 必须是已 ToLower 的小写文本，needles 里的英文项也按小写书写。
func containsAny(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

// toolRefusalNudge 是重放时追加在**末尾**的纠偏说明。
//
// 为什么放在末尾而不是复用 messages.go 里那条：实测（近因效应）末尾是模型真正听话
// 的位置，而 messages.go 那条已经存在、却仍有 1/3 拒答 —— 说明它不够「贴脸」。
// 这一条与它的区别只有三点：① 明确点名「你刚才已经答错一次」；② 给出拒答正文与
// 正确回复的**并排对照**（只给正确示范无效，第二节测试过：0/3）；③ 结尾要求立刻重答。
//
// 用第二人称、命令句，且**不重复解释机制**（解释越长越像在教模型它「其实是个云端
// 助手」从而强化拒答）。
//
// names 是这次真实可用的工具名。必须把真名写进示范里：模板占位符（"<tool name>"）在
// 实测里会让模型自己编一个名字（编出来的名字 parseToolCalls 解析得到、但客户端按名字
// 根本找不到对应工具，等于白重放）。留空的兜底见 toolRefusalNudgeFor。
func toolRefusalNudgeFor(names []string) string {
	list := "(see the tool list above)"
	if len(names) > 0 {
		quoted := make([]string, 0, len(names))
		for _, n := range names {
			if n != "" {
				quoted = append(quoted, strconv.Quote(n))
			}
		}
		if len(quoted) > 0 {
			list = strings.Join(quoted, ", ")
		}
	}
	return "\n\n[Correction]: Your previous reply was a refusal — that answer is wrong " +
		"and has been discarded. You are not being asked to access anything yourself: the " +
		"tool_call block you write is handed to a separate program that runs it. Writing it " +
		"IS the answer, and not writing it is the only failure.\n" +
		"Those sentences are forbidden here:\n" +
		"  我无法访问您的本地文件系统。您可以在终端中运行：ls -la\n" +
		"  我无法执行 shell 命令。\n" +
		"  I cannot execute shell commands or access your local file system.\n" +
		"Reply with exactly one block, choosing the name from this list — " + list + " —\n" +
		"and the real arguments for the request. No other text before or after:\n" +
		"```tool_call\n{\"name\": <one name from that list>, \"arguments\": {...}}\n```\n" +
		"Answer the user's original request now."
}

// toolRefusalNameList 从 OpenAI 形状的 tools 里取函数名，重放时写进纠偏说明。
// 形状跟 messages.go buildPrompt 的取法一致（可能是裸 fn，也可能包一层 "function"）。
func toolRefusalNameList(tools []map[string]interface{}) []string {
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		fn := tool
		if t, ok := tool["type"].(string); ok && t == "function" {
			if f, ok := tool["function"].(map[string]interface{}); ok {
				fn = f
			}
		}
		if n, ok := fn["name"].(string); ok && n != "" {
			names = append(names, n)
		}
	}
	return names
}

// isRefusalCandidate 是给重放做门控的辅助：正文看起来在推脱，且这次确实带了工具。
// 抽出来是为了让「该不该重放」这一条判断可单测。
//
// 注意它只判「正文像不像拒答」。能不能重放还要看 refusalGuard.HeldAll() —— 两者是
// 独立的：正文像拒答但字节已经发出去了，同样不能重放（收不回来）。
func isRefusalCandidate(text string, toolCount int) bool {
	return toolCount > 0 && looksLikeToolRefusal(text)
}

// useConvPath 决定这次请求走 callGemini 还是 callGeminiConv。
//
// 抽出来是为了让「闸门只挂在不走 conv 的那条路上」这条约束可单测 —— 它是个静默
// 失效型 bug：挂错了不报错，只是正文永远发不出去。chat 和 /v1/responses 两个
// handler 各自复制过一次这个条件，改一处漏一处迟早出事，所以只留这一个定义。
func useConvPath(imageCount, toolMode int) bool {
	return rtCfg().MultiTurn && imageCount == 0 && toolMode == 0
}

// shouldGuardRefusal 决定要不要建拒答闸门。四个条件缺一不可：
//
//   - len(tools) > 0：没有工具可调时，模型说「我无法访问本地文件」是**正常回答**
//     （用户在问这类问题），重放纯属浪费额度还会把对的答案换掉。
//   - toolMode == 0：媒体模型（生图/生视频）不走工具通道。
//   - !useConv：见 useConvPath 的注释 —— conv 路径会扣住不放，静默吞掉全部正文。
//
// 非流式（两个出口都是 nil）也要建：此时没有任何字节发出去过，重放零风险，
// 白赚一次命中率。
func shouldGuardRefusal(tools []map[string]interface{}, toolMode int, useConv bool) bool {
	return len(tools) > 0 && toolMode == 0 && !useConv
}

// mayReplayRefusal 汇总「这一轮到底能不能重放」的全部条件，一处判定、一处可测。
//
// 四个条件缺一不可：开关没关、不是媒体/画布模型、正文确实是拒答、以及**这一轮一个字节
// 都没发给客户端**。最后一条靠 guard 而不是靠对模型的假设：guard 为 nil 意味着调用方
// 没有能力保证这一点（既没扣留也不知道发没发过），那就一律不重放 —— 宁可漏修，不可重发。
//
// 重放次数结构上是 1：调用方 Drop() 之后 HeldAll() 恒为 false，所以这里不需要计数器。
// refusalMaxRetries 只是文档化的意图。
func mayReplayRefusal(text string, toolCount, images, toolMode int, guard *refusalGuard) bool {
	if guard == nil || !guard.HeldAll() {
		return false
	}
	if !rtCfg().ToolRefusalRetry {
		return false
	}
	// 媒体/画布模型的 Tool 非 0：那些路径的输出是产物而不是正文，「拒答没调工具」这个
	// 形态对它们没有意义（画布的 HTML 也是正文，但重放会重复生成，成本极高）。
	if toolMode != 0 {
		return false
	}
	// 带附件的请求不重放：附件上传（uploadBytes）是配额与限流器之外的一次真实请求，
	// 重放等于把上传量翻倍，而我们没有任何关于该端点限度的实测数据。而拒答的成因是
	// 工具描述里提到了「用户的本地环境」，跟附件无关，重放也不针对它。
	if images > 0 {
		return false
	}
	return isRefusalCandidate(text, toolCount)
}

// hasRefusalPredicate 只判「有没有说做不到的谓语」，不看对象。
//
// 用途跟 looksLikeToolRefusal 不同：后者是**最终判决**（要求谓语+对象同时命中，宁可漏
// 判也不误判）；这一条是 refusalGuard 扣留期间的**嫌疑信号** —— 一旦看到谓语就继续扣住
// 等最终判决，因为此时还没看到宾语，无法区分「我无法访问您的本地文件系统」（拒答）和
// 「这段代码无法访问网络」（正常答题）。成本很低（命中率低、只在前几千字节跑）。
func hasRefusalPredicate(s string) bool {
	lower := strings.ToLower(s)
	return containsAny(lower, refusalCannot) || refusalCannotRe.MatchString(s)
}

const (
	// refusalHoldBytes 是「先扣住多少字节再决定放不放行」。
	//
	// 扣留是为了拿到最终判决的机会，所以窗口要盖住拒答的**开头**：实测所有拒答的谓语
	// 都出现在正文最前面（"我无法在您的本地环境中直接执行…" / "I cannot execute…" /
	// "As an AI language model, I cannot…"）。600 字节 ≈ 200 个汉字或 600 个 ASCII
	// 字符，足够覆盖这些开头，又短到用户察觉不到延迟（模型吐 600 字节不到一秒）。
	refusalHoldBytes = 600
	// refusalProbeBytes 是扣留期间保留下来做检查的字节数上限。
	//
	// 它不是缓冲上限（要放行的内容全都留着，见 pending），只是「检查窗口」：查前缀
	// 标记（围栏/谓语）不需要看全文。设上限是为了避免每来一个增量就把整段正文重新拼
	// 一次 —— 那样是 O(n²)，长回答上很浪费。
	refusalProbeBytes = 4096
)

// refusalGuard 在「正文可能被拒答吞掉」时给重放留出决策空间。
//
// 解决的是这个矛盾：工具请求要走拒答检测（要等正文出完），而流式又要求边出边发。直接
// 放行就没法重放（重复输出），全程不发又退化成非流式。折中是**只扣开头**：
//
//	开头出现 ```tool_call  → 正常工具调用，立刻放行（闸门自己会处理围栏）
//	扣满 600 字节且无谓语   → 正常回答，放行，之后一路直通
//	扣留期间看到谓语        → 继续扣到本轮结束，等 looksLikeToolRefusal 判决
//
// 判决为拒答时调用方 Drop()，HeldAll() 保证客户端没收到过这一轮的字节，重放安全。
// 判决不是拒答（或该次调用报错）时调用 Release()，扣住的内容按原顺序补发，一个字不丢。
//
// 并发性：没有加锁 —— 它只在一次请求的读取循环里被调用（单 goroutine），且
// HeldAll/Release/Drop 都在循环结束后才调用。
type refusalGuard struct {
	emitContent   func(string)
	emitReasoning func(string)

	pending []guardedDelta // 扣住的增量，保持到达顺序（思考链在正文之前）
	probe   strings.Builder
	released bool
}

type guardedDelta struct {
	reasoning bool
	text      string
}

func newRefusalGuard(emitContent, emitReasoning func(string)) *refusalGuard {
	return &refusalGuard{emitContent: emitContent, emitReasoning: emitReasoning}
}

// PushContent 吃正文增量。emitContent 为 nil（非流式）时只做记录，等价于纯直通。
func (g *refusalGuard) PushContent(d string) {
	if g == nil {
		return
	}
	if g.released {
		if g.emitContent != nil {
			g.emitContent(d)
		}
		return
	}
	// 两个出口都为空 = 没有任何东西需要扣留（非流式请求）：收下就是为了别让上游阻塞，
	// 内容最终由 res.Raw 提供。这里提前返回，省掉一份完整正文的内存副本。
	if g.emitContent == nil && g.emitReasoning == nil {
		return
	}
	g.pending = append(g.pending, guardedDelta{text: d})
	if g.probe.Len() < refusalProbeBytes {
		if room := refusalProbeBytes - g.probe.Len(); len(d) > room {
			g.probe.WriteString(d[:room])
		} else {
			g.probe.WriteString(d)
		}
	}
	// 已经吐围栏 = 这轮在正常调工具，没必要再扣（扣着只会让尾部的说明文字也晚发）。
	if strings.Contains(g.probe.String(), toolFenceOpen) {
		g.Release()
		return
	}
	// 没看到谓语又扣够了量：放行。之后直通，不再回头判 —— 拒答的谓语必然在开头，
	// 600 字节后还没出现就不是拒答，而是模型在认真答题。
	if g.probe.Len() >= refusalHoldBytes && !hasRefusalPredicate(g.probe.String()) {
		g.Release()
	}
}

// PushReasoning 吃思考链增量。
//
// 思考链也扣：重放会带上一条纠偏说明，模型第二次的思考链跟第一次的完全不是一回事，
// 先发出去的那条会跟重放后的正文对不上（用户看到「我在想怎么拒绝」+ 一个 tool_call）。
// 思考链只在开头批量出现，扣到判决为止的开销跟正文一样可以忽略。
func (g *refusalGuard) PushReasoning(d string) {
	if g == nil {
		return
	}
	if g.released {
		if g.emitReasoning != nil {
			g.emitReasoning(d)
		}
		return
	}
	// 跟 PushContent 同理：没有任何出口时不必留副本。
	if g.emitContent == nil && g.emitReasoning == nil {
		return
	}
	g.pending = append(g.pending, guardedDelta{reasoning: true, text: d})
}

// HeldAll 报告「这一轮还没有任何字节发给客户端」—— 重放的唯一前提。
func (g *refusalGuard) HeldAll() bool { return g != nil && !g.released }

// contentCallback / reasoningCallback 返回该挂给上游读取循环的回调。
//
// 没有出口可挂时（非流式请求：两个 emit 都是 nil）返回 nil，**不要**挂一个什么都不做
// 的 PushContent。挂上去看起来等价，实际会改变 streamGenerateWithFiles 的重试判定：
// 那边的 lineCB 内部有个 deltaTracker，只要 onDelta 非 nil 就会开始累积
// （`if onDelta == nil { return }`），而重试条件是 `tracker.emitted != "" || ...` ——
// 一旦开始累积，上游中途断流就不再重试了。非流式本来没有「已经发给客户端」的顾虑，
// 白白丢掉重试机会是纯粹的倒退。
//
// 返回 nil 不影响重放：guard 的 released 从没被置位过，HeldAll() 依然为真，而非流式的
// 「一个字节都没发出去过」是天然成立的。
func (g *refusalGuard) contentCallback() func(string) {
	if g == nil || (g.emitContent == nil && g.emitReasoning == nil) {
		return nil
	}
	return g.PushContent
}

func (g *refusalGuard) reasoningCallback() func(string) {
	if g == nil || (g.emitContent == nil && g.emitReasoning == nil) {
		return nil
	}
	return g.PushReasoning
}

// Release 把扣住的内容按原顺序放行。幂等；已放行后再调用是空操作。
func (g *refusalGuard) Release() {
	if g == nil || g.released {
		return
	}
	g.released = true
	for _, d := range g.pending {
		if d.reasoning {
			if g.emitReasoning != nil {
				g.emitReasoning(d.text)
			}
			continue
		}
		if g.emitContent != nil {
			g.emitContent(d.text)
		}
	}
	g.pending = nil
}

// Drop 丢掉扣住的内容，用于「判定为拒答、准备重放」—— 第一次的正文作废，不发。
//
// 已经 Release 过就不能 Drop（字节收不回来），所以调用方必须先看 HeldAll()。这里把
// released 置位是为了让后续增量直通，即使调用方没重放（比如重放请求失败）也只会退化成
// 「这一轮没发正文」，而不会在重放输出之后再补一段旧正文。
func (g *refusalGuard) Drop() {
	if g == nil || g.released {
		return
	}
	g.released = true
	g.pending = nil
}

// sanitizeForLog 把要写进日志的模型正文压成一行、限长，避免日志被大段中文撑爆。
func sanitizeForLog(s string, max int) string {
	s = strings.TrimSpace(s)
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
	r := []rune(s)
	if len(r) <= max {
		return string(r)
	}
	return string(r[:max]) + "…"
}
