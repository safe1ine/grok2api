import {
  ActivityIcon,
  CircleDollarSignIcon,
  DatabaseZapIcon,
  RefreshCwIcon,
} from 'lucide-react'
import { useCallback, useEffect, useMemo, useState } from 'react'
import {
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { api, DashboardPoint, DashboardResponse } from '../api'
import { formatTokenCount } from '../format'

const ranges = [
  { label: '最近 1 小时', minutes: 60 },
  { label: '最近 6 小时', minutes: 360 },
  { label: '最近 24 小时', minutes: 1440 },
  { label: '最近 7 天', minutes: 10080 },
]

const integer = new Intl.NumberFormat('zh-CN')
const compact = new Intl.NumberFormat('zh-CN', { notation: 'compact', maximumFractionDigits: 1 })
const usd = new Intl.NumberFormat('en-US', {
  style: 'currency',
  currency: 'USD',
  minimumFractionDigits: 2,
  maximumFractionDigits: 6,
})

function formatTime(value: string, minutes: number) {
  return new Intl.DateTimeFormat('zh-CN', {
    timeZone: 'Asia/Shanghai',
    month: minutes > 1440 ? '2-digit' : undefined,
    day: minutes > 1440 ? '2-digit' : undefined,
    hour: '2-digit',
    minute: '2-digit',
    hour12: false,
  }).format(new Date(value))
}

function MetricChart({
  title,
  description,
  data,
  dataKey,
  color,
  rangeMinutes,
  valueFormatter,
}: {
  title: string
  description: string
  data: DashboardPoint[]
  dataKey: 'calls' | 'input_tokens' | 'cost_usd'
  color: string
  rangeMinutes: number
  valueFormatter: (value: number) => string
}) {
  return (
    <section className="card rounded-2xl border border-base-300 bg-base-100 shadow-sm">
      <div className="card-body p-5">
        <div>
          <h2 className="card-title text-base">{title}</h2>
          <p className="text-xs text-base-content/60 mt-1">{description}</p>
        </div>
        <div className="h-72 mt-2">
          <ResponsiveContainer width="100%" height="100%">
            <LineChart data={data} margin={{ top: 8, right: 16, left: 4, bottom: 0 }}>
              <CartesianGrid strokeDasharray="3 3" stroke="currentColor" opacity={0.12} />
              <XAxis
                dataKey="timestamp"
                tickFormatter={(value) => formatTime(String(value), rangeMinutes)}
                minTickGap={54}
                tick={{ fontSize: 11 }}
                axisLine={false}
                tickLine={false}
              />
              <YAxis
                width={72}
                tickFormatter={(value) => {
                  const number = Number(value)
                  if (dataKey === 'cost_usd') return usd.format(number)
                  if (dataKey === 'input_tokens') return formatTokenCount(number)
                  return compact.format(number)
                }}
                tick={{ fontSize: 11 }}
                axisLine={false}
                tickLine={false}
                domain={[0, 'auto']}
              />
              <Tooltip
                labelFormatter={(value) => `${formatTime(String(value), rangeMinutes)}（北京时间）`}
                formatter={(value) => [valueFormatter(Number(value)), title]}
                contentStyle={{ borderRadius: '0.75rem', borderColor: 'hsl(var(--bc) / 0.15)' }}
              />
              <Line
                type="monotone"
                dataKey={dataKey}
                stroke={color}
                strokeWidth={2}
                dot={false}
                activeDot={{ r: 4 }}
                isAnimationActive={false}
              />
            </LineChart>
          </ResponsiveContainer>
        </div>
      </div>
    </section>
  )
}

export default function Dashboard() {
  const [rangeMinutes, setRangeMinutes] = useState(360)
  const [data, setData] = useState<DashboardResponse | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')

  const load = useCallback(async () => {
    setError('')
    try {
      const result = await api<DashboardResponse>(`/api/dashboard?minutes=${rangeMinutes}`)
      setData(result)
    } catch (err) {
      setError(err instanceof Error ? err.message : '加载仪表盘失败')
    } finally {
      setLoading(false)
    }
  }, [rangeMinutes])

  useEffect(() => {
    setLoading(true)
    void load()
    const timer = window.setInterval(() => void load(), 60_000)
    return () => window.clearInterval(timer)
  }, [load])

  const cacheRatio = useMemo(() => {
    if (!data?.totals.input_tokens) return 0
    return (data.totals.cached_tokens / data.totals.input_tokens) * 100
  }, [data])

  return (
    <section className="grid gap-6">
      <header className="flex flex-col gap-3 sm:flex-row sm:items-center sm:justify-between">
        <h1 className="text-2xl font-semibold tracking-tight">仪表盘</h1>
        <div className="flex flex-wrap gap-2 self-start sm:self-auto">
          <select
            className="select select-bordered select-sm"
            value={rangeMinutes}
            onChange={(event) => setRangeMinutes(Number(event.target.value))}
          >
            {ranges.map((range) => (
              <option key={range.minutes} value={range.minutes}>{range.label}</option>
            ))}
          </select>
          <button className="btn btn-sm btn-outline" onClick={() => void load()} disabled={loading}>
            <RefreshCwIcon className={loading ? 'size-4 animate-spin' : 'size-4'} />
            刷新
          </button>
        </div>
      </header>

      {error && <div className="alert alert-error"><span>{error}</span></div>}

      <div className="grid grid-cols-2 gap-3 lg:grid-cols-4">
        <div className="rounded-2xl border border-base-300 bg-base-100 p-5 shadow-sm">
          <div className="flex items-center justify-between text-xs text-base-content/50">
            <span>调用次数</span><ActivityIcon className="size-4" />
          </div>
          <div className="mt-4 text-2xl font-semibold tabular-nums">{integer.format(data?.totals.calls ?? 0)}</div>
        </div>
        <div className="rounded-2xl border border-base-300 bg-base-100 p-5 shadow-sm">
          <div className="flex items-center justify-between text-xs text-base-content/50">
            <span>输入 Token</span><DatabaseZapIcon className="size-4" />
          </div>
          <div className="mt-4 text-2xl font-semibold tabular-nums">{formatTokenCount(data?.totals.input_tokens ?? 0)}</div>
          <div className="mt-1 text-xs text-base-content/45">包含缓存命中</div>
        </div>
        <div className="rounded-2xl border border-base-300 bg-base-100 p-5 shadow-sm">
          <div className="flex items-center justify-between text-xs text-base-content/50">
            <span>缓存命中</span><DatabaseZapIcon className="size-4" />
          </div>
          <div className="mt-4 text-2xl font-semibold tabular-nums">{cacheRatio.toFixed(1)}%</div>
          <div className="mt-1 text-xs text-base-content/45">{formatTokenCount(data?.totals.cached_tokens ?? 0)} Token</div>
        </div>
        <div className="rounded-2xl bg-neutral p-5 text-neutral-content shadow-sm">
          <div className="flex items-center justify-between text-xs text-neutral-content/60">
            <span>估算价格</span><CircleDollarSignIcon className="size-4" />
          </div>
          <div className="mt-4 text-2xl font-semibold tabular-nums">{usd.format(data?.totals.cost_usd ?? 0)}</div>
          <div className="mt-1 text-xs text-neutral-content/50">按 xAI 官方费率</div>
        </div>
      </div>

      {loading && !data ? (
        <div className="flex h-72 items-center justify-center">
          <span className="loading loading-spinner loading-lg" />
        </div>
      ) : (
        <div className="space-y-5">
          <MetricChart
            title="每分钟调用次数"
            description="包括成功和失败的所有网关调用"
            data={data?.points ?? []}
            dataKey="calls"
            color="#2563eb"
            rangeMinutes={rangeMinutes}
            valueFormatter={(value) => `${integer.format(value)} 次`}
          />
          <MetricChart
            title="每分钟输入 Token"
            description="输入总量，缓存命中 Token 已包含在内"
            data={data?.points ?? []}
            dataKey="input_tokens"
            color="#7c3aed"
            rangeMinutes={rangeMinutes}
            valueFormatter={(value) => `${formatTokenCount(value)} Token`}
          />
          <MetricChart
            title="每分钟估算价格"
            description="非缓存输入、缓存输入、输出及长上下文分别按官方费率计算"
            data={data?.points ?? []}
            dataKey="cost_usd"
            color="#059669"
            rangeMinutes={rangeMinutes}
            valueFormatter={(value) => usd.format(value)}
          />
        </div>
      )}

      {data && (
        <div className="space-y-1 rounded-2xl border border-base-300 bg-base-100 p-5 text-xs text-base-content/60 shadow-sm">
          {data.pricing.map((price) => (
            <p key={price.model}>
              {price.model}：输入 ${price.input_usd_per_million}/百万，缓存 ${price.cached_usd_per_million}/百万，
              输出 ${price.output_usd_per_million}/百万；超过 {formatTokenCount(price.long_context_threshold)} 输入 Token 后为
              ${price.long_input_usd_per_million}/${price.long_cached_usd_per_million}/${price.long_output_usd_per_million}。
            </p>
          ))}
          {data.unpriced_models.length > 0 && (
            <p className="text-warning">未找到官方费率、未计入价格：{data.unpriced_models.join('、')}</p>
          )}
          <p>
            费率快照：{data.pricing_as_of} ·{' '}
            <a className="link" href={data.pricing_source} target="_blank" rel="noreferrer">xAI 官方模型与价格</a>
          </p>
        </div>
      )}
    </section>
  )
}
