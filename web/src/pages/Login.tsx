import { LogInIcon, SparklesIcon } from 'lucide-react'
import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { setToken } from '../api'

export default function Login() {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [err, setErr] = useState('')
  const [loading, setLoading] = useState(false)
  const navigate = useNavigate()

  async function submit(e: FormEvent) {
    e.preventDefault()
    setLoading(true)
    setErr('')
    try {
      const res = await fetch('/api/auth/login', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ username, password }),
      })
      const data = await res.json()
      if (!res.ok) {
        setErr(data.error || '登录失败')
        return
      }
      setToken(data.token)
      navigate('/dashboard')
    } catch (e) {
      setErr(String(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="app-page-background flex min-h-screen items-center justify-center p-4">
      <div className="card w-full max-w-sm rounded-2xl border border-base-300 bg-base-100 shadow-sm">
        <div className="card-body gap-5">
          <div className="grid justify-items-center gap-3 text-center">
            <span className="rounded-box flex size-12 items-center justify-center bg-neutral text-neutral-content">
              <SparklesIcon className="size-6" />
            </span>
            <div>
              <h1 className="text-xl font-semibold">Grok 中转站</h1>
              <p className="mt-1 text-xs text-base-content/55">xAI API Gateway Console</p>
            </div>
          </div>
          <form onSubmit={submit} className="grid gap-6">
            <div className="grid gap-4">
              <label className="form-control gap-2">
                <span className="label-text">用户名</span>
                <input
                  autoFocus
                  autoComplete="username"
                  className="input input-bordered w-full"
                  value={username}
                  onChange={(e) => setUsername(e.target.value)}
                />
              </label>
              <label className="form-control gap-2">
                <span className="label-text">密码</span>
                <input
                  type="password"
                  autoComplete="current-password"
                  className="input input-bordered w-full"
                  value={password}
                  onChange={(e) => setPassword(e.target.value)}
                />
              </label>
            </div>
            {err && <div className="text-sm text-error">{err}</div>}
            <button className="btn btn-neutral w-full" disabled={loading}>
              {loading ? <span className="loading loading-spinner loading-sm" /> : <LogInIcon className="size-4" />}
              {loading ? '登录中...' : '登录'}
            </button>
          </form>
        </div>
      </div>
    </div>
  )
}
