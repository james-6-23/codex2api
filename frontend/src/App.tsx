import { Route, Routes } from 'react-router-dom'
import Layout from './components/Layout'
import Accounts from './pages/Accounts'
import Dashboard from './pages/Dashboard'
import Settings from './pages/Settings'
import Usage from './pages/Usage'

export default function App() {
  return (
    <Layout>
      <Routes>
        <Route path="/" element={<Dashboard />} />
        <Route path="/accounts" element={<Accounts />} />
        <Route path="/usage" element={<Usage />} />
        <Route path="/settings" element={<Settings />} />
      </Routes>
    </Layout>
  )
}
