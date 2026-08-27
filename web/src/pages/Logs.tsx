import { Columns3Icon, RefreshCwIcon } from 'lucide-react'
import { useCallback, useEffect, useState } from 'react'
import { api, type LogItem, type LogListResponse } from '../api'
import { formatTokenCount } from '../format'

function formatTokens(value: number) {
  return value > 0 ? formatTokenCount(value) : '-'
}

function uncachedInputTokens(promptTokens: number, cachedTokens: number) {
  return Math.max(0, promptTokens - cachedTokens)
}

function cacheHitRate(promptTokens: number, cachedTokens: number) {
  if (promptTokens <= 0) return null
  const cached = Math.min(promptTokens, Math.max(0, cachedTokens))
  return (cached / promptTokens) * 100
}

function formatCacheHitRate(promptTokens: number, cachedTokens: number) {
  const rate = cacheHitRate(promptTokens, cachedTokens)
  return rate === null ? '-' : `${rate.toFixed(1)}%`
}

function cacheHitRateColor(promptTokens: number, cachedTokens: number) {
  const rate = cacheHitRate(promptTokens, cachedTokens)
  if (rate !== null && rate < 90) return 'text-error font-semibold'
  if (rate !== null && rate < 98) return 'text-orange-500 font-semibold'
  return ''
}

function formatDuration(value: number) {
  return value > 0 ? `${(value / 1000).toFixed(1)}s` : '-'
}

function durationColor(value: number, warningMs: number, errorMs: number) {
  if (value > errorMs) return 'text-error font-semibold'
  if (value > warningMs) return 'text-warning font-semibold'
  return ''
}

const logTimeFormatter = new Intl.DateTimeFormat('zh-CN', {
  timeZone: 'Asia/Shanghai',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hour12: false,
})

type ColumnKey =
  | 'time'
  | 'key'
  | 'account'
  | 'model'
  | 'endpoint'
  | 'status'
  | 'input'
  | 'cached'
  | 'cacheHit'
  | 'output'
  | 'ttft'
  | 'latency'
  | 'stream'

type ColumnVisibility = Record<ColumnKey, boolean>

const columnOptions: Array<{ key: ColumnKey; label: string }> = [
  { key: 'time', label: '时间' },
  { key: 'key', label: 'Key' },
  { key: 'account', label: '账号' },
  { key: 'model', label: '模型' },
  { key: 'endpoint', label: '端点' },
  { key: 'status', label: '状态' },
  { key: 'input', label: '输入' },
  { key: 'cached', label: '缓存' },
  { key: 'cacheHit', label: '缓存命中' },
  { key: 'output', label: '输出' },
  { key: 'ttft', label: '首字延迟' },
  { key: 'latency', label: '总耗时' },
  { key: 'stream', label: '流式' },
]

const defaultColumnVisibility: ColumnVisibility = {
  time: true,
  key: false,
  account: false,
  model: false,
  endpoint: false,
  status: true,
  input: true,
  cached: false,
  cacheHit: true,
  output: true,
  ttft: true,
  latency: true,
  stream: true,
}

const legacyColumnStorageKey = 'grok2api.logs.visible-columns'
const columnStorageKey = 'grok2api.logs.visible-columns.v2'

function loadColumnVisibility(): ColumnVisibility {
  try {
    const current = window.localStorage.getItem(columnStorageKey)
    const stored = JSON.parse(current || window.localStorage.getItem(legacyColumnStorageKey) || '{}') as Partial<ColumnVisibility>
    return Object.fromEntries(
      columnOptions.map(({ key }) => [
        key,
        current === null && key === 'cached'
          ? false
          : typeof stored[key] === 'boolean'
            ? stored[key]
            : defaultColumnVisibility[key],
      ]),
    ) as ColumnVisibility
  } catch {
    return defaultColumnVisibility
  }
}

export default function Logs() {
  const [logs, setLogs] = useState<LogItem[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(50)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [visibleColumns, setVisibleColumns] = useState<ColumnVisibility>(loadColumnVisibility)

  const load = useCallback(async () => {
    setLoading(true)
    setError('')
    try {
      const offset = (page - 1) * pageSize
      const data = await api<LogListResponse>(`/api/logs?limit=${pageSize}&offset=${offset}`)
      const lastPage = Math.max(1, Math.ceil(data.total / pageSize))
      if (page > lastPage) {
        setPage(lastPage)
        return
      }
      setLogs(data.items)
      setTotal(data.total)
    } catch (e) {
      setError(String(e))
    } finally {
      setLoading(false)
    }
  }, [page, pageSize])

  useEffect(() => {
    load()
  }, [load])

  useEffect(() => {
    window.localStorage.setItem(columnStorageKey, JSON.stringify(visibleColumns))
  }, [visibleColumns])

  const visibleColumnCount = Object.values(visibleColumns).filter(Boolean).length
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const firstItem = total === 0 ? 0 : (page - 1) * pageSize + 1
  const lastItem = Math.min(page * pageSize, total)

  return (
    <section className="grid gap-6">
      <header className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <h1 className="text-2xl font-semibold tracking-tight">调用记录</h1>
        <div className="flex gap-2 self-start sm:self-auto">
          <div className="dropdown dropdown-end">
            <button type="button" tabIndex={0} className="btn btn-outline btn-sm">
              <Columns3Icon className="size-4" />
              显示列
            </button>
            <ul
              tabIndex={0}
              className="dropdown-content menu z-30 mt-2 w-44 rounded-box border border-base-300 bg-base-100 p-2 shadow"
            >
              {columnOptions.map((column) => (
                <li key={column.key}>
                  <label className="flex cursor-pointer items-center gap-3">
                    <input
                      type="checkbox"
                      className="checkbox checkbox-sm"
                      checked={visibleColumns[column.key]}
                      disabled={visibleColumns[column.key] && visibleColumnCount === 1}
                      onChange={(event) => {
                        setVisibleColumns((current) => ({ ...current, [column.key]: event.target.checked }))
                      }}
                    />
                    <span>{column.label}</span>
                  </label>
                </li>
              ))}
            </ul>
          </div>
          <button className="btn btn-outline btn-sm" onClick={load} disabled={loading}>
            <RefreshCwIcon className={loading ? 'size-4 animate-spin' : 'size-4'} />
            刷新
          </button>
        </div>
      </header>

      {error && <div className="alert alert-error">{error}</div>}

      <div className="overflow-hidden rounded-2xl border border-base-300 bg-base-100 shadow-sm">
        <div className="border-b border-base-300 px-4 py-4 sm:px-5">
          <h2 className="font-semibold">请求明细</h2>
          <p className="mt-0.5 text-xs text-base-content/50">共记录 {total} 次调用</p>
        </div>
        <div className="overflow-x-auto">
          <table className="table whitespace-nowrap">
          <thead>
            <tr>
              {visibleColumns.time && <th>时间</th>}
              {visibleColumns.key && <th>Key</th>}
              {visibleColumns.account && <th>账号</th>}
              {visibleColumns.model && <th>模型</th>}
              {visibleColumns.endpoint && <th>端点</th>}
              {visibleColumns.status && <th>状态</th>}
              {visibleColumns.input && <th className="text-right">输入</th>}
              {visibleColumns.cached && <th className="text-right">缓存</th>}
              {visibleColumns.cacheHit && <th className="text-right">缓存命中</th>}
              {visibleColumns.output && <th className="text-right">输出</th>}
              {visibleColumns.ttft && <th className="text-right">首字延迟</th>}
              {visibleColumns.latency && <th className="text-right">总耗时</th>}
              {visibleColumns.stream && <th className="min-w-20">流式</th>}
            </tr>
          </thead>
          <tbody>
            {loading ? (
              <tr>
                <td colSpan={visibleColumnCount} className="text-center">
                  <span className="loading loading-spinner" />
                </td>
              </tr>
            ) : logs.length === 0 ? (
              <tr>
                <td colSpan={visibleColumnCount} className="text-center text-base-content/50">
                  暂无调用记录
                </td>
              </tr>
            ) : (
              logs.map((l) => (
                <tr key={l.id}>
                  {visibleColumns.time && (
                    <td className="whitespace-nowrap">{logTimeFormatter.format(new Date(l.created_at))}</td>
                  )}
                  {visibleColumns.key && <td>{l.key_name || '-'}</td>}
                  {visibleColumns.account && <td>{l.account_email || '-'}</td>}
                  {visibleColumns.model && <td>{l.model || '-'}</td>}
                  {visibleColumns.endpoint && (
                    <td>
                      <code>{l.endpoint || '-'}</code>
                    </td>
                  )}
                  {visibleColumns.status && (
                    <td>
                      {l.status >= 200 && l.status < 300 ? (
                        <span className="badge badge-success badge-sm">成功</span>
                      ) : (
                        <div
                          className="tooltip tooltip-left"
                          data-tip={l.error_reason || '历史记录或响应中未包含可提取的错误原因'}
                        >
                          <span className="badge badge-error badge-sm">错误</span>
                        </div>
                      )}
                    </td>
                  )}
                  {visibleColumns.input && (
                    <td className="text-right tabular-nums">
                      {formatTokens(uncachedInputTokens(l.prompt_tokens, l.cached_tokens))}
                    </td>
                  )}
                  {visibleColumns.cached && (
                    <td className="text-right tabular-nums">{formatTokens(l.cached_tokens)}</td>
                  )}
                  {visibleColumns.cacheHit && (
                    <td className={`text-right tabular-nums ${cacheHitRateColor(l.prompt_tokens, l.cached_tokens)}`}>
                      {formatCacheHitRate(l.prompt_tokens, l.cached_tokens)}
                    </td>
                  )}
                  {visibleColumns.output && (
                    <td className="text-right tabular-nums">{formatTokens(l.completion_tokens)}</td>
                  )}
                  {visibleColumns.ttft && (
                    <td className={`text-right tabular-nums ${durationColor(l.ttft_ms, 5000, 15000)}`}>
                      {formatDuration(l.ttft_ms)}
                    </td>
                  )}
                  {visibleColumns.latency && (
                    <td className={`text-right tabular-nums ${durationColor(l.latency_ms, 10000, 30000)}`}>
                      {formatDuration(l.latency_ms)}
                    </td>
                  )}
                  {visibleColumns.stream && (
                    <td className="min-w-20">
                      <span className={`badge badge-sm whitespace-nowrap ${l.stream ? 'badge-info' : 'badge-ghost'}`}>
                        {l.stream ? '流式' : '非流'}
                      </span>
                    </td>
                  )}
                </tr>
              ))
            )}
          </tbody>
          </table>
        </div>

        <div className="flex flex-col gap-3 border-t border-base-300 px-4 py-4 sm:flex-row sm:items-center sm:justify-between sm:px-5">
        <div className="text-sm text-base-content/70">
          共 {total} 条，当前显示第 {firstItem}–{lastItem} 条
        </div>
        <div className="flex flex-wrap items-center gap-3">
          <label className="flex items-center gap-2 text-sm">
            每页
            <select
              className="select select-bordered select-sm"
              value={pageSize}
              disabled={loading}
              onChange={(e) => {
                setPageSize(Number(e.target.value))
                setPage(1)
              }}
            >
              <option value={20}>20</option>
              <option value={50}>50</option>
              <option value={100}>100</option>
            </select>
            条
          </label>
          <div className="join">
            <button
              className="join-item btn btn-sm"
              disabled={loading || page <= 1}
              onClick={() => setPage(1)}
              aria-label="首页"
            >
              «
            </button>
            <button
              className="join-item btn btn-sm"
              disabled={loading || page <= 1}
              onClick={() => setPage((value) => Math.max(1, value - 1))}
            >
              上一页
            </button>
            <button className="join-item btn btn-sm btn-disabled" aria-disabled="true">
              {page} / {totalPages}
            </button>
            <button
              className="join-item btn btn-sm"
              disabled={loading || page >= totalPages}
              onClick={() => setPage((value) => Math.min(totalPages, value + 1))}
            >
              下一页
            </button>
            <button
              className="join-item btn btn-sm"
              disabled={loading || page >= totalPages}
              onClick={() => setPage(totalPages)}
              aria-label="末页"
            >
              »
            </button>
          </div>
        </div>
        </div>
      </div>
    </section>
  )
}
