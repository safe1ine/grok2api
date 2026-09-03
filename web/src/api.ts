const TOKEN_KEY = 'grok2api_token'

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}
export function setToken(t: string) {
  localStorage.setItem(TOKEN_KEY, t)
}
export function clearToken() {
  localStorage.removeItem(TOKEN_KEY)
}
export function isAuthed(): boolean {
  return !!getToken()
}

export interface Account {
  id: number
  email: string
  subject: string
  status: string
  cooldown_until: string | null
  created_at: string
  updated_at: string
  last_used_at: string | null
  scheduling_disabled: boolean
  scheduling_weight: number
  subscription_tier: string
  weekly_used_percent: number | null
  weekly_reset_at: string | null
  reset_credits_known: boolean
  reset_credits_available: number
  reset_credit_expires_at: string | null
}

export interface KeyItem {
  id: number
  name: string
  prefix: string
  revoked: boolean
  historical_calls: number
  today_calls: number
  created_at: string
}

export interface LogItem {
  id: number
  key_id: number | null
  account_id: number | null
  model: string
  endpoint: string
  status: number
  error_reason: string
  prompt_tokens: number
  cached_tokens: number
  completion_tokens: number
  ttft_ms: number
  latency_ms: number
  stream: boolean
  created_at: string
  key_name: string
  account_email: string
}

export interface LogListResponse {
  items: LogItem[]
  total: number
  limit: number
  offset: number
}

export interface DashboardPoint {
  timestamp: string
  calls: number
  input_tokens: number
  cached_tokens: number
  output_tokens: number
  cost_usd: number
}

export interface DashboardTotals {
  calls: number
  input_tokens: number
  cached_tokens: number
  output_tokens: number
  cost_usd: number
}

export interface ModelPricing {
  model: string
  canonical_model: string
  input_usd_per_million: number
  cached_usd_per_million: number
  output_usd_per_million: number
  long_input_usd_per_million: number
  long_cached_usd_per_million: number
  long_output_usd_per_million: number
  long_context_threshold: number
}

export interface UsageKeyOption {
  id: number
  name: string
  prefix: string
}

export interface DashboardResponse {
  range_minutes: number
  timezone: string
  from: string
  to: string
  points: DashboardPoint[]
  totals: DashboardTotals
  models: string[]
  keys: UsageKeyOption[]
  pricing: ModelPricing[]
  unpriced_models: string[]
  pricing_source: string
  pricing_as_of: string
}

export async function api<T>(path: string, opts: RequestInit = {}): Promise<T> {
  const headers: Record<string, string> = {
    ...(opts.headers as Record<string, string> | undefined),
  }
  if (opts.body && !headers['Content-Type']) {
    headers['Content-Type'] = 'application/json'
  }
  const token = getToken()
  if (token) headers['Authorization'] = 'Bearer ' + token

  const res = await fetch(path, { ...opts, headers })
  if (res.status === 401) {
    clearToken()
    window.location.href = '/login'
    throw new Error('未登录或登录已过期')
  }
  if (!res.ok) {
    let msg = res.statusText
    try {
      const b = await res.json()
      if (b.error) msg = b.error
    } catch {
      /* ignore */
    }
    throw new Error(msg)
  }
  return res.json() as Promise<T>
}
