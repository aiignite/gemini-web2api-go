/**
 * gemini-web2api-go 后端客户端。
 *
 * 只依赖 Node 内置 fetch / ReadableStream，不引入任何依赖。负责：
 *   - 健康检查与模型清单（`GET /`、`GET /v1/models`）
 *   - OpenAI 形状的流式对话（`POST /v1/chat/completions`，SSE）
 *
 * SSE 解析按字节喂入、按 UTF-8 边界切行：Gemini 网页端吐中文时一个 chunk
 * 常在多字节字符中间断开，直接 toString 会产生乱码。
 */

/** 单次请求超时（毫秒）。后端自身默认 180s，这里留一点余量。 */
const DEFAULT_TIMEOUT_MS = 200_000

/**
 * 把 ReadableStream 按行喂给 onLine。
 *
 * `onLine` 返回 false 时停止消费（用于 `[DONE]` 后立即收尾）。
 *
 * @param {ReadableStream<Uint8Array>} stream
 * @param {(line: string) => boolean|void} onLine
 */
async function forEachLine(stream, onLine) {
  const decoder = new TextDecoder('utf-8')
  let buffer = ''
  let open = true

  for await (const chunk of stream) {
    buffer += decoder.decode(chunk, { stream: true })
    let index
    while ((index = buffer.indexOf('\n')) !== -1) {
      const line = buffer.slice(0, index).replace(/\r$/, '')
      buffer = buffer.slice(index + 1)
      if (line.length > 0 && onLine(line) === false) {
        open = false
        break
      }
    }
    if (!open) break
  }

  if (open) {
    const tail = buffer + decoder.decode()
    const line = tail.replace(/\r$/, '')
    if (line.length > 0) onLine(line)
  }
}

/** 后端返回的错误体 → 带 code 的 Error。 */
function backendError(status, body) {
  let message = `后端返回 HTTP ${status}`
  let code = 'BACKEND_ERROR'
  try {
    const parsed = JSON.parse(body)
    if (typeof parsed?.error?.message === 'string') message = parsed.error.message
    else if (typeof parsed?.error === 'string') message = parsed.error
    if (typeof parsed?.error?.code === 'string') code = parsed.error.code
  } catch { /* 非 JSON 错误体，沿用默认文案 */ }
  const error = new Error(message)
  error.code = code
  error.status = status
  return error
}

export class GeminiWeb2ApiClient {
  /**
   * @param {object} options
   * @param {string} options.baseUrl 后端根地址，如 http://127.0.0.1:8083
   * @param {string} options.apiKey  Bearer 密钥
   * @param {number} [options.timeoutMs]
   */
  constructor({ baseUrl, apiKey, timeoutMs = DEFAULT_TIMEOUT_MS }) {
    this.baseUrl = String(baseUrl || '').replace(/\/+$/, '')
    this.apiKey = String(apiKey || '')
    this.timeoutMs = timeoutMs
  }

  #headers() {
    const headers = { 'content-type': 'application/json' }
    if (this.apiKey) headers.authorization = `Bearer ${this.apiKey}`
    return headers
  }

  /** 健康检查：返回 { status, version, models }，后端不可用时抛错。 */
  async health(signal) {
    const response = await fetch(`${this.baseUrl}/`, { signal })
    if (!response.ok) throw backendError(response.status, await response.text())
    return response.json()
  }

  /**
   * 后端实际暴露的模型清单。
   *
   * 后端按 cookie 状态决定放行哪些模型（匿名 2 个 / 登录后 17 个），所以这里必须
   * 动态读取、写死就会在登录后少一堆。返回归一化条目，登录后无需重启即可变长。
   */
  async models(signal) {
    const response = await fetch(`${this.baseUrl}/v1/models`, { headers: this.#headers(), signal })
    if (!response.ok) throw backendError(response.status, await response.text())
    const body = await response.json()
    const list = Array.isArray(body?.data) ? body.data : []
    return list
      .map(entry => ({
        id: String(entry?.id ?? ''),
        name: typeof entry?.name === 'string' && entry.name ? entry.name : String(entry?.id ?? ''),
        description: typeof entry?.description === 'string' ? entry.description : '',
      }))
      .filter(entry => entry.id)
  }

  /**
   * 流式对话。
   *
   * 后端对普通 chat 是真流式（上游每出一帧就转发），所以这里逐帧转成 DSH
   * 的 block 事件；带 tools 的请求后端会退化成收完再发，这对我们无影响。
   *
   * @param {object} options
   * @param {string} options.model
   * @param {Array<{role:string, content:string, tool_call_id?:string}>} options.messages
   * @param {AbortSignal} [options.signal]
   * @returns {Promise<{content:string, reasoning:string, usage:object|null, model:string}>}
   */
  async chat({ model, messages, signal }) {
    const controller = new AbortController()
    const onAbort = () => controller.abort(signal?.reason)
    if (signal) {
      if (signal.aborted) controller.abort(signal.reason)
      else signal.addEventListener('abort', onAbort, { once: true })
    }
    const timer = setTimeout(() => controller.abort(new Error('请求超时')), this.timeoutMs)
    timer.unref?.()

    let content = ''
    let reasoning = ''
    let usage = null
    let servedModel = model

    try {
      const response = await fetch(`${this.baseUrl}/v1/chat/completions`, {
        method: 'POST',
        headers: { ...this.#headers(), accept: 'text/event-stream' },
        body: JSON.stringify({
          model,
          messages,
          stream: true,
          // 后端默认不在流里附带 usage：只有显式要，它才在 finish_reason 之后
          // 补一个 choices 为空的 usage 帧。不带这句 usage 永远是 null。
          stream_options: { include_usage: true },
        }),
        signal: controller.signal,
      })

      if (!response.ok) throw backendError(response.status, await response.text())
      if (!response.body) throw new Error('后端没有返回可读流')

      await forEachLine(response.body, rawLine => {
        if (!rawLine.startsWith('data:')) return true
        const data = rawLine.slice(5).trim()
        if (data === '' || data === '[DONE]') return data === '[DONE]' ? false : true

        let frame
        try {
          frame = JSON.parse(data)
        } catch {
          return true // 半帧或心跳，忽略
        }

        if (typeof frame?.model === 'string' && frame.model) servedModel = frame.model

        const delta = frame?.choices?.[0]?.delta ?? frame?.choices?.[0]?.message
        if (typeof delta?.content === 'string') content += delta.content
        if (typeof delta?.reasoning_content === 'string') reasoning += delta.reasoning_content

        // usage 帧的 choices 是空数组，所以 delta 取不到，必须单独读 usage。
        if (frame?.usage && Number.isFinite(frame.usage.total_tokens ?? Infinity)) usage = frame.usage
        return true
      })
    } finally {
      clearTimeout(timer)
      if (signal) signal.removeEventListener('abort', onAbort)
    }

    return { content, reasoning, usage, model: servedModel }
  }
}
