import { CheckIcon, CopyIcon, EyeIcon, PlusIcon, RefreshCwIcon, Trash2Icon } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { api, type KeyItem } from '../api'
import { ConfirmDialog, TextInputDialog } from '../components/Dialogs'

const callCountFormatter = new Intl.NumberFormat('zh-CN')

export default function Keys() {
  const [keys, setKeys] = useState<KeyItem[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [keyView, setKeyView] = useState<{ name: string; key: string; regenerated?: boolean } | null>(null)
  const [createOpen, setCreateOpen] = useState(false)
  const [keyName, setKeyName] = useState('')
  const [creating, setCreating] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<KeyItem | null>(null)
  const [deletingId, setDeletingId] = useState<number | null>(null)
  const [revealingId, setRevealingId] = useState<number | null>(null)
  const [regenerateTarget, setRegenerateTarget] = useState<KeyItem | null>(null)
  const [regeneratingId, setRegeneratingId] = useState<number | null>(null)

  const load = useCallback(async (showLoading = true) => {
    if (showLoading) setLoading(true)
    setError('')
    try {
      setKeys(await api<KeyItem[]>('/api/keys'))
    } catch (e) {
      setError(String(e))
    } finally {
      if (showLoading) setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
    const timer = window.setInterval(() => void load(false), 60 * 1000)
    return () => window.clearInterval(timer)
  }, [load])

  async function create() {
    const name = keyName.trim() || 'default'
    setCreating(true)
    setError('')
    try {
      const res = await api<{ id: number; name: string; key: string }>('/api/keys', {
        method: 'POST',
        body: JSON.stringify({ name }),
      })
      setKeyView({ name: res.name, key: res.key })
      setCreateOpen(false)
      setKeyName('')
      await load()
    } catch (e) {
      setError(String(e))
    } finally {
      setCreating(false)
    }
  }

  async function reveal(key: KeyItem) {
    if (!key.has_secret || revealingId !== null) return
    setRevealingId(key.id)
    setError('')
    try {
      const result = await api<{ key: string }>(`/api/keys/${key.id}/secret`)
      setKeyView({ name: key.name, key: result.key })
    } catch (e) {
      setError(String(e))
    } finally {
      setRevealingId(null)
    }
  }

  async function regenerate(key: KeyItem) {
    setRegeneratingId(key.id)
    setError('')
    try {
      const result = await api<{ key: string }>(`/api/keys/${key.id}/regenerate`, { method: 'POST' })
      setKeyView({ name: key.name, key: result.key, regenerated: true })
      setRegenerateTarget(null)
      await load()
    } catch (e) {
      setError(String(e))
    } finally {
      setRegeneratingId(null)
    }
  }

  async function remove(key: KeyItem) {
    setDeletingId(key.id)
    setError('')
    try {
      await api(`/api/keys/${key.id}`, { method: 'DELETE' })
      await load()
    } catch (e) {
      setError(String(e))
    } finally {
      setDeletingId(null)
      setDeleteTarget(null)
    }
  }

  return (
    <section className="grid gap-6">
      <header className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <h1 className="text-2xl font-semibold tracking-tight">密钥管理</h1>
        <button className="btn btn-neutral btn-sm self-start sm:self-auto" onClick={() => setCreateOpen(true)}>
          <PlusIcon className="size-4" />
          创建 Key
        </button>
      </header>

      {error && <div className="alert alert-error">{error}</div>}

      <div className="overflow-hidden rounded-2xl border border-base-300 bg-base-100 shadow-sm">
        <div className="border-b border-base-300 px-4 py-4 sm:px-5">
          <h2 className="font-semibold">访问密钥</h2>
          <p className="mt-0.5 text-xs text-base-content/50">当前共 {keys.length} 个 Key</p>
        </div>
        <div className="overflow-x-auto">
          <table className="table">
          <thead>
            <tr>
              <th>ID</th>
              <th>名称</th>
              <th>前缀</th>
              <th>状态</th>
              <th className="text-right">历史调用</th>
              <th className="text-right">今日调用</th>
              <th>创建时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={8} className="text-center">
                  <span className="loading loading-spinner" />
                </td>
              </tr>
            ) : keys.length === 0 ? (
              <tr>
                <td colSpan={8} className="text-center text-base-content/50">
                  还没有 Key，点击右上角创建
                </td>
              </tr>
            ) : (
              keys.map((k) => (
                <tr key={k.id}>
                  <td>{k.id}</td>
                  <td>{k.name}</td>
                  <td>
                    <code>{k.prefix}…</code>
                  </td>
                  <td>
                    <span className={`badge ${k.revoked ? 'badge-error' : 'badge-success'}`}>
                      {k.revoked ? '已撤销' : '生效中'}
                    </span>
                  </td>
                  <td className="text-right tabular-nums">{callCountFormatter.format(k.historical_calls)}</td>
                  <td className="text-right tabular-nums">{callCountFormatter.format(k.today_calls)}</td>
                  <td>{new Date(k.created_at).toLocaleString()}</td>
                  <td>
                    <div className="flex flex-wrap gap-2">
                      <button
                        className="btn btn-ghost btn-xs"
                        title={k.has_secret ? '查看密钥' : '历史密钥未保存明文，请重新生成'}
                        disabled={!k.has_secret || revealingId === k.id}
                        onClick={() => void reveal(k)}
                      >
                        {revealingId === k.id ? <span className="loading loading-spinner loading-xs" /> : <EyeIcon className="size-3.5" />}
                        {k.has_secret ? '查看' : '不可查看'}
                      </button>
                      <button
                        className="btn btn-ghost btn-xs"
                        disabled={regeneratingId === k.id}
                        onClick={() => setRegenerateTarget(k)}
                      >
                        {regeneratingId === k.id ? <span className="loading loading-spinner loading-xs" /> : <RefreshCwIcon className="size-3.5" />}
                        重新生成
                      </button>
                      <button
                        className="btn btn-error btn-xs"
                        disabled={deletingId === k.id}
                        onClick={() => setDeleteTarget(k)}
                      >
                        {deletingId === k.id ? <span className="loading loading-spinner loading-xs" /> : <Trash2Icon className="size-3.5" />}
                        {deletingId === k.id ? '删除中' : '删除'}
                      </button>
                    </div>
                  </td>
                </tr>
              ))
            )}
          </tbody>
          </table>
        </div>
      </div>

      <TextInputDialog
        open={createOpen}
        title="创建 Key"
        label="名称（备注）"
        value={keyName}
        placeholder="default"
        confirmLabel="创建 Key"
        pending={creating}
        onChange={setKeyName}
        onClose={() => {
          setCreateOpen(false)
          setKeyName('')
        }}
        onConfirm={() => void create()}
      />

      <ConfirmDialog
        open={regenerateTarget !== null}
        title="重新生成 Key？"
        description={regenerateTarget ? `重新生成 ${regenerateTarget.name} 后，旧 Key 会立即失效，所有调用方都必须更换为新 Key。` : ''}
        confirmLabel="重新生成"
        tone="danger"
        pending={regeneratingId !== null}
        onClose={() => setRegenerateTarget(null)}
        onConfirm={() => {
          if (regenerateTarget) void regenerate(regenerateTarget)
        }}
      />

      <ConfirmDialog
        open={deleteTarget !== null}
        title="删除 Key？"
        description={deleteTarget ? `确定删除 ${deleteTarget.name}？删除后该 Key 立即失效。` : ''}
        confirmLabel="删除 Key"
        tone="danger"
        pending={deletingId !== null}
        onClose={() => setDeleteTarget(null)}
        onConfirm={() => {
          if (deleteTarget) void remove(deleteTarget)
        }}
      />

      {keyView && (
        <div className="modal modal-open">
          <div className="modal-box">
            <h3 className="mb-2 text-lg font-bold">{keyView.regenerated ? 'Key 已重新生成' : '查看 Key'}</h3>
            <p className="mb-4 text-sm">
              {keyView.regenerated
                ? '旧 Key 已立即失效，请更新所有调用方。'
                : `${keyView.name} 的完整密钥：`}
            </p>
            <div className="flex gap-2">
              <input className="input input-bordered min-w-0 flex-1 font-mono" readOnly value={keyView.key} />
              <button
                className="btn btn-outline"
                onClick={() => navigator.clipboard.writeText(keyView.key)}
              >
                <CopyIcon className="size-4" />
                复制
              </button>
            </div>
            <div className="modal-action">
              <button className="btn btn-neutral" onClick={() => setKeyView(null)}>
                <CheckIcon className="size-4" />
                关闭
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  )
}
