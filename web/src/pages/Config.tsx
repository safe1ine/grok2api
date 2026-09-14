import { SaveIcon, ShieldCheckIcon } from 'lucide-react'
import { useEffect, useState } from 'react'
import { api } from '../api'

type FallbackConfig = {
  openai_base_url: string
  openai_model: string
  openai_key_set: boolean
  anthropic_base_url: string
  anthropic_model: string
  anthropic_key_set: boolean
}

type Provider = 'openai' | 'anthropic'

type FallbackUsage = {
  historical_calls: number
  today_calls: number
  daily: Array<{ day: string; calls: number }>
}

const emptyConfig: FallbackConfig = {
  openai_base_url: '', openai_model: '', openai_key_set: false,
  anthropic_base_url: '', anthropic_model: '', anthropic_key_set: false,
}

export default function Config() {
  const [config, setConfig] = useState(emptyConfig)
  const [keys, setKeys] = useState({ openai: '', anthropic: '' })
  const [loading, setLoading] = useState(true)
  const [saving, setSaving] = useState(false)
  const [message, setMessage] = useState('')
  const [error, setError] = useState('')
  const [usage, setUsage] = useState<FallbackUsage>({ historical_calls: 0, today_calls: 0, daily: [] })
  const [checking, setChecking] = useState<Provider | null>(null)

  useEffect(() => {
    void Promise.all([
      api<FallbackConfig>('/api/config/fallback'),
      api<FallbackUsage>('/api/config/fallback/usage'),
    ]).then(([nextConfig, nextUsage]) => {
      setConfig(nextConfig)
      setUsage(nextUsage)
    }).catch((e) => setError(String(e))).finally(() => setLoading(false))
  }, [])

  function setValue(field: keyof FallbackConfig, value: string) {
    setConfig((current) => ({ ...current, [field]: value }))
  }

  async function save() {
    setSaving(true)
    setMessage('')
    setError('')
    try {
      const body: Record<string, string> = {
        openai_base_url: config.openai_base_url,
        openai_model: config.openai_model,
        anthropic_base_url: config.anthropic_base_url,
        anthropic_model: config.anthropic_model,
      }
      if (keys.openai) body.openai_key = keys.openai
      if (keys.anthropic) body.anthropic_key = keys.anthropic
      const updated = await api<FallbackConfig>('/api/config/fallback', {
        method: 'PUT',
        body: JSON.stringify(body),
      })
      setConfig(updated)
      setKeys({ openai: '', anthropic: '' })
      setMessage('Fallback 配置已保存')
    } catch (e) {
      setError(String(e))
    } finally {
      setSaving(false)
    }
  }

  async function check(provider: Provider) {
    setChecking(provider)
    setMessage('')
    setError('')
    try {
      const result = await api<{ model: string }>('/api/config/fallback/check', {
        method: 'POST', body: JSON.stringify({ provider }),
      })
      setMessage(`${provider === 'openai' ? 'OpenAI' : 'Anthropic'} fallback 模型 ${result.model} 工作正常`)
    } catch (e) {
      setError(String(e))
    } finally {
      setChecking(null)
    }
  }

  function providerCard(provider: Provider, title: string, description: string) {
    const prefix = provider === 'openai' ? 'openai' : 'anthropic'
    const baseField = `${prefix}_base_url` as 'openai_base_url' | 'anthropic_base_url'
    const modelField = `${prefix}_model` as 'openai_model' | 'anthropic_model'
    const keyConfigured = config[`${prefix}_key_set` as 'openai_key_set' | 'anthropic_key_set']
    return (
      <div className="card border border-base-300 bg-base-100 shadow-sm">
        <div className="card-body gap-4">
          <div>
            <h2 className="card-title text-base">{title}</h2>
            <p className="mt-1 text-sm text-base-content/60">{description}</p>
          </div>
          <label className="form-control">
            <span className="label-text mb-1 text-sm">Base URL</span>
            <input
              className="input input-bordered w-full"
              value={config[baseField]}
              placeholder={provider === 'openai' ? 'https://api.openai.com/v1' : 'https://api.anthropic.com'}
              onChange={(e) => setValue(baseField, e.target.value)}
            />
          </label>
          <label className="form-control">
            <span className="label-text mb-1 text-sm">模型名称</span>
            <input
              className="input input-bordered w-full"
              value={config[modelField]}
              placeholder={provider === 'openai' ? 'gpt-4.1' : 'claude-sonnet-4-5'}
              onChange={(e) => setValue(modelField, e.target.value)}
            />
          </label>
          <label className="form-control">
            <span className="label-text mb-1 text-sm">API Key</span>
            <input
              type="password"
              className="input input-bordered w-full font-mono"
              value={keys[provider]}
              placeholder={keyConfigured ? '已配置，留空保持不变' : '请输入 API Key'}
              autoComplete="new-password"
              onChange={(e) => setKeys((current) => ({ ...current, [provider]: e.target.value }))}
            />
          </label>
          <div className="flex items-center justify-between gap-3">
            <div className="flex items-center gap-2 text-xs text-base-content/60">
              <ShieldCheckIcon className="size-4" />
              <span>{keyConfigured ? 'API Key 已加密保存' : '尚未配置 API Key'}</span>
            </div>
            <button className="btn btn-outline btn-sm" disabled={checking !== null} onClick={() => void check(provider)}>
              {checking === provider && <span className="loading loading-spinner loading-xs" />}
              检查模型
            </button>
          </div>
        </div>
      </div>
    )
  }

  if (loading) return <div className="flex h-72 items-center justify-center"><span className="loading loading-spinner loading-lg" /></div>

  return (
    <section className="grid gap-6">
      <header>
        <h1 className="text-xl font-semibold">其他配置</h1>
        <p className="mt-1 text-sm text-base-content/60">账号池全部不可用时，按请求协议直接转发到对应的 fallback 上游。</p>
      </header>
      <div className="alert alert-info text-sm">
        <span>429 不会触发 fallback。Fallback 不经过 Grok 兼容转换，只替换配置的模型名称和认证 Key。</span>
      </div>
      <div className="grid gap-4 sm:grid-cols-2">
        <div className="stat rounded-box border border-base-300 bg-base-100"><div className="stat-title">Fallback 历史调用</div><div className="stat-value text-3xl">{usage.historical_calls.toLocaleString()}</div></div>
        <div className="stat rounded-box border border-base-300 bg-base-100"><div className="stat-title">Fallback 今日调用</div><div className="stat-value text-3xl">{usage.today_calls.toLocaleString()}</div></div>
      </div>
      <div className="card border border-base-300 bg-base-100 shadow-sm">
        <div className="card-body"><h2 className="card-title text-base">近 30 天趋势</h2>
          {usage.daily.length === 0 ? <p className="text-sm text-base-content/50">暂无 fallback 调用</p> : (
            <div className="flex h-36 items-end gap-1 overflow-x-auto pt-4">
              {usage.daily.map((point) => { const max = Math.max(...usage.daily.map((item) => item.calls), 1); return <div key={point.day} className="group flex min-w-5 flex-1 flex-col items-center justify-end gap-1" title={`${new Date(point.day).toLocaleDateString()}：${point.calls} 次`}><span className="text-[10px] opacity-0 group-hover:opacity-70">{point.calls}</span><div className="w-full rounded-t bg-neutral" style={{ height: `${Math.max(4, point.calls / max * 100)}px` }} /></div> })}
            </div>
          )}
        </div>
      </div>
      <div className="grid gap-4 lg:grid-cols-2">
        {providerCard('openai', 'OpenAI fallback', '用于 OpenAI 兼容请求，直接透传请求和响应。')}
        {providerCard('anthropic', 'Anthropic fallback', '用于 Anthropic Messages 请求，直接透传请求和响应。')}
      </div>
      {message && <div className="alert alert-success text-sm">{message}</div>}
      {error && <div className="alert alert-error text-sm">{error}</div>}
      <div className="flex justify-end">
        <button className="btn btn-neutral" disabled={saving} onClick={() => void save()}>
          {saving ? <span className="loading loading-spinner loading-sm" /> : <SaveIcon className="size-4" />}
          保存配置
        </button>
      </div>
    </section>
  )
}
