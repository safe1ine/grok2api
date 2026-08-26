import { CopyIcon, ExternalLinkIcon, PlusIcon, RotateCcwIcon, Trash2Icon, XIcon } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { api, type Account } from '../api'
import { ConfirmDialog } from '../components/Dialogs'

function statusBadge(status: string) {
  const map: Record<string, string> = {
    active: 'badge-success',
    cooldown: 'badge-warning',
    need_relogin: 'badge-error',
    disabled: 'badge-ghost',
  }
  return map[status] || 'badge-ghost'
}

const resetTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  timeZone: 'Asia/Shanghai',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  hour12: false,
})

const resetCreditWarningWindowMs = 3 * 24 * 60 * 60 * 1000

function resetCreditState(account: Account) {
  const expiresAt = account.reset_credit_expires_at
    ? new Date(account.reset_credit_expires_at).getTime()
    : null
  const expiresIn = expiresAt === null ? null : expiresAt - Date.now()
  const available = account.reset_credits_available > 0 && (expiresIn === null || expiresIn > 0)
  return {
    available,
    expiringSoon: available && expiresIn !== null && expiresIn <= resetCreditWarningWindowMs,
    title: !account.reset_credits_known
      ? '重置状态暂无'
      : !available
        ? '暂无重置次数'
        : expiresAt === null
          ? `有 ${account.reset_credits_available} 次重置`
          : `有 ${account.reset_credits_available} 次重置，过期时间：${resetTimeFormatter.format(new Date(expiresAt))}`,
  }
}

type AccountDialog = { kind: 'redeem' | 'delete'; account: Account }

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
  const [success, setSuccess] = useState('')
  const [redeemingId, setRedeemingId] = useState<number | null>(null)
  const [deletingId, setDeletingId] = useState<number | null>(null)
  const [accountDialog, setAccountDialog] = useState<AccountDialog | null>(null)

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

  async function redeemReset(account: Account) {
    const state = resetCreditState(account)
    if (!state.available || redeemingId !== null) return
    setRedeemingId(account.id)
    setError('')
    setSuccess('')
    try {
      await api(`/api/accounts/${account.id}/redeem-reset`, { method: 'POST' })
      setSuccess(`${account.email || `账号 ${account.id}`} 的周限已重置`)
      await load()
    } catch (e) {
      const message = String(e)
      await load()
      setError(message)
    } finally {
      setRedeemingId(null)
      setAccountDialog(null)
    }
  }

  async function remove(account: Account) {
    setDeletingId(account.id)
    setError('')
    try {
      await api(`/api/accounts/${account.id}`, { method: 'DELETE' })
      await load()
    } catch (e) {
      setError(String(e))
    } finally {
      setDeletingId(null)
      setAccountDialog(null)
    }
  }

  return (
    <section className="grid gap-6">
      <header className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <h1 className="text-2xl font-semibold tracking-tight">账号管理</h1>
        <button className="btn btn-neutral btn-sm self-start sm:self-auto" onClick={startAdd}>
          <PlusIcon className="size-4" />
          添加账号
        </button>
      </header>

      {error && <div className="alert border-error/20 bg-error/10 text-error">{error}</div>}
      {success && <div className="alert border-success/20 bg-success/10 text-success">{success}</div>}

      <div className="overflow-hidden rounded-2xl border border-base-300 bg-base-100 shadow-sm">
        <div className="border-b border-base-300 px-4 py-4 sm:px-5">
          <h2 className="font-semibold">账号池</h2>
          <p className="mt-0.5 text-xs text-base-content/50">当前共 {accounts.length} 个账号</p>
        </div>
        <div className="overflow-x-auto">
          <table className="table">
          <thead>
            <tr>
              <th>ID</th>
              <th>邮箱</th>
              <th>状态</th>
              <th>会员等级</th>
              <th>周限用量</th>
              <th>重置时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={7} className="text-center">
                  <span className="loading loading-spinner" />
                </td>
              </tr>
            ) : accounts.length === 0 ? (
              <tr>
                <td colSpan={7} className="text-center text-base-content/50">
                  还没有账号，点击右上角添加
                </td>
              </tr>
            ) : (
              accounts.map((a) => {
                const weeklyUsed = a.weekly_used_percent
                const resetCredit = resetCreditState(a)
                const redeeming = redeemingId === a.id
                const deleting = deletingId === a.id
                return (
                  <tr key={a.id}>
                    <td>{a.id}</td>
                    <td>{a.email || '-'}</td>
                    <td>
                      <span className={`badge ${statusBadge(a.status)}`}>{a.status}</span>
                    </td>
                    <td>
                      {a.subscription_tier ? (
                        <span className="badge badge-outline badge-sm whitespace-nowrap">{a.subscription_tier}</span>
                      ) : (
                        <span className="text-base-content/40">-</span>
                      )}
                    </td>
                    <td>
                      {weeklyUsed !== null ? (
                        <div className="flex items-center gap-2">
                          <progress className="progress progress-neutral w-24" value={weeklyUsed} max={100} />
                          <span className="whitespace-nowrap text-xs tabular-nums">已用 {weeklyUsed.toFixed(1)}%</span>
                        </div>
                      ) : (
                        <span className="text-base-content/40">-</span>
                      )}
                    </td>
                    <td className="whitespace-nowrap tabular-nums">
                      {a.weekly_reset_at ? resetTimeFormatter.format(new Date(a.weekly_reset_at)) : '-'}
                    </td>
                    <td>
                      <div className="flex items-center gap-2 whitespace-nowrap">
                        <div className="tooltip tooltip-left" data-tip={resetCredit.title}>
                          <button
                            className={`btn btn-xs ${resetCredit.expiringSoon ? 'btn-warning' : 'btn-outline'}`}
                            disabled={!resetCredit.available || redeeming || deleting}
                            onClick={() => setAccountDialog({ kind: 'redeem', account: a })}
                          >
                            {redeeming ? (
                              <span className="loading loading-spinner loading-xs" />
                            ) : (
                              <RotateCcwIcon className="size-3.5" />
                            )}
                            {redeeming ? '重置中' : '重置'}
                            {resetCredit.available && !redeeming && (
                              <span className="badge badge-sm">{a.reset_credits_available}</span>
                            )}
                          </button>
                        </div>
                        <button
                          className="btn btn-error btn-xs"
                          disabled={redeeming || deleting}
                          onClick={() => setAccountDialog({ kind: 'delete', account: a })}
                        >
                          {deleting ? <span className="loading loading-spinner loading-xs" /> : <Trash2Icon className="size-3.5" />}
                          {deleting ? '删除中' : '删除'}
                        </button>
                      </div>
                    </td>
                  </tr>
                )
              })
            )}
          </tbody>
          </table>
        </div>
      </div>

      <ConfirmDialog
        open={accountDialog !== null}
        title={accountDialog?.kind === 'redeem' ? '重置周限？' : '删除账号？'}
        description={accountDialog?.kind === 'redeem' ? (
          <>
            将立即重置 {accountDialog.account.email || `账号 ${accountDialog.account.id}`} 的周限，重置后次数为 {accountDialog.account.reset_credits_available - 1}。
            {accountDialog.account.reset_credit_expires_at && (
              <><br />最近过期时间：{resetTimeFormatter.format(new Date(accountDialog.account.reset_credit_expires_at))}。</>
            )}
          </>
        ) : accountDialog ? `确定删除 ${accountDialog.account.email || `账号 ${accountDialog.account.id}`}？删除后需要重新授权才能恢复。` : ''}
        confirmLabel={accountDialog?.kind === 'redeem' ? '立即重置' : '删除账号'}
        tone={accountDialog?.kind === 'redeem' ? 'warning' : 'danger'}
        pending={accountDialog?.kind === 'redeem' ? redeemingId !== null : deletingId !== null}
        onClose={() => setAccountDialog(null)}
        onConfirm={() => {
          if (!accountDialog) return
          if (accountDialog.kind === 'redeem') void redeemReset(accountDialog.account)
          else void remove(accountDialog.account)
        }}
      />

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
                    <ExternalLinkIcon className="size-4" />
                    打开链接
                  </button>
                  <button
                    className="btn btn-outline"
                    onClick={() => navigator.clipboard.writeText(device.verification_uri_complete)}
                  >
                    <CopyIcon className="size-4" />
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
              <button className="btn btn-ghost" onClick={closeModal}>
                <XIcon className="size-4" />
                关闭
              </button>
            </div>
          </div>
        </div>
      )}
    </section>
  )
}
