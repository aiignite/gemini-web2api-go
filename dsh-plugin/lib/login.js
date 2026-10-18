/**
 * 浏览器登录 + cookie 提取（Chrome DevTools Protocol）。
 *
 * 背景：gemini-web2api-go 的 Cookie 池需要一整条 Google 会话 cookie 才能解锁
 * Pro / 扩展思考 / 读图 / 生图。手抄这 7 项既繁琐又易错，所以这里由插件拉起
 * 一个真实浏览器，让用户在界面里正常登录（Google 会话、2FA、CAPTCHA 都由用户
 * 自己完成，插件不接触任何凭据），登录完成后从浏览器里把 cookie 取出来灌进
 * 后端。
 *
 * 为什么值得这么做：后端自己续期 `__Secure-1PSIDTS` 时会撞上 Chrome 的
 * Device Bound Session Credentials 而 401。但**浏览器实例是活的**——只要我们
 * 保持它运行并周期性地把最新 cookie 重新同步过去，后端就一直拿到有效会话。
 * 这条路径不需要 Firefox。
 *
 * 只依赖 Node 内置能力：child_process + 全局 WebSocket + fetch。
 */

import { spawn } from 'node:child_process'
import { existsSync } from 'node:fs'
import { join } from 'node:path'

/** CDP 调试端口。用高位端口避免和本机其它调试会话冲突。 */
const DEBUG_PORT = 9333

/** gemini.google.com 相关 cookie 的域名后缀。 */
const COOKIE_DOMAINS = ['gemini.google.com', '.google.com']

/**
 * 后端真正需要的 cookie 名字。缺 SAPISID 就没法算 SAPISIDHASH，
 * 缺 __Secure-1PSIDTS 会被当成匿名用户。
 */
const REQUIRED = ['SID', 'HSID', 'SSID', 'APISID', 'SAPISID', '__Secure-1PSID', '__Secure-1PSIDTS']

/** 候选浏览器，按「能用且未被 DebuggingPort 限制」的顺序尝试。 */
function browserCandidates() {
  const env = process.env
  const candidates = [
    { path: env.CHROME_PATH, name: 'Chrome' },
    { path: join(env.ProgramFiles || 'C:\\Program Files', 'Google', 'Chrome', 'Application', 'chrome.exe'), name: 'Chrome' },
    { path: join(env['ProgramFiles(x86)'] || 'C:\\Program Files (x86)', 'Google', 'Chrome', 'Application', 'chrome.exe'), name: 'Chrome' },
    { path: join(env.ProgramFiles || 'C:\\Program Files', 'Microsoft', 'Edge', 'Application', 'msedge.exe'), name: 'Edge' },
    { path: join(env['ProgramFiles(x86)'] || 'C:\\Program Files (x86)', 'Microsoft', 'Edge', 'Application', 'msedge.exe'), name: 'Edge' },
  ]
  return candidates.filter(entry => entry.path && existsSync(entry.path))
}

const sleep = ms => new Promise(resolve => setTimeout(resolve, ms))

/** 轮询 CDP 的 /json/version，直到调试端口起来。 */
async function waitForDevTools(attempts = 40, intervalMs = 250) {
  for (let i = 0; i < attempts; i += 1) {
    try {
      const response = await fetch(`http://127.0.0.1:${DEBUG_PORT}/json/version`)
      if (response.ok) return await response.json()
    } catch { /* 端口还没起来 */ }
    await sleep(intervalMs)
  }
  throw new Error(`等待浏览器调试端口 127.0.0.1:${DEBUG_PORT} 超时`)
}

/** 取出一个 Gemini 标签页的 webSocketDebuggerUrl。 */
async function findGeminiTarget() {
  for (let i = 0; i < 40; i += 1) {
    try {
      const response = await fetch(`http://127.0.0.1:${DEBUG_PORT}/json/list`)
      if (response.ok) {
        const targets = await response.json()
        const page = targets.find(t => t.type === 'page' && /gemini\.google\.com/.test(t.url || ''))
        if (page?.webSocketDebuggerUrl) return page
      }
    } catch { /* 忽略 */ }
    await sleep(300)
  }
  return null
}

/** 一次 CDP 调用（取 cookie / 导航）。 */
function cdpCall(socket, method, params = {}, id = Math.floor(Math.random() * 1e9)) {
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      socket.removeEventListener('message', onMessage)
      reject(new Error(`CDP ${method} 超时`))
    }, 15000)

    function onMessage(event) {
      let message
      try {
        message = JSON.parse(event.data)
      } catch {
        return
      }
      if (message.id !== id) return
      clearTimeout(timer)
      socket.removeEventListener('message', onMessage)
      if (message.error) reject(new Error(message.error.message || JSON.stringify(message.error)))
      else resolve(message.result)
    }

    socket.addEventListener('message', onMessage)
    socket.send(JSON.stringify({ id, method, params }))
  })
}

/** 打开一个 WebSocket 并等待连上。 */
function connect(url) {
  return new Promise((resolve, reject) => {
    const socket = new WebSocket(url)
    const timer = setTimeout(() => reject(new Error('CDP WebSocket 连接超时')), 10000)
    socket.addEventListener('open', () => {
      clearTimeout(timer)
      resolve(socket)
    }, { once: true })
    socket.addEventListener('error', () => {
      clearTimeout(timer)
      reject(new Error('CDP WebSocket 连接失败'))
    }, { once: true })
  })
}

/**
 * 把 cookie 数组转成后端要的 `name=value; name=value` 字符串。
 *
 * 后端按 cookie 名字识别会话，所以这里按名字精确挑选，不按顺序也不按数量。
 */
export function toCookieHeader(cookies) {
  const picked = new Map()
  for (const cookie of cookies) {
    const domain = String(cookie.domain || '')
    if (!COOKIE_DOMAINS.some(suffix => domain === suffix || domain.endsWith(suffix))) continue
    picked.set(cookie.name, cookie.value)
  }
  // 保持后端期望的顺序，便于人工核对。
  return REQUIRED.filter(name => picked.has(name))
    .map(name => `${name}=${picked.get(name)}`)
    .join('; ')
}

/**
 * 登录会话管理器。
 *
 * 生命周期：start() 拉起浏览器 → 用户自行登录 → sync() 周期把最新 cookie 推给
 * 后端。浏览器实例在插件存活期间一直开着，这正是绕过 DBSC 的关键。
 */
export class BrowserLogin {
  #baseUrl
  #adminToken
  #profileDir
  #logger
  #child = null
  #browser = null
  #synced = false
  #lastError = ''
  // id of the account row we wrote. The backend's POST /admin/api/cookies only
  // ever appends, and PATCH can change nothing but label/note/status — there is
  // no way to update a cookie in place. So a refresh must delete the old row and
  // recreate it. Without remembering the id we would append a duplicate every
  // 15 seconds (observed: 34 identical rows in the pool).
  #accountId = null

  constructor({ baseUrl, adminToken, profileDir, logger }) {
    this.#baseUrl = baseUrl
    this.#adminToken = adminToken
    this.#profileDir = profileDir
    this.#logger = logger
  }

  #headers() {
    return {
      'content-type': 'application/json',
      authorization: `Bearer ${this.#adminToken}`,
    }
  }

  /** Remove the row we wrote last time. Returns false when it fails. */
  async #dropPrevious() {
    if (this.#accountId == null) return true
    const id = this.#accountId
    this.#accountId = null
    try {
      const response = await fetch(`${this.#baseUrl}/admin/api/cookies/${id}`, {
        method: 'DELETE',
        headers: this.#headers(),
      })
      return response.ok
    } catch {
      return false
    }
  }

  get running() {
    return this.#child !== null
  }

  get synced() {
    return this.#synced
  }

  get lastError() {
    return this.#lastError
  }

  /**
   * 拉起浏览器并打开 Gemini 登录页。
   *
   * 用独立的 user-data-dir，这样不会干扰用户日常的 Chrome 配置；同一个目录复用
   * 则意味着第二次登录不必重新扫码。
   */
  async start() {
    if (this.#child) return { ok: true, already: true }

    const candidates = browserCandidates()
    if (candidates.length === 0) throw new Error('找不到 Chrome 或 Edge')

    const { path: exe, name } = candidates[0]

    // 已经有一个带调试端口的实例在跑就直接复用，否则新开一个。
    try {
      await fetch(`http://127.0.0.1:${DEBUG_PORT}/json/version`)
      this.#logger?.info?.(`gemini-web2api: 复用已在运行的浏览器调试端口 ${DEBUG_PORT}`)
    } catch {
      this.#child = spawn(exe, [
        `--remote-debugging-port=${DEBUG_PORT}`,
        `--user-data-dir=${this.#profileDir}`,
        '--no-first-run',
        '--no-default-browser-check',
        'https://gemini.google.com/',
      ], { detached: true, stdio: 'ignore' })
      this.#child.unref()
      this.#logger?.info?.(`gemini-web2api: 已拉起 ${name}，请在浏览器里登录 Google 账号`)
    }

    await waitForDevTools()
    return { ok: true, browser: name }
  }

  /**
   * 兜底清理：把池子里所有 browser-login 记录删掉。
   *
   * 正常路径靠 #accountId 精确替换；但如果 DSH 重启过（内存里的 id 丢失），
   * 或历史版本已经堆了重复项，就靠这一步收敛回"池里只有一条"。只在
   * 拿不到 id 时才调用，不会误删用户手动添加的其它账号。
   */
  async #purgeStrays() {
    try {
      const response = await fetch(`${this.#baseUrl}/admin/api/cookies`, { headers: this.#headers() })
      if (!response.ok) return
      const body = await response.json()
      const items = Array.isArray(body?.items) ? body.items : []
      for (const item of items) {
        if (item?.label !== 'browser-login') continue
        if (item?.id === this.#accountId) continue
        await fetch(`${this.#baseUrl}/admin/api/cookies/${item.id}`, {
          method: 'DELETE',
          headers: this.#headers(),
        }).catch(() => {})
      }
    } catch { /* 清理失败不影响写入 */ }
  }

  /**
   * 把当前浏览器的 Gemini 会话 cookie 同步进后端 Cookie 池。
   *
   * 登录完成后周期调用：浏览器里 `__Secure-1PSIDTS` 会自己轮换，我们跟着刷新
   * 即可，后端因此不必依赖它自己续期（那条路在 Chrome 上会被 DBSC 拒绝）。
   *
   * @returns {Promise<{ok:boolean, loggedIn?:boolean, reason?:string, cookieCount?:number}>}
   */
  async sync() {
    this.#lastError = ''
    try {
      const target = await findGeminiTarget()
      if (!target) {
        this.#synced = false
        return { ok: false, reason: 'no-tab' }
      }

      const socket = await connect(target.webSocketDebuggerUrl)
      try {
        const result = await cdpCall(socket, 'Network.getAllCookies')
        const header = toCookieHeader(result?.cookies ?? [])
        if (!header) {
          this.#synced = false
          return { ok: false, loggedIn: false, reason: 'not-logged-in' }
        }

        // 后端的管理接口只认 admin token（requireAuth 校验 Bearer <admin_token>），
        // 拿 OpenAI API key 去调会直接 401。
        // 先删掉上一轮写入的那条：后端没有"更新 cookie"的接口，只能删了重建，
        // 否则池子里会每 15 秒多出一条完全相同的账号。
        await this.#dropPrevious()
        if (this.#accountId == null) await this.#purgeStrays()

        const response = await fetch(`${this.#baseUrl}/admin/api/cookies`, {
          method: 'POST',
          headers: this.#headers(),
          body: JSON.stringify({ label: 'browser-login', cookie: header }),
        })
        if (!response.ok) {
          this.#synced = false
          this.#lastError = `后端拒绝 cookie：HTTP ${response.status}`
          return { ok: false, reason: 'rejected' }
        }

        const body = await response.json().catch(() => ({}))
        if (Number.isFinite(body?.id)) this.#accountId = body.id

        this.#synced = true
        return { ok: true, loggedIn: true, cookieCount: header.split(';').length, accountId: this.#accountId }
      } finally {
        socket.close()
      }
    } catch (error) {
      this.#synced = false
      this.#lastError = String(error?.message ?? error)
      return { ok: false, reason: 'error' }
    }
  }

  /** 关闭由插件拉起的浏览器（只关自己起的那个）。 */
  stop() {
    try {
      if (this.#child?.pid) {
        process.kill(this.#child.pid)
        this.#logger?.info?.('gemini-web2api: 已关闭登录浏览器')
      }
    } catch { /* 进程可能已退出 */ }
    this.#child = null
    this.#synced = false
  }
}
