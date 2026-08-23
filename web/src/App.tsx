import { Routes, Route, Navigate } from 'react-router-dom'
import Layout from './components/Layout'
import Login from './pages/Login'
import Accounts from './pages/Accounts'
import Keys from './pages/Keys'
import Logs from './pages/Logs'
import { isAuthed } from './api'

function RequireAuth({ children }: { children: React.ReactElement }) {
  if (!isAuthed()) return <Navigate to="/login" replace />
  return children
}

export default function App() {
  return (
    <Routes>
      <Route path="/login" element={<Login />} />
      <Route
        path="/"
        element={
          <RequireAuth>
            <Layout />
          </RequireAuth>
        }
      >
        <Route index element={<Navigate to="/accounts" replace />} />
        <Route path="accounts" element={<Accounts />} />
        <Route path="keys" element={<Keys />} />
        <Route path="logs" element={<Logs />} />
      </Route>
      <Route path="*" element={<Navigate to="/" replace />} />
    </Routes>
  )
}
