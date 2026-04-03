import { Route, Routes } from 'react-router-dom'
import AuthGate from './components/AuthGate'
import Layout from './components/Layout'
import Accounts from './pages/Accounts'
import Dashboard from './pages/Dashboard'
import Operations from './pages/Operations'
import Proxies from './pages/Proxies'
import SchedulerBoard from './pages/SchedulerBoard'
import Settings from './pages/Settings'
import Guide from './pages/Guide'
import ApiReference from './pages/ApiReference'
import Usage from './pages/Usage'

export default function App() {
  return (
    <AuthGate>
      <Layout>
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/accounts" element={<Accounts />} />
          <Route path="/proxies" element={<Proxies />} />
          <Route path="/ops" element={<Operations />} />
          <Route path="/ops/scheduler" element={<SchedulerBoard />} />
          <Route path="/usage" element={<Usage />} />
          <Route path="/settings" element={<Settings />} />
          <Route path="/guide" element={<Guide />} />
          <Route path="/api-reference" element={<ApiReference />} />
        </Routes>
      </Layout>
    </AuthGate>
  )
}
