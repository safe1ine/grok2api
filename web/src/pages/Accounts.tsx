import { ChevronDownIcon, ChevronRightIcon, CopyIcon, EllipsisIcon, ExternalLinkIcon, FolderCogIcon, FolderInputIcon, PencilIcon, PlusIcon, PowerIcon, PowerOffIcon, RotateCcwIcon, SlidersHorizontalIcon, Trash2Icon, XIcon } from 'lucide-react'
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { api, type Account, type AccountGroup } from '../api'
import { ConfirmDialog } from '../components/Dialogs'

function statusBadge(status: string) {
  const map: Record<string, string> = {
    active: 'badge-success',
    cooldown: 'badge-warning',
    exhausted: 'badge-error',
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

const minuteMs = 60 * 1000
const hourMs = 60 * minuteMs
const dayMs = 24 * hourMs
const resetCreditWarningWindowMs = 3 * dayMs

function formatResetCountdown(value: string) {
  const remaining = new Date(value).getTime() - Date.now()
  if (!Number.isFinite(remaining)) return '-'
  if (remaining <= 0) return '即将重置'
  if (remaining >= dayMs) return `${Math.floor(remaining / dayMs)} 天后重置`
  if (remaining >= hourMs) return `${Math.floor(remaining / hourMs)} 小时后重置`
  if (remaining >= minuteMs) return `${Math.floor(remaining / minuteMs)} 分钟后重置`
  return '1 分钟内重置'
}

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

type AccountDialog = { kind: 'redeem' | 'disable' | 'enable' | 'delete'; account: Account }
type AccountMenu = { account: Account; top: number; left: number }

function initialCollapsedGroups() {
  try {
    const value = JSON.parse(localStorage.getItem(collapsedGroupsStorageKey) || '[]')
    return new Set<number>(Array.isArray(value) ? value.filter(Number.isInteger) : [])
  } catch {
    return new Set<number>()
  }
}

const accountMenuWidth = 176
const accountMenuFallbackHeight = 216
const accountMenuViewportGap = 8
const collapsedGroupsStorageKey = 'grok2api_collapsed_account_groups'
const maxSchedulingWeight = 1000

function accountMenuPosition(trigger: HTMLButtonElement, menuHeight = accountMenuFallbackHeight) {
  const rect = trigger.getBoundingClientRect()
  const maxLeft = Math.max(accountMenuViewportGap, window.innerWidth - accountMenuWidth - accountMenuViewportGap)
  const left = Math.min(Math.max(accountMenuViewportGap, rect.right - accountMenuWidth), maxLeft)
  const fitsBelow = rect.bottom + accountMenuViewportGap + menuHeight <= window.innerHeight
  const top = fitsBelow
    ? rect.bottom + accountMenuViewportGap
    : Math.max(accountMenuViewportGap, rect.top - accountMenuViewportGap - menuHeight)
  return { top, left }
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
  const [groups, setGroups] = useState<AccountGroup[]>([])
  const [collapsedGroups, setCollapsedGroups] = useState<Set<number>>(initialCollapsedGroups)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [success, setSuccess] = useState('')
  const [redeemingId, setRedeemingId] = useState<number | null>(null)
  const [togglingId, setTogglingId] = useState<number | null>(null)
  const [updatingWeightId, setUpdatingWeightId] = useState<number | null>(null)
  const [weightDialog, setWeightDialog] = useState<Account | null>(null)
  const [weightValue, setWeightValue] = useState('1')
  const [weightError, setWeightError] = useState('')
  const [deletingId, setDeletingId] = useState<number | null>(null)
  const [accountDialog, setAccountDialog] = useState<AccountDialog | null>(null)
  const [accountMenu, setAccountMenu] = useState<AccountMenu | null>(null)
  const [showGroupManager, setShowGroupManager] = useState(false)
  const [groupDrafts, setGroupDrafts] = useState<Record<number, string>>({})
  const [newGroupName, setNewGroupName] = useState('')
  const [groupBusy, setGroupBusy] = useState<number | 'new' | null>(null)
  const [groupError, setGroupError] = useState('')
  const [groupDialog, setGroupDialog] = useState<Account | null>(null)
  const [selectedGroupID, setSelectedGroupID] = useState(0)
  const [updatingGroup, setUpdatingGroup] = useState(false)
  const accountMenuRef = useRef<HTMLUListElement | null>(null)
  const accountMenuTriggerRef = useRef<HTMLButtonElement | null>(null)

  const [showModal, setShowModal] = useState(false)
  const [device, setDevice] = useState<DeviceInfo | null>(null)
  const [flowState, setFlowState] = useState<'pending' | 'complete' | 'failed' | ''>('')
  const [flowErr, setFlowErr] = useState('')

  const load = useCallback(async (showLoading = true) => {
    if (showLoading) setLoading(true)
    setError('')
    try {
      const [loadedAccounts, loadedGroups] = await Promise.all([
        api<Account[]>('/api/accounts'),
        api<AccountGroup[]>('/api/account-groups'),
      ])
      setAccounts(loadedAccounts)
      setGroups(loadedGroups)
    } catch (e) {
      setError(String(e))
    } finally {
      if (showLoading) setLoading(false)
    }
  }, [])

  useEffect(() => {
    void load()
    const timer = window.setInterval(() => void load(false), 5 * 60 * 1000)
    return () => window.clearInterval(timer)
  }, [load])

  useLayoutEffect(() => {
    if (!accountMenu || !accountMenuRef.current || !accountMenuTriggerRef.current) return
    const position = accountMenuPosition(accountMenuTriggerRef.current, accountMenuRef.current.offsetHeight)
    if (position.top === accountMenu.top && position.left === accountMenu.left) return
    setAccountMenu((current) => current ? { ...current, ...position } : null)
  }, [accountMenu])

  useEffect(() => {
    if (!accountMenu) return

    const close = () => setAccountMenu(null)
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target as Node
      if (accountMenuRef.current?.contains(target) || accountMenuTriggerRef.current?.contains(target)) return
      close()
    }
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key !== 'Escape') return
      close()
      accountMenuTriggerRef.current?.focus()
    }

    document.addEventListener('pointerdown', handlePointerDown)
    document.addEventListener('keydown', handleKeyDown)
    window.addEventListener('resize', close)
    window.addEventListener('scroll', close, true)
    return () => {
      document.removeEventListener('pointerdown', handlePointerDown)
      document.removeEventListener('keydown', handleKeyDown)
      window.removeEventListener('resize', close)
      window.removeEventListener('scroll', close, true)
    }
  }, [accountMenu])

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

  async function setSchedulingDisabled(account: Account, disabled: boolean) {
    setTogglingId(account.id)
    setError('')
    setSuccess('')
    try {
      await api(`/api/accounts/${account.id}/${disabled ? 'disable' : 'enable'}`, { method: 'POST' })
      setSuccess(`${account.email || `账号 ${account.id}`} 已${disabled ? '禁用' : '启用'}`)
      await load()
    } catch (e) {
      setError(String(e))
    } finally {
      setTogglingId(null)
      setAccountDialog(null)
    }
  }

  function openWeightDialog(account: Account) {
    setWeightDialog(account)
    setWeightValue(String(account.scheduling_weight))
    setWeightError('')
  }

  async function updateWeight() {
    if (!weightDialog) return
    const weight = Number(weightValue)
    if (!Number.isInteger(weight) || weight < 1 || weight > maxSchedulingWeight) {
      setWeightError(`请输入 1 到 ${maxSchedulingWeight} 之间的整数`)
      return
    }
    setUpdatingWeightId(weightDialog.id)
    setWeightError('')
    setError('')
    setSuccess('')
    try {
      await api(`/api/accounts/${weightDialog.id}/weight`, {
        method: 'PUT',
        body: JSON.stringify({ scheduling_weight: weight }),
      })
      setSuccess(`${weightDialog.email || `账号 ${weightDialog.id}`} 的权重已更新为 ${weight}`)
      await load()
      setWeightDialog(null)
    } catch (e) {
      setWeightError(String(e))
    } finally {
      setUpdatingWeightId(null)
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

  function toggleGroup(groupID: number) {
    setCollapsedGroups((current) => {
      const next = new Set(current)
      if (next.has(groupID)) next.delete(groupID)
      else next.add(groupID)
      localStorage.setItem(collapsedGroupsStorageKey, JSON.stringify([...next]))
      return next
    })
  }

  function openGroupManager() {
    setGroupDrafts(Object.fromEntries(groups.map((group) => [group.id, group.name])))
    setNewGroupName('')
    setGroupError('')
    setShowGroupManager(true)
  }

  async function createGroup() {
    if (groupBusy !== null || !newGroupName.trim()) return
    setGroupBusy('new')
    setGroupError('')
    try {
      const created = await api<AccountGroup>('/api/account-groups', { method: 'POST', body: JSON.stringify({ name: newGroupName }) })
      setNewGroupName('')
      setGroupDrafts((current) => ({ ...current, [created.id]: created.name }))
      await load(false)
    } catch (e) {
      setGroupError(String(e))
    } finally {
      setGroupBusy(null)
    }
  }

  async function renameGroup(group: AccountGroup) {
    const name = groupDrafts[group.id]?.trim()
    if (groupBusy !== null || !name || name === group.name) return
    setGroupBusy(group.id)
    setGroupError('')
    try {
      await api(`/api/account-groups/${group.id}`, { method: 'PUT', body: JSON.stringify({ name }) })
      await load(false)
    } catch (e) {
      setGroupError(String(e))
    } finally {
      setGroupBusy(null)
    }
  }

  async function deleteGroup(group: AccountGroup) {
    if (groupBusy !== null || group.is_default) return
    if (!window.confirm(`删除分组“${group.name}”？组内账号将移入默认分组。`)) return
    setGroupBusy(group.id)
    setGroupError('')
    try {
      await api(`/api/account-groups/${group.id}`, { method: 'DELETE' })
      setCollapsedGroups((current) => {
        const next = new Set(current)
        next.delete(group.id)
        localStorage.setItem(collapsedGroupsStorageKey, JSON.stringify([...next]))
        return next
      })
      await load(false)
    } catch (e) {
      setGroupError(String(e))
    } finally {
      setGroupBusy(null)
    }
  }

  function openGroupDialog(account: Account) {
    setGroupDialog(account)
    setSelectedGroupID(account.group_id)
    setGroupError('')
  }

  async function updateAccountGroup() {
    if (!groupDialog || selectedGroupID <= 0 || updatingGroup) return
    setUpdatingGroup(true)
    setGroupError('')
    try {
      await api(`/api/accounts/${groupDialog.id}/group`, {
        method: 'PUT',
        body: JSON.stringify({ group_id: selectedGroupID }),
      })
      await load(false)
      setGroupDialog(null)
    } catch (e) {
      setGroupError(String(e))
    } finally {
      setUpdatingGroup(false)
    }
  }

  const groupedAccounts = groups.map((group) => ({
    group,
    accounts: accounts.filter((account) => account.group_id === group.id),
  }))
  const menuResetCredit = accountMenu ? resetCreditState(accountMenu.account) : null

  return (
    <section className="grid gap-6">
      <header className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <h1 className="text-2xl font-semibold tracking-tight">账号管理</h1>
        <div className="flex items-center gap-2 self-start sm:self-auto">
          <button className="btn btn-outline btn-sm" onClick={openGroupManager}>
            <FolderCogIcon className="size-4" />
            管理分组
          </button>
          <button className="btn btn-neutral btn-sm" onClick={startAdd}>
            <PlusIcon className="size-4" />
            添加账号
          </button>
        </div>
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
              <th>权重</th>
              <th>会员等级</th>
              <th>周限用量</th>
              <th>重置次数</th>
              <th>重置时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={9} className="text-center">
                  <span className="loading loading-spinner" />
                </td>
              </tr>
            ) : accounts.length === 0 ? (
              <tr>
                <td colSpan={9} className="text-center text-base-content/50">
                  还没有账号，点击右上角添加
                </td>
              </tr>
            ) : (
              groupedAccounts.flatMap(({ group, accounts: groupAccounts }) => [
                <tr key={`group-${group.id}`} className="bg-base-200/70">
                  <td colSpan={9} className="p-0">
                    <button
                      type="button"
                      className="flex w-full items-center gap-2 px-4 py-3 text-left font-medium hover:bg-base-200"
                      aria-expanded={!collapsedGroups.has(group.id)}
                      onClick={() => toggleGroup(group.id)}
                    >
                      {collapsedGroups.has(group.id)
                        ? <ChevronRightIcon className="size-4 shrink-0" />
                        : <ChevronDownIcon className="size-4 shrink-0" />}
                      <span>{group.name}</span>
                      {group.is_default && <span className="badge badge-ghost badge-xs">默认</span>}
                      <span className="text-xs font-normal text-base-content/50">{groupAccounts.length} 个账号</span>
                    </button>
                  </td>
                </tr>,
                ...(collapsedGroups.has(group.id) ? [] : groupAccounts.map((a) => {
                const weeklyUsed = a.weekly_used_percent
                const redeeming = redeemingId === a.id
                const toggling = togglingId === a.id
                const updatingWeight = updatingWeightId === a.id
                const deleting = deletingId === a.id
                const busy = redeeming || toggling || updatingWeight || deleting
                return (
                  <tr key={a.id}>
                    <td>{a.id}</td>
                    <td>{a.email || '-'}</td>
                    <td>
                      <span className={`badge ${statusBadge(a.status)}`}>{a.status}</span>
                    </td>
                    <td className="tabular-nums">{a.scheduling_weight}</td>
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
                          <span className="whitespace-nowrap text-xs tabular-nums">{Math.round(weeklyUsed)}%</span>
                        </div>
                      ) : (
                        <span className="text-base-content/40">-</span>
                      )}
                    </td>
                    <td className="tabular-nums">
                      {a.reset_credits_known ? a.reset_credits_available : <span className="text-base-content/40">未知</span>}
                    </td>
                    <td className="whitespace-nowrap tabular-nums">
                      {a.weekly_reset_at ? formatResetCountdown(a.weekly_reset_at) : '-'}
                    </td>
                    <td>
                      <button
                        type="button"
                        className="btn btn-ghost btn-xs btn-square"
                        aria-label={`打开 ${a.email || `账号 ${a.id}`} 的操作菜单`}
                        aria-haspopup="menu"
                        aria-expanded={accountMenu?.account.id === a.id}
                        disabled={busy}
                        onClick={(event) => {
                          if (accountMenu?.account.id === a.id) {
                            setAccountMenu(null)
                            return
                          }
                          accountMenuTriggerRef.current = event.currentTarget
                          setAccountMenu({ account: a, ...accountMenuPosition(event.currentTarget) })
                        }}
                      >
                        {busy ? <span className="loading loading-spinner loading-xs" /> : <EllipsisIcon className="size-4" />}
                      </button>
                    </td>
                  </tr>
                )
              })),
              ])
            )}
          </tbody>
          </table>
        </div>
      </div>

      {accountMenu && menuResetCredit && createPortal(
        <ul
          ref={accountMenuRef}
          role="menu"
          aria-label={`${accountMenu.account.email || `账号 ${accountMenu.account.id}`} 的操作`}
          className="menu fixed z-50 w-44 rounded-box border border-base-300 bg-base-100 p-2 shadow-xl"
          style={{ top: accountMenu.top, left: accountMenu.left }}
        >
          <li>
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                const account = accountMenu.account
                setAccountMenu(null)
                openWeightDialog(account)
              }}
            >
              <SlidersHorizontalIcon className="size-4" />
              调整权重
            </button>
          </li>
          <li>
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                const account = accountMenu.account
                setAccountMenu(null)
                openGroupDialog(account)
              }}
            >
              <FolderInputIcon className="size-4" />
              调整分组
            </button>
          </li>
          <li>
            <button
              type="button"
              role="menuitem"
              onClick={() => {
                const account = accountMenu.account
                setAccountMenu(null)
                setAccountDialog({ kind: account.scheduling_disabled ? 'enable' : 'disable', account })
              }}
            >
              {accountMenu.account.scheduling_disabled ? <PowerIcon className="size-4" /> : <PowerOffIcon className="size-4" />}
              {accountMenu.account.scheduling_disabled ? '启用账号' : '禁用账号'}
            </button>
          </li>
          <li>
            <button
              type="button"
              role="menuitem"
              title={menuResetCredit.title}
              onClick={() => {
                const account = accountMenu.account
                setAccountMenu(null)
                setAccountDialog({ kind: 'redeem', account })
              }}
            >
              <RotateCcwIcon className="size-4" />
              重置周限
              {menuResetCredit.available && <span className="badge badge-sm">{accountMenu.account.reset_credits_available}</span>}
            </button>
          </li>
          <li>
            <button
              type="button"
              role="menuitem"
              className="text-error"
              onClick={() => {
                const account = accountMenu.account
                setAccountMenu(null)
                setAccountDialog({ kind: 'delete', account })
              }}
            >
              <Trash2Icon className="size-4" />
              删除账号
            </button>
          </li>
        </ul>,
        document.body,
      )}

      {showGroupManager && (
        <dialog className="modal modal-open" aria-labelledby="group-manager-title">
          <div className="modal-box max-w-xl">
            <button
              type="button"
              className="btn btn-ghost btn-sm btn-circle absolute right-3 top-3"
              aria-label="关闭分组管理"
              disabled={groupBusy !== null}
              onClick={() => setShowGroupManager(false)}
            >
              <XIcon className="size-4" />
            </button>
            <h3 id="group-manager-title" className="text-lg font-semibold">管理分组</h3>
            <p className="mt-1 text-sm text-base-content/60">删除分组后，组内账号会自动移入默认分组。</p>

            <div className="mt-5 grid gap-3">
              {groups.map((group) => (
                <div key={group.id} className="flex items-center gap-2 rounded-xl border border-base-300 p-3">
                  <input
                    className="input input-bordered input-sm min-w-0 flex-1"
                    value={groupDrafts[group.id] ?? group.name}
                    maxLength={50}
                    disabled={groupBusy !== null}
                    aria-label={`${group.name}的名称`}
                    onChange={(event) => setGroupDrafts((current) => ({ ...current, [group.id]: event.target.value }))}
                  />
                  {group.is_default && <span className="badge badge-ghost badge-sm shrink-0">默认</span>}
                  <button
                    type="button"
                    className="btn btn-ghost btn-sm btn-square"
                    title="保存名称"
                    disabled={groupBusy !== null || !(groupDrafts[group.id] ?? '').trim() || groupDrafts[group.id]?.trim() === group.name}
                    onClick={() => void renameGroup(group)}
                  >
                    {groupBusy === group.id ? <span className="loading loading-spinner loading-xs" /> : <PencilIcon className="size-4" />}
                  </button>
                  <button
                    type="button"
                    className="btn btn-ghost btn-sm btn-square text-error"
                    title={group.is_default ? '默认分组不能删除' : '删除分组'}
                    disabled={groupBusy !== null || group.is_default}
                    onClick={() => void deleteGroup(group)}
                  >
                    <Trash2Icon className="size-4" />
                  </button>
                </div>
              ))}
            </div>

            <div className="mt-4 flex gap-2 border-t border-base-300 pt-4">
              <input
                className="input input-bordered input-sm min-w-0 flex-1"
                placeholder="新分组名称"
                value={newGroupName}
                maxLength={50}
                disabled={groupBusy !== null}
                onChange={(event) => setNewGroupName(event.target.value)}
                onKeyDown={(event) => {
                  if (event.key === 'Enter') void createGroup()
                }}
              />
              <button
                type="button"
                className="btn btn-neutral btn-sm"
                disabled={groupBusy !== null || !newGroupName.trim()}
                onClick={() => void createGroup()}
              >
                {groupBusy === 'new' ? <span className="loading loading-spinner loading-xs" /> : <PlusIcon className="size-4" />}
                新建分组
              </button>
            </div>
            {groupError && <p className="mt-3 text-sm text-error">{groupError}</p>}
            <div className="modal-action">
              <button type="button" className="btn" disabled={groupBusy !== null} onClick={() => setShowGroupManager(false)}>完成</button>
            </div>
          </div>
          <button
            type="button"
            className="modal-backdrop cursor-default"
            aria-label="关闭分组管理"
            disabled={groupBusy !== null}
            onClick={() => setShowGroupManager(false)}
          />
        </dialog>
      )}

      {groupDialog && (
        <dialog className="modal modal-open" aria-labelledby="account-group-dialog-title">
          <div className="modal-box max-w-md">
            <h3 id="account-group-dialog-title" className="text-lg font-semibold">调整分组</h3>
            <p className="mt-2 text-sm text-base-content/60">{groupDialog.email || `账号 ${groupDialog.id}`}</p>
            <select
              className="select select-bordered mt-5 w-full"
              value={selectedGroupID}
              disabled={updatingGroup}
              onChange={(event) => setSelectedGroupID(Number(event.target.value))}
            >
              {groups.map((group) => <option key={group.id} value={group.id}>{group.name}</option>)}
            </select>
            {groupError && <p className="mt-3 text-sm text-error">{groupError}</p>}
            <div className="modal-action">
              <button type="button" className="btn" disabled={updatingGroup} onClick={() => setGroupDialog(null)}>取消</button>
              <button
                type="button"
                className="btn btn-neutral"
                disabled={updatingGroup || selectedGroupID === groupDialog.group_id}
                onClick={() => void updateAccountGroup()}
              >
                {updatingGroup && <span className="loading loading-spinner loading-xs" />}
                保存
              </button>
            </div>
          </div>
          <button
            type="button"
            className="modal-backdrop cursor-default"
            aria-label="关闭调整分组"
            disabled={updatingGroup}
            onClick={() => setGroupDialog(null)}
          />
        </dialog>
      )}

      <ConfirmDialog
        open={accountDialog !== null}
        title={accountDialog?.kind === 'redeem'
          ? '重置周限？'
          : accountDialog?.kind === 'disable'
            ? '禁用账号？'
            : accountDialog?.kind === 'enable'
              ? '启用账号？'
              : '删除账号？'}
        description={accountDialog?.kind === 'redeem' ? (
          <>
            将查询 {accountDialog.account.email || `账号 ${accountDialog.account.id}`} 当前可用的重置机会并尝试重置
            {accountDialog.account.reset_credits_known && accountDialog.account.reset_credits_available > 0
              ? `，成功后剩余次数为 ${accountDialog.account.reset_credits_available - 1}。`
              : '。'}
            {accountDialog.account.reset_credit_expires_at && (
              <><br />最近过期时间：{resetTimeFormatter.format(new Date(accountDialog.account.reset_credit_expires_at))}。</>
            )}
          </>
        ) : accountDialog?.kind === 'disable'
          ? `禁用 ${accountDialog.account.email || `账号 ${accountDialog.account.id}`} 后将不再分配新请求，正在处理的请求不会中断。`
          : accountDialog?.kind === 'enable'
            ? `启用 ${accountDialog.account.email || `账号 ${accountDialog.account.id}`} 后将重新允许参与调度，实际可用状态仍取决于额度和登录状态。`
            : accountDialog
              ? `确定删除 ${accountDialog.account.email || `账号 ${accountDialog.account.id}`}？删除后需要重新授权才能恢复。`
              : ''}
        confirmLabel={accountDialog?.kind === 'redeem'
          ? '立即重置'
          : accountDialog?.kind === 'disable'
            ? '确认禁用'
            : accountDialog?.kind === 'enable'
              ? '确认启用'
              : '删除账号'}
        tone={accountDialog?.kind === 'delete' ? 'danger' : accountDialog?.kind === 'enable' ? 'neutral' : 'warning'}
        pending={accountDialog?.kind === 'redeem'
          ? redeemingId !== null
          : accountDialog?.kind === 'disable' || accountDialog?.kind === 'enable'
            ? togglingId !== null
            : deletingId !== null}
        onClose={() => setAccountDialog(null)}
        onConfirm={() => {
          if (!accountDialog) return
          if (accountDialog.kind === 'redeem') void redeemReset(accountDialog.account)
          else if (accountDialog.kind === 'disable') void setSchedulingDisabled(accountDialog.account, true)
          else if (accountDialog.kind === 'enable') void setSchedulingDisabled(accountDialog.account, false)
          else void remove(accountDialog.account)
        }}
      />

      {weightDialog && (
        <div className="modal modal-open" role="dialog" aria-modal="true" aria-labelledby="weight-dialog-title">
          <form
            className="modal-box max-w-md rounded-2xl border border-base-300 shadow-xl"
            onSubmit={(event) => {
              event.preventDefault()
              if (updatingWeightId === null) void updateWeight()
            }}
          >
            <h3 id="weight-dialog-title" className="text-lg font-semibold">调整权重</h3>
            <p className="mt-2 text-sm text-base-content/60">
              {weightDialog.email || `账号 ${weightDialog.id}`}，权重越大，分配到的请求越多。
            </p>
            <label className="form-control mt-5 gap-2">
              <span className="label-text">调度权重（1–1000）</span>
              <input
                autoFocus
                className={`input input-bordered w-full ${weightError ? 'input-error' : ''}`}
                type="number"
                min={1}
                max={maxSchedulingWeight}
                step={1}
                value={weightValue}
                disabled={updatingWeightId !== null}
                onChange={(event) => {
                  setWeightValue(event.target.value)
                  setWeightError('')
                }}
              />
              {weightError && <span className="text-sm text-error">{weightError}</span>}
            </label>
            <div className="modal-action">
              <button
                type="button"
                className="btn btn-ghost"
                disabled={updatingWeightId !== null}
                onClick={() => setWeightDialog(null)}
              >
                取消
              </button>
              <button type="submit" className="btn btn-neutral" disabled={updatingWeightId !== null}>
                {updatingWeightId !== null && <span className="loading loading-spinner loading-sm" />}
                {updatingWeightId !== null ? '保存中...' : '保存'}
              </button>
            </div>
          </form>
          <button
            type="button"
            className="modal-backdrop cursor-default"
            aria-label="关闭权重对话框"
            disabled={updatingWeightId !== null}
            onClick={() => setWeightDialog(null)}
          />
        </div>
      )}

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
