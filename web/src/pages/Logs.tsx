import { useCallback, useEffect, useState } from 'react'
import { api, type LogItem } from '../api'

export default function Logs() {
  const [logs, setLogs] = useState<LogItem[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      setLogs(await api<LogItem[]>('/api/logs?limit=100'))
    } catch (e) {
      setError(String(e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h1 className="text-2xl font-bold">调用记录</h1>
        <button className="btn btn-outline" onClick={load} disabled={loading}>
          刷新
        </button>
      </div>

      {error && <div className="alert alert-error mb-4">{error}</div>}

      <div className="overflow-x-auto bg-base-100 rounded-box shadow">
        <table className="table table-sm">
          <thead>
            <tr>
              <th>时间</th>
              <th>Key</th>
              <th>账号</th>
              <th>模型</th>
              <th>端点</th>
              <th>状态</th>
              <th>延迟</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={7} className="text-center">
                  <span className="loading loading-spinner" />
                </td>
              </tr>
            ) : logs.length === 0 ? (
              <tr>
                <td colSpan={7} className="text-center text-base-content/50">
                  暂无调用记录
                </td>
              </tr>
            ) : (
              logs.map((l) => (
                <tr key={l.id}>
                  <td className="whitespace-nowrap">{new Date(l.created_at).toLocaleString()}</td>
                  <td>{l.key_name || '-'}</td>
                  <td>{l.account_email || '-'}</td>
                  <td>{l.model || '-'}</td>
                  <td>
                    <code>{l.endpoint || '-'}</code>
                  </td>
                  <td>
                    <span
                      className={`badge ${
                        l.status >= 200 && l.status < 300
                          ? 'badge-success'
                          : l.status >= 500
                            ? 'badge-error'
                            : 'badge-warning'
                      }`}
                    >
                      {l.status}
                    </span>
                  </td>
                  <td>{l.latency_ms}ms</td>
                </tr>
              ))
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
