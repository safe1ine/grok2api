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
  rl_limit: number
  rl_remaining: number
  rl_token_limit: number
  rl_token_remaining: number
}

export interface KeyItem {
  id: number
  name: string
  prefix: string
  revoked: boolean
  created_at: string
}

export interface LogItem {
  id: number
  key_id: number | null
  account_id: number | null
  model: string
  endpoint: string
  status: number
  prompt_tokens: number
  completion_tokens: number
  latency_ms: number
  created_at: string
  key_name: string
  account_email: string
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
