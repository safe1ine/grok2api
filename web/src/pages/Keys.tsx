import { useCallback, useEffect, useState } from 'react'
import { api, type KeyItem } from '../api'

export default function Keys() {
  const [keys, setKeys] = useState<KeyItem[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [newKey, setNewKey] = useState<{ name: string; key: string } | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      setKeys(await api<KeyItem[]>('/api/keys'))
    } catch (e) {
      setError(String(e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  async function create() {
    const name = prompt('给这个 Key 起个名字（备注）') || 'default'
    try {
      const res = await api<{ id: number; name: string; key: string }>('/api/keys', {
        method: 'POST',
        body: JSON.stringify({ name }),
      })
      setNewKey({ name: res.name, key: res.key })
      load()
    } catch (e) {
      setError(String(e))
    }
  }

  async function remove(id: number) {
    if (!confirm('确定删除该 Key？删除后立即失效。')) return
    try {
      await api(`/api/keys/${id}`, { method: 'DELETE' })
      load()
    } catch (e) {
      setError(String(e))
    }
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h1 className="text-2xl font-bold">密钥管理</h1>
        <button className="btn btn-primary" onClick={create}>
          创建 Key
        </button>
      </div>

      {error && <div className="alert alert-error mb-4">{error}</div>}

      <div className="overflow-x-auto bg-base-100 rounded-box shadow">
        <table className="table">
          <thead>
            <tr>
              <th>ID</th>
              <th>名称</th>
              <th>前缀</th>
              <th>状态</th>
              <th>创建时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={6} className="text-center">
                  <span className="loading loading-spinner" />
                </td>
              </tr>
            ) : keys.length === 0 ? (
              <tr>
                <td colSpan={6} className="text-center text-base-content/50">
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
                  <td>{new Date(k.created_at).toLocaleString()}</td>
                  <td>
                    <button className="btn btn-error btn-xs" onClick={() => remove(k.id)}>
                      删除
                    </button>
                  </td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>

      {newKey && (
        <div className="modal modal-open">
          <div className="modal-box">
            <h3 className="font-bold text-lg mb-2">Key 创建成功</h3>
            <p className="text-sm mb-4">请立即保存，明文只显示这一次：</p>
            <div className="flex gap-2">
              <input className="input input-bordered flex-1 font-mono" readOnly value={newKey.key} />
              <button
                className="btn btn-outline"
                onClick={() => navigator.clipboard.writeText(newKey.key)}
              >
                复制
              </button>
            </div>
            <div className="modal-action">
              <button className="btn" onClick={() => setNewKey(null)}>
                我已保存
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
