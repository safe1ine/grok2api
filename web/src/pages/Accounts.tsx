import { useCallback, useEffect, useState } from 'react'
import { api, type Account } from '../api'

function statusBadge(status: string) {
  const map: Record<string, string> = {
    active: 'badge-success',
    cooldown: 'badge-warning',
    need_relogin: 'badge-error',
    disabled: 'badge-ghost',
  }
  return map[status] || 'badge-ghost'
}

interface DeviceInfo {
  device_code: string
  user_code: string
  verification_uri: string
  verification_uri_complete: string
  expires_in: number
}

export default function Accounts() {
  const [accounts, setAccounts] = useState<Account[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')

  const [showModal, setShowModal] = useState(false)
  const [device, setDevice] = useState<DeviceInfo | null>(null)
  const [flowState, setFlowState] = useState<'pending' | 'complete' | 'failed' | ''>('')
  const [flowErr, setFlowErr] = useState('')

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      setAccounts(await api<Account[]>('/api/accounts'))
    } catch (e) {
      setError(String(e))
    } finally {
      setLoading(false)
    }
  }, [])

  useEffect(() => {
    load()
  }, [load])

  async function startAdd() {
    setShowModal(true)
    setFlowErr('')
    setFlowState('')
    setDevice(null)
    try {
      const d = await api<DeviceInfo>('/api/oauth/device', { method: 'POST' })
      setDevice(d)
      setFlowState('pending')
    } catch (e) {
      setFlowErr(String(e))
      setFlowState('failed')
    }
  }

  useEffect(() => {
    if (!device || flowState !== 'pending') return
    let stopped = false
    const timer = setInterval(async () => {
      try {
        const res = await api<{ state: string; error?: string }>(
          `/api/oauth/device/status?device_code=${encodeURIComponent(device.device_code)}`,
        )
        if (stopped) return
        if (res.state === 'complete') {
          setFlowState('complete')
          setShowModal(false)
          load()
        } else if (res.state === 'failed') {
          setFlowState('failed')
          setFlowErr(res.error || '登录失败')
        }
      } catch {
        // 网络抖动继续轮询
      }
    }, 3000)
    return () => {
      stopped = true
      clearInterval(timer)
    }
  }, [device, flowState, load])

  function closeModal() {
    setShowModal(false)
    setDevice(null)
    setFlowState('')
  }

  async function remove(id: number) {
    if (!confirm('确定删除该账号？')) return
    try {
      await api(`/api/accounts/${id}`, { method: 'DELETE' })
      load()
    } catch (e) {
      setError(String(e))
    }
  }

  return (
    <div>
      <div className="flex items-center justify-between mb-4">
        <h1 className="text-2xl font-bold">账号管理</h1>
        <button className="btn btn-primary" onClick={startAdd}>
          添加账号
        </button>
      </div>

      {error && <div className="alert alert-error mb-4">{error}</div>}

      <div className="overflow-x-auto bg-base-100 rounded-box shadow">
        <table className="table">
          <thead>
            <tr>
              <th>ID</th>
              <th>邮箱</th>
              <th>状态</th>
              <th>用量</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={5} className="text-center">
                  <span className="loading loading-spinner" />
                </td>
              </tr>
            ) : accounts.length === 0 ? (
              <tr>
                <td colSpan={5} className="text-center text-base-content/50">
                  还没有账号，点击右上角添加
                </td>
              </tr>
            ) : (
              accounts.map((a) => {
                const used =
                  a.rl_limit > 0 ? Math.max(0, 100 - (a.rl_remaining / a.rl_limit) * 100) : 0
                return (
                  <tr key={a.id}>
                    <td>{a.id}</td>
                    <td>{a.email || '-'}</td>
                    <td>
                      <span className={`badge ${statusBadge(a.status)}`}>{a.status}</span>
                    </td>
                    <td>
                      {a.rl_limit > 0 ? (
                        <div className="flex flex-col gap-1">
                          <div className="flex items-center gap-2">
                            <progress
                              className="progress progress-success w-24"
                              value={used}
                              max={100}
                            />
                            <span className="text-xs whitespace-nowrap">
                              {a.rl_remaining}/{a.rl_limit} 次
                            </span>
                          </div>
                          {a.rl_token_limit > 0 && (
                            <span className="text-xs text-base-content/60 whitespace-nowrap">
                              {a.rl_token_remaining}/{a.rl_token_limit} tokens
                            </span>
                          )}
                        </div>
                      ) : (
                        <span className="text-base-content/40">-</span>
                      )}
                    </td>
                    <td>
                      <button className="btn btn-error btn-xs" onClick={() => remove(a.id)}>
                        删除
                      </button>
                    </td>
                  </tr>
                )
              })
            )}
          </tbody>
        </table>
      </div>

      {showModal && (
        <div className="modal modal-open">
          <div className="modal-box max-w-2xl">
            <h3 className="font-bold text-lg mb-4">添加 Grok 账号</h3>

            {!device ? (
              <div className="flex items-center gap-2">
                <span className="loading loading-spinner loading-sm" />
                <span>正在生成登录链接...</span>
              </div>
            ) : (
              <div className="space-y-4">
                <div className="text-sm">打开下面的链接，登录 xAI 账号并完成授权：</div>
                <div className="flex gap-2">
                  <input
                    className="input input-bordered flex-1"
                    readOnly
                    value={device.verification_uri_complete}
                  />
                  <button
                    className="btn btn-outline"
                    onClick={() => window.open(device.verification_uri_complete, '_blank')}
                  >
                    打开链接
                  </button>
                  <button
                    className="btn btn-outline"
                    onClick={() => navigator.clipboard.writeText(device.verification_uri_complete)}
                  >
                    复制
                  </button>
                </div>
                <div className="text-sm text-base-content/70">
                  或在授权页面手动输入代码：
                  <span className="badge badge-lg font-mono ml-1">{device.user_code}</span>
                </div>

                {flowState === 'pending' && (
                  <div className="flex items-center gap-2 text-sm">
                    <span className="loading loading-spinner loading-sm" />
                    等待授权中，授权完成后会自动关闭...
                  </div>
                )}
                {flowState === 'failed' && (
                  <div className="alert alert-error text-sm">{flowErr}</div>
                )}
              </div>
            )}

            <div className="modal-action">
              <button className="btn" onClick={closeModal}>
                关闭
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
