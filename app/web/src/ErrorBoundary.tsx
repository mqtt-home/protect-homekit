import { Component, type ErrorInfo, type ReactNode } from 'react'
import { useI18n } from '@/lib/i18n'

function Crash({ error }: { error: Error }) {
  const { t } = useI18n()
  return (
    <div className="mx-auto max-w-3xl px-safe py-6">
      <h1 className="text-lg font-semibold" style={{ color: 'var(--alert)' }}>{t('crash.title')}</h1>
      <p className="mt-2 text-sm text-muted-foreground">
        {t('crash.body')}{' '}
        <a className="underline" href="./api/cameras">api/cameras</a>
      </p>
      <pre className="mt-4 overflow-x-auto rounded-md border p-3 text-xs">{String(error.stack ?? error)}</pre>
      <button className="btn mt-4" onClick={() => location.reload()}>{t('crash.reload')}</button>
    </div>
  )
}

// Without this a single render error unmounts the whole tree and leaves a
// white page with nothing to go on.
export class ErrorBoundary extends Component<{ children: ReactNode }, { error: Error | null }> {
  state = { error: null as Error | null }

  static getDerivedStateFromError(error: Error) {
    return { error }
  }

  componentDidCatch(error: Error, info: ErrorInfo) {
    console.error('UI crashed', error, info.componentStack)
  }

  render() {
    return this.state.error ? <Crash error={this.state.error} /> : this.props.children
  }
}
