import { lazy, Suspense } from 'react'
import { Navigate, Route, Routes } from 'react-router-dom'
import AuthGate from './components/AuthGate'
import Layout from './components/Layout'
import RouteErrorBoundary from './components/RouteErrorBoundary'
import StateShell from './components/StateShell'
import { ToastProvider } from './components/ToastProvider'
import { BrandingProvider } from './branding'
import { ThemeProvider } from './hooks/useTheme'
import Dashboard from './pages/Dashboard'

const Accounts = lazy(() => import('./pages/Accounts'))
const Operations = lazy(() => import('./pages/Operations'))
const OperationsErrors = lazy(() => import('./pages/OperationsErrors'))
const RuntimeStatus = lazy(() => import('./pages/RuntimeStatus'))
const Proxies = lazy(() => import('./pages/Proxies'))
const SchedulerBoard = lazy(() => import('./pages/SchedulerBoard'))
const Settings = lazy(() => import('./pages/Settings'))
const Docs = lazy(() => import('./pages/Docs'))
const APIKeys = lazy(() => import('./pages/APIKeys'))
const APIKeyUsagePortal = lazy(() => import('./pages/APIKeyUsagePortal'))
const ImageStudioPortal = lazy(() => import('./pages/ImageStudioPortal'))
const Usage = lazy(() => import('./pages/Usage'))
const ImageStudio = lazy(() => import('./pages/ImageStudio'))
const PromptFilter = lazy(() => import('./pages/PromptFilter'))
const ThemeSettings = lazy(() => import('./pages/ThemeSettings'))
const ModelPricing = lazy(() => import('./pages/ModelPricing'))
const PayloadRules = lazy(() => import('./pages/PayloadRules'))

export default function App() {
  return (
    <ThemeProvider>
      <BrandingProvider>
        <ToastProvider>
          <RouteErrorBoundary>
            <Suspense fallback={<StateShell variant="page" loading>{null}</StateShell>}>
              <Routes>
                <Route path="/key-usage" element={<Navigate to="/key-usage/overview" replace />} />
                <Route path="/key-usage/:view" element={<APIKeyUsagePortal />} />
                <Route path="/image-studio" element={<Navigate to="/image-studio/studio" replace />} />
                <Route path="/image-studio/:view" element={<ImageStudioPortal />} />
                <Route path="/*" element={<AdminApp />} />
              </Routes>
            </Suspense>
          </RouteErrorBoundary>
        </ToastProvider>
      </BrandingProvider>
    </ThemeProvider>
  )
}

function AdminApp() {
  return (
    <AuthGate>
      <Layout>
        <Routes>
          <Route path="/" element={<Dashboard />} />
          <Route path="/accounts" element={<Accounts />} />
          <Route path="/api-keys" element={<APIKeys />} />
          <Route path="/proxies" element={<Proxies />} />
          <Route path="/images" element={<Navigate to="/images/studio" replace />} />
          <Route path="/images/:view" element={<ImageStudio />} />
          <Route path="/prompt-filter" element={<Navigate to="/prompt-filter/overview" replace />} />
          <Route path="/prompt-filter/:view" element={<PromptFilter />} />
          <Route path="/ops" element={<Navigate to="/ops/overview" replace />} />
          <Route path="/ops/overview" element={<Operations />} />
          <Route path="/ops/runtime" element={<RuntimeStatus />} />
          <Route path="/ops/errors" element={<OperationsErrors />} />
          <Route path="/ops/scheduler" element={<SchedulerBoard />} />
          <Route path="/usage" element={<Usage />} />
          <Route path="/model-pricing" element={<ModelPricing />} />
          <Route path="/payload-rules" element={<PayloadRules />} />
          <Route path="/theme" element={<ThemeSettings />} />
          <Route path="/settings" element={<Settings />} />
          <Route path="/docs" element={<Docs />} />
          <Route path="/guide" element={<Navigate to="/docs" replace />} />
          <Route path="/api-reference" element={<Navigate to="/docs#model-api" replace />} />
        </Routes>
      </Layout>
    </AuthGate>
  )
}
