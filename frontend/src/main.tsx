import { StrictMode } from 'react'
import ReactDOM from 'react-dom/client'
import { BrowserRouter } from 'react-router-dom'
import App from './App'
import './App.css'

const rootElement = document.getElementById('root')

if (!rootElement) {
  throw new Error('未找到 root 节点')
}

ReactDOM.createRoot(rootElement).render(
  <StrictMode>
    <BrowserRouter basename="/admin">
      <App />
    </BrowserRouter>
  </StrictMode>,
)
