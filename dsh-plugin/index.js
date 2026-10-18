/**
 * Gemini 网页端 → DeepSeek Harness 原生 LlmAdapter。
 *
 * 本插件不自己出网：它把本机 gemini-web2api-go 暴露的 OpenAI 兼容接口
 * （`http://127.0.0.1:8083`）注册成一个 DSH provider。反代链路是
 *
 *     [DSH 模型选择器]
 *         ↓ http://127.0.0.1:8083/v1/chat/completions
 *     [gemini-web2api-go]
 *         ↓ 代理池里的 SOCKS5/HTTP 出口
 *     [gemini.google.com]
 *
 * 后端由它自己管理（进程、代理池、cookie 池、数据库），本插件只负责把
 * 对话转成 OpenAI 形状并把流式结果转成 DSH 的 block 事件。
 */

import { homedir } from 'node:os'
import { join } from 'node:path'

import { GeminiWeb2ApiClient } from './lib/client.js'
import { toOpenAIMessages } from './lib/messages.js'
import { BrowserLogin } from './lib/login.js'

/** Provider 路由名（模型选择器里的分组）。 */
const PROVIDER = 'gemini-web2api'
const PROVIDER_LABEL = 'Gemini 网页端'

/** 健康检查重试间隔；后端可能随用户手动重启而短暂不可用。 */
const HEALTH_MS = 60_000

/** 启动时等待后端就绪的最大尝试次数（每次间隔 2s，即最多约 1 分钟）。 */
const BOOT_ATTEMPTS = 30

/** cookie 自动同步间隔。15s 足够跟上 __Secure-1PSIDTS 的轮换，又不至于压后端。 */
const SYNC_MS = 15_000

const DEFAULT_MODELS = [
  { id: 'gemini-3.6-flash', name: 'Gemini 3.6 Flash', context: 1_000_000 },
  { id: 'gemini-3.5-flash-lite', name: 'Gemini 3.5 Flash Lite', context: 1_000_000 },
]

/**
 * 登录浏览器专用的 Chrome 配置目录。
 *
 * 与用户日常 Chrome 分开，避免插件往日常 profile 里塞东西、也避免用户开着 Chrome
 * 时参数被忽略。放在 DSH_HOME 下，与其它插件数据同处。
 */
function loginProfileDir() {
  const home = process.env.DSH_HOME || join(homedir(), '.dsh')
  return join(home, 'gemini-web2api', 'chrome-profile')
}

/**
 * 可变的模型目录。
 *
 * 后端按 cookie 状态放行不同数量的模型（匿名 2 个、登录后 17 个），而 DSH 的模型
 * 选择器读的是 listModels() 的返回值。所以这里持有一个可变数组：登录成功后原地
 * 替换，再广播一次注册，模型选择器就会自己多出可选项，无需重启 DSH。
 */
class ModelCatalog {
  #models

  constructor(models) {
    this.#models = models
  }

  get models() {
    return this.#models
  }

  /** 用后端返回的清单替换；无变化时返回 false，避免无谓的 UI 刷新。 */
  replace(models) {
    const before = this.#models.map(m => m.id).sort().join(',')
    const after = models.map(m => m.id).sort().join(',')
    this.#models = models
    return before !== after
  }

  find(id) {
    const wanted = String(id)
    return this.#models.find(entry => entry.id === wanted || entry.name === wanted)
  }
}

class GeminiWeb2ApiAdapter {
  #client
  #catalog
  #logger
  #status

  constructor({ client, catalog, logger, status }) {
    this.#client = client
    this.#catalog = catalog
    this.#logger = logger
    this.#status = status
  }

  providerInfo(provider) {
    return { id: provider, name: PROVIDER_LABEL }
  }

  providerRetryPolicy() {
    return undefined
  }

  imageRequestPricing() {
    return undefined
  }

  imageRequestTarget(_provider, _model, image) {
    return { width: image.width, height: image.height, maxBytes: image.bytes }
  }

  async listModels(provider) {
    return this.#catalog.models.map(model => ({
      provider,
      id: model.id,
      name: model.name || model.id,
      ...(model.context ? { description: `context ${model.context}` } : {}),
      // 后端对读图/生图类模型有专门工具，但本插件只走纯文本通道，统一标 text。
      inputModalities: ['text'],
    }))
  }

  async resolveModel(provider, model) {
    const entry = this.#catalog.find(model)
    return {
      provider,
      id: model,
      name: entry?.name || model,
      ...(entry?.context ? { context: { contextWindow: entry.context } } : {}),
    }
  }

  async prepareCall(provider, model, signal) {
    return {
      model: await this.resolveModel(provider, model, signal),
      stream: options => this.stream(options),
    }
  }

  async *#fail(message, code) {
    yield { type: 'finish', reason: { kind: 'error', failure: { message, code } } }
  }

  async *stream(options) {
    if (this.#status.phase !== 'ready') {
      yield* this.#fail(
        `gemini-web2api 后端尚未就绪（${this.#status.phase}）：${this.#status.lastError || '正在等待连接'}`,
        'BACKEND_NOT_READY',
      )
      return
    }

    if (options.signal?.aborted) {
      yield { type: 'finish', reason: { kind: 'aborted', failure: { message: '已取消', code: 'ABORTED' } } }
      return
    }

    const entry = this.#catalog.find(options.model)
    if (!entry) {
      yield* this.#fail(`模型不在后端当前可用清单中：${options.model}`, 'MODEL_NOT_FOUND')
      return
    }

    const messages = toOpenAIMessages(options)
    if (messages.length === 0) {
      yield* this.#fail('没有可发送的对话内容', 'EMPTY_CONVERSATION')
      return
    }

    // 逐帧转发：后端出一帧就往前端推一帧。攒完再发会让整个生成期间空白。
    let nextIndex = 0
    let reasonIndex = -1
    let textIndex = -1
    let reasoning = ''
    let content = ''
    let usage = null
    let servedModel = entry.id

    try {
      for await (const event of this.#client.stream({
        model: entry.id,
        messages,
        signal: options.signal,
      })) {
        if (event.type === 'reasoning') {
          if (reasonIndex < 0) {
            reasonIndex = nextIndex
            nextIndex += 1
            yield { type: 'block-start', index: reasonIndex, blockType: 'reasoning' }
          }
          reasoning += event.text
          yield { type: 'reasoning-delta', index: reasonIndex, text: event.text }
        } else if (event.type === 'delta') {
          if (textIndex < 0) {
            textIndex = nextIndex
            nextIndex += 1
            yield { type: 'block-start', index: textIndex, blockType: 'text' }
          }
          content += event.text
          yield { type: 'text-delta', index: textIndex, text: event.text }
        } else if (event.type === 'usage') {
          usage = event.usage
        } else if (event.type === 'done') {
          usage = event.usage ?? usage
          if (event.model) servedModel = event.model
        }
      }
    } catch (error) {
      // 已经吐出去的内容不能收回，先把开着的块闭合，再报错。
      if (reasonIndex >= 0) {
        yield { type: 'block-end', index: reasonIndex, block: { type: 'reasoning', text: reasoning } }
      }
      if (textIndex >= 0) {
        yield { type: 'block-end', index: textIndex, block: { type: 'text', text: content } }
      }
      if (options.signal?.aborted) {
        yield { type: 'finish', reason: { kind: 'aborted', failure: { message: '已取消', code: 'ABORTED' } } }
        return
      }
      this.#status.lastError = String(error?.message ?? error)
      this.#logger?.warn?.(`gemini-web2api: 请求失败：${this.#status.lastError}`)
      yield* this.#fail(this.#status.lastError, error?.code ?? 'REQUEST_FAILED')
      return
    }

    this.#status.lastError = ''
    this.#status.servedModel = servedModel
    this.#status.lastUsedAt = new Date().toISOString()

    if (reasonIndex >= 0) {
      yield { type: 'block-end', index: reasonIndex, block: { type: 'reasoning', text: reasoning } }
    }
    if (textIndex >= 0) {
      yield { type: 'block-end', index: textIndex, block: { type: 'text', text: content } }
    } else if (reasonIndex < 0) {
      yield* this.#fail('后端返回了空回复（可能是瞬时上游拒绝，重发一次通常就好）', 'EMPTY_RESPONSE')
      return
    }

    if (usage && Number.isFinite(usage.prompt_tokens + usage.completion_tokens)) {
      yield {
        type: 'usage',
        usage: {
          inputTokens: usage.prompt_tokens ?? 0,
          outputTokens: usage.completion_tokens ?? 0,
          totalTokens: usage.total_tokens ?? (usage.prompt_tokens ?? 0) + (usage.completion_tokens ?? 0),
        },
      }
    }

    yield { type: 'finish', reason: { kind: 'stop' } }
  }
}

/* ------------------------------------------------------------------ */
/* Plugin                                                              */
/* ------------------------------------------------------------------ */

export function apply(ctx, config = {}) {
  const logger = ctx.logger
  const baseUrl = String(config.baseUrl || 'http://127.0.0.1:8083').replace(/\/+$/, '')

  const status = {
    phase: 'starting',
    baseUrl,
    version: '',
    servedModel: '',
    lastUsedAt: '',
    lastError: '',
    cookieSynced: false,
    startedAt: Date.now(),
  }

  let wasSynced = false
  let disposed = false

  const client = new GeminiWeb2ApiClient({ baseUrl, apiKey: config.apiKey })

  // 初始目录：优先用配置里的清单，否则用匿名态的两个。cookie 挂上后会由
  // refreshModels() 从后端拉全量替换。
  const seeded = Array.isArray(config.models) && config.models.length > 0 ? config.models : DEFAULT_MODELS
  const catalog = new ModelCatalog(seeded)

  const adapter = new GeminiWeb2ApiAdapter({ client, catalog, logger, status })
  const registration = ctx.llm.registerAdapter([PROVIDER], adapter)
  logger?.info?.(`gemini-web2api: provider "${PROVIDER}" 已注册，等待后端 ${baseUrl}`)

  /**
   * 从后端拉取模型目录并替换。
   *
   * 后端 availableModels() 会按 cookie 状态放行不同集合，所以登录成功后必须重拉：
   * 匿名 2 个 → 登录后 17 个。变化时才广播，模型选择器会自动多出可选项。
   */
  const refreshModels = async () => {
    if (disposed) return
    try {
      const served = await client.models()
      if (served.length === 0) return
      if (!catalog.replace(served)) return
      status.models = catalog.models.length
      logger?.info?.(`gemini-web2api: 模型目录已更新，共 ${catalog.models.length} 个`)
      try {
        registration.replace([PROVIDER])
      } catch { /* 广播失败不影响实际调用 */ }
    } catch { /* 后端不可达时保留旧目录 */ }
  }

  // 浏览器登录：拉起独立 Chrome → 用户自行登录 → CDP 取 cookie 灌进后端 Cookie 池。
  // 浏览器实例保持存活，并每 SYNC_MS 重新同步一次：Chrome 的 __Secure-1PSIDTS 会自己
  // 轮换，后端跟着刷新就不必依赖它自己续期，也就绕开了 DBSC 对 Chrome 的限制。
  const browserLogin = config.adminToken
    ? new BrowserLogin({
      baseUrl,
      adminToken: config.adminToken,
      profileDir: loginProfileDir(),
      logger,
    })
    : null

  if (!browserLogin) {
    logger?.warn?.('gemini-web2api: 未配置 adminToken，浏览器登录功能不可用（请在 cordis.patch.yml 里补上）')
  }

  // Settings → Gemini 桥 卡片读取的状态端点（仅本机页面使用）。
  if (typeof ctx.inject === 'function') {
    ctx.inject(['webServer'], webCtx => {
      const send = (res, code, payload) => {
        res.writeHead(code, {
          'content-type': 'application/json; charset=utf-8',
          'cache-control': 'no-store',
          'referrer-policy': 'no-referrer',
        })
        res.end(JSON.stringify(payload))
      }

      webCtx.effect(() => webCtx.webServer.register({
        kind: 'exact',
        path: '/api/gemini-web2api/status',
        handler: (req, res) => {
          if (req.method !== 'GET') {
            send(res, 405, { error: 'method not allowed' })
            return
          }
          send(res, 200, {
            provider: PROVIDER,
            ...status,
            models: catalog.models.map(model => model.id),
            login: browserLogin
              ? { available: true, browserOpen: browserLogin.running, synced: browserLogin.synced, error: browserLogin.lastError }
              : { available: false, browserOpen: false, synced: false, error: '未配置 adminToken' },
            uptimeMs: Date.now() - status.startedAt,
          })
        },
      }))

      // 拉起登录浏览器。cookie 同步由后台定时器负责，这里只负责"打开窗口"。
      webCtx.effect(() => webCtx.webServer.register({
        kind: 'exact',
        path: '/api/gemini-web2api/login',
        handler: async (req, res) => {
          if (req.method !== 'POST') {
            send(res, 405, { error: 'method not allowed' })
            return
          }
          if (!browserLogin) {
            send(res, 501, { error: '未配置 adminToken，浏览器登录不可用' })
            return
          }
          try {
            const result = await browserLogin.start()
            send(res, 200, { ...result, message: '请在弹出的浏览器里登录 Google 账号' })
          } catch (error) {
            send(res, 500, { error: String(error?.message ?? error) })
          }
        },
      }))

      // 手动触发一次 cookie 同步（登录完点一下即可，不必等 15s）。
      webCtx.effect(() => webCtx.webServer.register({
        kind: 'exact',
        path: '/api/gemini-web2api/login/sync',
        handler: async (req, res) => {
          if (req.method !== 'POST') {
            send(res, 405, { error: 'method not allowed' })
            return
          }
          if (!browserLogin) {
            send(res, 501, { error: '未配置 adminToken，浏览器登录不可用' })
            return
          }
          send(res, 200, await browserLogin.sync())
        },
      }))
    })
  }

  ctx.effect(() => () => {
    status.phase = 'stopped'
  })

  const probe = async () => {
    if (disposed) return
    try {
      const health = await client.health()
      status.phase = 'ready'
      status.version = String(health?.version ?? '')
      status.lastError = ''
    } catch (error) {
      status.phase = 'offline'
      status.lastError = String(error?.message ?? error)
    }
  }

  void (async () => {
    // 后端可能比 DSH 晚启动（用户手动跑 exe），所以这里重试到就绪为止。
    for (let attempt = 0; attempt < BOOT_ATTEMPTS && !disposed; attempt += 1) {
      await probe()
      if (status.phase === 'ready') break
      await new Promise(resolve => setTimeout(resolve, 2000))
    }

    if (status.phase === 'ready') {
      logger?.info?.(`gemini-web2api: 后端已就绪（${status.version || '未知版本'}）`)
      // 后端挂了 cookie 后会多放行一批模型，启动时先对齐一次。
      await refreshModels()
      logger?.info?.(`gemini-web2api: 当前暴露 ${catalog.models.length} 个模型`)
    } else {
      logger?.warn?.(`gemini-web2api: 后端 ${baseUrl} 暂不可达：${status.lastError}`)
    }

    // 目录定时对齐：cookie 状态变化（登录/登出/失效）会改变放行集合。
    const modelTimer = setInterval(() => { void refreshModels() }, HEALTH_MS)
    modelTimer.unref?.()

    const timer = setInterval(() => { void probe() }, HEALTH_MS)
    timer.unref?.()
    ctx.effect(() => () => {
      clearInterval(timer)
      clearInterval(modelTimer)
    })
  })()

  // cookie 保活：浏览器开着就一直每 SYNC_MS 同步一次。
  // 只在同步成功后才会写后端，未登录时是纯粹的空转，不产生任何写操作。
  if (browserLogin) {
    const syncTimer = setInterval(() => {
      if (!browserLogin.running) return
      void browserLogin.sync().then(async result => {
        if (result.ok && !wasSynced) {
          wasSynced = true
          status.cookieSynced = true
          logger?.info?.('gemini-web2api: 已从浏览器获取 cookie 并写入后端 Cookie 池')
          // cookie 到位后后端才会放行 Pro 等模型，立刻重拉目录。
          await refreshModels()
          logger?.info?.(`gemini-web2api: 登录后可用模型 ${catalog.models.length} 个`)
        } else if (!result.ok) {
          wasSynced = false
          status.cookieSynced = false
        }
      })
    }, SYNC_MS)
    syncTimer.unref?.()
    ctx.effect(() => () => {
      clearInterval(syncTimer)
      browserLogin.stop()
    })
  }

  // 目录变化时让模型选择器刷新。
  ctx.effect(() => () => {
    try {
      registration.replace([PROVIDER])
    } catch { /* 卸载期广播失败可忽略 */ }
  })
}

export const inject = ['llm']
