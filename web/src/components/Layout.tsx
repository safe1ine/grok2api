import { GaugeIcon, KeyRoundIcon, ScrollTextIcon, SparklesIcon, UsersRoundIcon } from 'lucide-react'
import { NavLink, Outlet } from 'react-router-dom'

const navItems = [
  { to: '/dashboard', label: '仪表盘', icon: GaugeIcon },
  { to: '/accounts', label: '账号管理', icon: UsersRoundIcon },
  { to: '/keys', label: '密钥管理', icon: KeyRoundIcon },
  { to: '/logs', label: '调用记录', icon: ScrollTextIcon },
]

export default function Layout() {
  return (
    <div className="app-page-background flex min-h-screen flex-col text-base-content">
      <header className="sticky top-0 z-20 bg-base-100/95 shadow-sm backdrop-blur">
        <div className="navbar mx-auto min-h-16 w-full max-w-[1440px] gap-1 px-2 sm:gap-3 sm:px-4 lg:px-6">
          <NavLink className="btn btn-ghost h-auto min-h-0 min-w-0 gap-2 p-0 text-left font-normal hover:bg-transparent sm:gap-3" to="/dashboard">
            <span className="rounded-box flex size-10 shrink-0 items-center justify-center bg-neutral text-neutral-content">
              <SparklesIcon className="size-5" />
            </span>
            <span className="hidden min-w-0 sm:block">
              <span className="block truncate text-base font-semibold">Grok 中转站</span>
              <span className="block truncate text-xs text-base-content/60">xAI API Gateway</span>
            </span>
          </NavLink>

          <div className="min-w-0 flex-1" />

          <nav className="flex shrink-0 gap-1 sm:gap-2" aria-label="管理页面">
            {navItems.map((item) => {
              const Icon = item.icon
              return (
                <NavLink
                  key={item.to}
                  to={item.to}
                  className={({ isActive }) =>
                    isActive ? 'btn btn-neutral btn-sm' : 'btn btn-ghost btn-sm'
                  }
                >
                  <Icon className="size-4" />
                  <span className="hidden sm:inline">{item.label}</span>
                </NavLink>
              )
            })}
          </nav>
        </div>
      </header>
      <main className="mx-auto min-w-0 flex-1 p-4 lg:w-full lg:max-w-[1440px] lg:p-6">
        <Outlet />
      </main>
    </div>
  )
}
