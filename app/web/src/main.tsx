import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
import { ErrorBoundary } from './ErrorBoundary'
import { I18nProvider } from './lib/i18n'
import './index.css'

// Follow the system appearance live (index.html sets it before first paint).
const dark = matchMedia('(prefers-color-scheme: dark)')
dark.addEventListener('change', (e) => {
  // the Wall Display panel draws a dark gradient behind us: stay dark there
  if (document.documentElement.dataset.embed === 'wall-display') return
  document.documentElement.classList.toggle('dark', e.matches)
  document.documentElement.dataset.theme = e.matches ? 'dark' : 'light'
})

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <I18nProvider>
      <ErrorBoundary>
        <App />
      </ErrorBoundary>
    </I18nProvider>
  </React.StrictMode>,
)
