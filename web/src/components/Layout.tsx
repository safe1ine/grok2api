import { NavLink, Outlet, useNavigate } from 'react-router-dom'
import { clearToken } from '../api'

const navItems = [
  { to: '/accounts', label: '账号管理' },
  { to: '/keys', label: '密钥管理' },
  { to: '/logs', label: '调用记录' },
]

export default function Layout() {
  const navigate = useNavigate()

  function logout() {
    clearToken()
    navigate('/login')
  }

  return (
    <div className="min-h-screen bg-base-200">
      <div className="navbar bg-base-100 shadow">
        <div className="flex-1">
          <span className="text-xl font-bold px-2">grok2api</span>
          <ul className="menu menu-horizontal px-1">
            {navItems.map((item) => (
              <li key={item.to}>
                <NavLink
                  to={item.to}
                  className={({ isActive }) => (isActive ? 'active' : '')}
                >
                  {item.label}
                </NavLink>
              </li>
            ))}
          </ul>
        </div>
        <div className="flex-none">
          <button className="btn btn-ghost btn-sm" onClick={logout}>
            退出登录
          </button>
        </div>
      </div>
      <main className="p-6 max-w-7xl mx-auto">
        <Outlet />
      </main>
    </div>
  )
}
