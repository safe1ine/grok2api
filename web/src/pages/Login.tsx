import { useState, type FormEvent } from 'react'
import { useNavigate } from 'react-router-dom'
import { setToken } from '../api'

export default function Login() {
  const [username, setUsername] = useState('admin')
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
      navigate('/accounts')
    } catch (e) {
      setErr(String(e))
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-base-200">
      <div className="card w-96 bg-base-100 shadow-xl">
        <div className="card-body">
          <h2 className="card-title justify-center">grok2api 管理台</h2>
          <form onSubmit={submit} className="space-y-3">
            <label className="form-control">
              <span className="label-text mb-1">用户名</span>
              <input
                className="input input-bordered"
                value={username}
                onChange={(e) => setUsername(e.target.value)}
              />
            </label>
            <label className="form-control">
              <span className="label-text mb-1">密码</span>
              <input
                type="password"
                className="input input-bordered"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
              />
            </label>
            {err && <div className="text-error text-sm">{err}</div>}
            <button className="btn btn-primary w-full" disabled={loading}>
              {loading ? '登录中...' : '登录'}
            </button>
          </form>
        </div>
      </div>
    </div>
  )
}
