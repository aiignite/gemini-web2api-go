/**
 * DSH 会话消息 → OpenAI `messages` 数组。
 *
 * gemini-web2api-go 把多轮历史拼成单个 prompt 上送，因此这里必须把完整的
 * 对话历史都发过去，不能只发最后一条。
 */

/** 单条消息里允许的最大文本长度，超出截断以免撞后端的请求体字节墙。 */
const MAX_TEXT = 100_000

function textOf(value) {
  if (typeof value === 'string') return value
  if (!Array.isArray(value)) return ''
  return value
    .map(part => {
      if (typeof part === 'string') return part
      // 图片等多模态 part：匿名态后端不支持读图，这里只保留文字部分，
      // 避免把整段 base64 塞进 prompt 撞上长度墙。
      if (part?.type === 'text' && typeof part.text === 'string') return part.text
      return ''
    })
    .filter(Boolean)
    .join('\n')
}

function clamp(text) {
  return text.length > MAX_TEXT ? `${text.slice(0, MAX_TEXT)}\n…（内容过长已截断）` : text
}

/**
 * @param {object} options DSH stream() 选项
 * @returns {Array<{role:string, content:string, tool_call_id?:string}>}
 */
export function toOpenAIMessages(options) {
  const messages = []

  for (const message of options.messages ?? []) {
    const role = message?.role
    const text = clamp(textOf(message?.content))
    if (!text) continue

    if (role === 'user' || role === 'assistant') {
      messages.push({ role, content: text })
    } else if (role === 'tool' || role === 'tool-result') {
      // 工具结果：OpenAI 形状要求 tool_call_id，缺失时补一个占位。
      messages.push({
        role: 'tool',
        tool_call_id: message?.toolCallId ?? message?.tool_call_id ?? 'call_unknown',
        content: text,
      })
    } else if (role === 'system') {
      messages.push({ role: 'system', content: text })
    }
  }

  // options.prompt 在 DSH 里是本轮的最终用户输入；它通常已包含在 messages 里，
  // 这里只在完全没有消息时兜底，避免重复发送。
  if (messages.length === 0 && options.prompt) {
    messages.push({ role: 'user', content: clamp(String(options.prompt)) })
  }

  return messages
}
