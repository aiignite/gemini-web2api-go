window.__ModuleLoader__.load({
  id: 'dsh-gemini-web2api',
  factory(require) {
    const React = require('react')
    const h = React.createElement

    const NS = 'gemini-web2api'
    const zh = {
      title: 'Gemini 网页端',
      subtitle: 'gemini.google.com 反代 · 无需 API Key',
      phase: '状态',
      ready: '已连接',
      offline: '后端未运行',
      starting: '连接中',
      error: '异常',
      version: '后端版本',
      models: '可用模型',
      served: '最近服务模型',
      lastUsed: '最近使用',
      lastError: '最近错误',
      none: '无',
      modelsTitle: '模型清单',
      loading: '读取中…',
      admin: '打开管理面板',
      loginTitle: '挂载 Google 账号',
      loginHint: '匿名态只有 2 个文本模型。挂上 cookie 后解锁 Pro、扩展思考、读图与生图。',
      loginDocs: '点「登录用户」会拉起一个独立浏览器窗口，在里面正常登录 Google（密码、2FA 都由你操作）——插件不会经手你的凭据。登录完成后自动取 cookie 写入 Cookie 池，这张卡片会自动收起。',
      loginCta: '登录用户',
      loginWait: '请在弹出的浏览器里完成登录…',
      loginSynced: '已挂载，正在保活',
      loginManual: '打不开浏览器？',
      loginManualHint: '手动方式：用 Firefox 登录 gemini.google.com → F12 → Application → Cookies，全选复制，再粘进管理面板「Cookie 池 → + 导入 Cookie → 整串粘贴」。',
      loginDone: '添加后重启后端或等待保活刷新，模型清单会自动变长。',
      tipTitle: '为什么这次不用 Firefox',
      tip: '浏览器保持开着，插件每 15 秒把最新 cookie 重新同步一次。后端因此不必自己续期 __Secure-1PSIDTS，也就绕开了 Chrome 设备绑定会话（DBSC）导致的失效。',
    }
    const en = {
      title: 'Gemini Web',
      subtitle: 'gemini.google.com reverse proxy · no API key',
      phase: 'Status',
      ready: 'Connected',
      offline: 'Backend down',
      starting: 'Connecting',
      error: 'Error',
      version: 'Backend',
      models: 'Models',
      served: 'Last served',
      lastUsed: 'Last used',
      lastError: 'Last error',
      none: 'None',
      modelsTitle: 'Models',
      loading: 'Loading…',
      admin: 'Open admin panel',
      loginTitle: 'Attach a Google account',
      loginHint: 'Anonymous mode exposes only 2 text models. A cookie unlocks Pro, extended thinking, vision and image generation.',
      loginDocs: 'Clicking "Sign in" opens a separate browser window — sign in to Google there (password and 2FA are yours to enter; the plugin never touches your credentials). Once signed in, cookies are captured and written to the pool automatically and this card collapses.',
      loginCta: 'Sign in',
      loginWait: 'Finish signing in in the browser window that just opened…',
      loginSynced: 'Attached · keepalive running',
      loginManual: 'Browser will not open?',
      loginManualHint: 'Manual route: sign in to gemini.google.com in Firefox → F12 → Application → Cookies, select all and copy, then paste into the admin panel under Cookie pool → + Import cookies → paste whole string.',
      loginDone: 'Restart the backend or wait for the keepalive to refresh; the model list will grow.',
      tipTitle: 'Why Firefox is not needed this time',
      tip: 'The browser stays open and the plugin re-syncs fresh cookies every 15s, so the backend never has to renew __Secure-1PSIDTS itself — which sidesteps the Chrome Device Bound Session (DBSC) expiry.',
    }

    const label = { color: 'var(--dsw-alias-label-secondary)', fontSize: 12 }
    const value = { color: 'var(--dsw-alias-label-primary)', fontSize: 13, fontWeight: 500 }
    const muted = { color: 'var(--dsw-alias-label-secondary)', fontSize: 12, lineHeight: 1.6 }

    function formatUptime(ms) {
      if (!Number.isFinite(ms) || ms < 0) return '—'
      const s = Math.floor(ms / 1000)
      const hh = Math.floor(s / 3600)
      const mm = Math.floor((s % 3600) / 60)
      const ss = s % 60
      return hh > 0 ? `${hh}h ${mm}m` : mm > 0 ? `${mm}m ${ss}s` : `${ss}s`
    }

    function phaseColor(phase) {
      if (phase === 'ready') return 'var(--dsw-alias-state-success-primary)'
      if (phase === 'error' || phase === 'offline') return 'var(--dsw-alias-state-error-primary)'
      return 'var(--dsw-alias-state-warn-primary)'
    }

    function useStatus() {
      const [status, setStatus] = React.useState(null)
      React.useEffect(() => {
        let alive = true
        const tick = async () => {
          try {
            const response = await fetch('/api/gemini-web2api/status', { cache: 'no-store' })
            if (!response.ok) throw new Error(`HTTP ${response.status}`)
            const body = await response.json()
            if (alive) setStatus(body)
          } catch (error) {
            if (alive) setStatus({ phase: 'error', lastError: String(error && error.message ? error.message : error) })
          }
        }
        void tick()
        const timer = setInterval(tick, 5000)
        return () => { alive = false; clearInterval(timer) }
      }, [])
      return status
    }

    function Field({ name, text }) {
      return h('div', { style: { display: 'flex', flexDirection: 'column', gap: 2, minWidth: 110 } },
        h('span', { style: label }, name),
        h('span', { style: value }, text))
    }

    function Button({ children, href, primary }) {
      // These CSS variables are not defined in every theme. var() only falls back
      // when the first name is undefined; if both are missing the background
      // becomes transparent while color lands on #fff, which renders the whole
      // button invisible on a light theme. Hence the literal colours.
      const base = {
        display: 'inline-flex', alignItems: 'center', gap: 6,
        padding: '7px 13px', borderRadius: 8,
        fontSize: 12, fontWeight: 600, cursor: 'pointer',
        textDecoration: 'none',
        border: primary ? '1px solid #3b6ef6' : '1px solid var(--dsw-alias-border-l1, rgba(127,127,127,0.35))',
        background: primary ? '#3b6ef6' : 'var(--dsw-alias-bg-layer-2, rgba(127,127,127,0.14))',
        color: primary ? '#ffffff' : 'var(--dsw-alias-label-primary, #1f2328)',
      }
      return h('a', { href, target: '_blank', rel: 'noreferrer', style: base }, children)
    }

    function Card() {
      const status = useStatus()
      const t = React.useContext(LocaleContext)
      const [busy, setBusy] = React.useState(false)
      const [loginBusy, setLoginBusy] = React.useState(false)
      const phase = status ? status.phase : 'starting'
      const phaseText =
        phase === 'ready' ? t('ready')
          : phase === 'offline' ? t('offline')
            : phase === 'error' ? t('error')
              : t('starting')
      const models = status && Array.isArray(status.models) ? status.models : []
      const adminUrl = (status && status.baseUrl) || 'http://127.0.0.1:8083'
      const login = (status && status.login) || { available: false, browserOpen: false, synced: false }
      const loginAvailable = login.available === true
      // 已同步 → 收起引导；浏览器开着但还没登录 → 显示等待提示。
      const loggedIn = login.synced === true

      const startLogin = async () => {
        setBusy(true)
        setLoginBusy(true)
        try {
          await fetch('/api/gemini-web2api/login', { method: 'POST' })
        } catch { /* 状态轮询会反映结果 */ }
        // 后台每 15s 同步一次，这里短轮询几次让登录完成后的卡片尽快收起。
        for (let i = 0; i < 10; i += 1) {
          await new Promise(r => setTimeout(r, 1500))
          try {
            const body = await fetch('/api/gemini-web2api/status', { cache: 'no-store' }).then(r => r.json())
            if (body?.login?.synced) break
          } catch { /* 忽略 */ }
        }
        setBusy(false)
        setLoginBusy(false)
      }

      return h('div', {
        style: {
          display: 'flex', flexDirection: 'column', gap: 14,
          padding: 18, borderRadius: 14,
          background: 'var(--dsw-alias-bg-layer-1)',
          border: '1px solid var(--dsw-alias-border-l1)',
          color: 'var(--dsw-alias-label-primary)',
        },
      },
        h('div', { style: { display: 'flex', alignItems: 'center', gap: 10 } },
          h('span', {
            style: {
              width: 10, height: 10, borderRadius: 999,
              background: phaseColor(phase),
              boxShadow: `0 0 0 3px color-mix(in srgb, ${phaseColor(phase)} 22%, transparent)`,
            },
          }),
          h('div', { style: { display: 'flex', flexDirection: 'column' } },
            h('span', { style: { fontSize: 15, fontWeight: 600 } }, t('title')),
            h('span', { style: { ...label, marginTop: 2 } }, t('subtitle'))),
          h('span', { style: { marginLeft: 'auto', fontSize: 12, color: phaseColor(phase), fontWeight: 600 } }, phaseText)),

        h('div', { style: { display: 'flex', flexWrap: 'wrap', gap: 18 } },
          h(Field, { name: t('version'), text: status && status.version ? status.version : '—' }),
          h(Field, { name: t('models'), text: status ? String(models.length) : '—' }),
          h(Field, { name: t('served'), text: status && status.servedModel ? status.servedModel : t('none') }),
          h(Field, { name: t('lastUsed'), text: status && status.lastUsedAt ? new Date(status.lastUsedAt).toLocaleTimeString() : t('none') })),

        models.length > 0
          ? h('div', { style: { display: 'flex', flexDirection: 'column', gap: 8 } },
              h('span', { style: label }, t('modelsTitle')),
              h('div', { style: { display: 'flex', flexWrap: 'wrap', gap: 6 } },
                models.map(id => h('span', {
                  key: id,
                  style: {
                    fontSize: 12, padding: '3px 9px', borderRadius: 999,
                    background: 'var(--dsw-alias-bg-layer-2)',
                    border: '1px solid var(--dsw-alias-border-l1)',
                    color: 'var(--dsw-alias-label-secondary)',
                  },
                }, id))))
          : h('span', { style: label }, status ? '' : t('loading')),

        h('div', { style: { display: 'flex', gap: 8, flexWrap: 'wrap' } },
          h(Button, { href: `${adminUrl}/admin`, primary: true }, t('admin'))),

        // 挂账号引导。cookie 同步成功后整块收起，只留一行状态。
        loggedIn
          ? h('div', {
            style: {
              display: 'flex', alignItems: 'center', gap: 8,
              padding: 12, borderRadius: 10,
              background: 'var(--dsw-alias-bg-layer-2)',
              border: '1px solid var(--dsw-alias-border-l1)',
            },
          },
            h('span', { style: { fontSize: 13, fontWeight: 600, color: 'var(--dsw-alias-state-success-primary, #1a7f37)' } },
              '● ' + t('loginSynced')),
            h('span', { style: muted }, t('loginDone')))
          : h('div', {
            style: {
              display: 'flex', flexDirection: 'column', gap: 8,
              padding: 14, borderRadius: 10,
              background: 'var(--dsw-alias-bg-layer-2)',
              border: '1px solid var(--dsw-alias-border-l1)',
            },
          },
            h('span', { style: { fontSize: 13, fontWeight: 600 } }, t('loginTitle')),
            h('span', { style: muted }, t('loginHint')),
            h('span', { style: muted }, t('loginDocs')),
            h('div', { style: { display: 'flex', gap: 8, flexWrap: 'wrap', alignItems: 'center' } },
              h('button', {
                onClick: startLogin,
                disabled: busy || !loginAvailable,
                style: {
                  display: 'inline-flex', alignItems: 'center', gap: 6,
                  padding: '7px 13px', borderRadius: 8,
                  fontSize: 12, fontWeight: 600, cursor: busy ? 'wait' : 'pointer',
                  border: '1px solid #3b6ef6', background: '#3b6ef6', color: '#ffffff',
                  opacity: busy || !loginAvailable ? 0.55 : 1,
                },
              }, busy ? t('loginWait') : t('loginCta')),
              loginBusy ? h('span', { style: muted }, t('loginWait')) : null),
            h('span', { style: muted }, h('b', null, t('loginManual') + '：'), t('loginManualHint')),
            h('span', { style: { ...muted, color: 'var(--dsw-alias-state-warn-primary, #b26b00)' } },
              h('b', null, t('tipTitle') + '：'), t('tip'))),

        status && status.lastError
          ? h('div', { style: { display: 'flex', flexDirection: 'column', gap: 2 } },
              h('span', { style: label }, t('lastError')),
              h('span', { style: { fontSize: 12, color: 'var(--dsw-alias-state-error-primary)', wordBreak: 'break-all' } }, status.lastError))
          : null,

        status && status.uptimeMs != null
          ? h('span', { style: label }, t('lastUsed') + ' · ' + formatUptime(status.uptimeMs))
          : null)
    }

    const LocaleContext = React.createContext((key) => key)

    const inject = ['slots', 'locale']

    function apply(ctx) {
      const t = ctx.locale.bind(NS)
      ctx.effect(() => ctx.locale.register(NS, { zh, en }), 'gemini-web2api: copy')
      ctx.slots.inject('settings.section', () => ctx.slots.register({
        name: 'settings.section',
        id: 'gemini-web2api',
        order: 25,
        label: () => t('title'),
      }, function Section() {
        return h(LocaleContext.Provider, { value: t }, h(Card))
      }))
    }

    const module = { exports: {} }
    module.exports = { apply, inject, NS }
    return module.exports
  },
})
