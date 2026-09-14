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

  useEffect(() => {
    void api<FallbackConfig>('/api/config/fallback')
      .then(setConfig)
      .catch((e) => setError(String(e)))
      .finally(() => setLoading(false))
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
          <div className="flex items-center gap-2 text-xs text-base-content/60">
            <ShieldCheckIcon className="size-4" />
            <span>{keyConfigured ? 'API Key 已加密保存' : '尚未配置 API Key'}</span>
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
