# dsh-gemini-web2api

把本机的 [gemini-web2api-go](https://github.com/zexadev/gemini-web2api-go) 注册成
DeepSeek Harness 的一个 provider。选 `Gemini 网页端 / Gemini 3.6 Flash` 即可在
DSH 里直接对话。

## 它做什么

```
[DSH 模型选择器]
    ↓ http://127.0.0.1:8083/v1/chat/completions
[gemini-web2api-go]   ← 独立的 Go 进程，本插件不负责它的生命周期
    ↓ 代理池里的出口
[gemini.google.com]
```

后端把 Gemini 网页端反代成 OpenAI 兼容接口，**不需要 Google API Key、没有付费
配额**。本插件只做协议翻译：DSH 会话 → OpenAI `messages`，SSE 流 → DSH block
事件。

## 前置条件

后端必须已经在跑。本插件不含后端二进制，也不管它的启停：

```powershell
cd E:\DSH\gemini-web2api-go
.\gemini-web2api-go.exe --config .\config.json --admin-token <你的token>
```

首次使用请先确认 `curl http://127.0.0.1:8083/` 返回 `{"status":"ok",...}`。

## 配置

`cordis.patch.yml`：

| 项 | 默认 | 说明 |
| --- | --- | --- |
| `baseUrl` | `http://127.0.0.1:8083` | 后端根地址 |
| `apiKey` | — | 后端 API key（面板「设置」页可见） |
| `models` | 匿名可用的两个 | 暴露给模型选择器的清单 |

**代理不在这里配**。后端不读 `HTTPS_PROXY`/`ALL_PROXY`，它只从自己的代理池取
出口——配置写在后端的 `config.json` 的 `proxy` 字段或管理面板「代理池」里。

## 能力边界

- **匿名态**（未挂 cookie）只有 `gemini-3.6-flash` 和 `gemini-3.5-flash-lite`
  两个文本模型，**不支持读图**（后端会返回 400）。
- 挂了 Google cookie 后才解锁 `gemini-3.1-pro`、扩展思考版、读图、生图/音乐/画布。
  这些要手动在 `models` 里加，并建议同时改用 Firefox 导出的 cookie。
- 本插件**不发工具调用**：后端的 function calling 是 prompt 级实现、不稳定，
  转发过来只会让 agent 客户端行为诡异，因此这里只暴露纯文本。
- 单出口 IP 突发约 80-180 次请求会被 Google 重定向到 sorry 页（约两小时恢复）。
  要放大产能得在代理池里加出口。

## 状态端点

本机页面可读 `GET /api/gemini-web2api/status`，返回后端版本、最近一次实际服务的
模型和最后错误，用来判断「模型选不中」是插件没加载还是后端没起来。

## License

MIT
