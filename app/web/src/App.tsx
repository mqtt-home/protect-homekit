import { useCallback, useEffect, useRef, useState } from 'react'
import { Activity, Camera as CameraIcon, History, CircleCheck, Play, QrCode, Square, Video, X } from 'lucide-react'
import { liveSupported, startLive, type LiveSession } from './live'
import { LangSwitch, useI18n } from '@/lib/i18n'

const API = './api'

interface Channel {
  name: string
  width: number
  height: number
  fps: number
  rtsp: boolean
}

interface Camera {
  aid: number
  id: string
  name: string
  model: string
  mac: string
  firmware: string
  online: boolean
  motion: boolean
  last_motion: number
  last_ring: number
  doorbell: boolean
  codec: string
  channels: Channel[] | null
}

interface Info {
  bridge: string
  pin: string
  setup_uri: string
  nvr: string
  nvr_version: string
  cameras: number
  healthy: boolean
}

// Read the backend defensively: one null where a list is expected blanks the page.
const list = <T,>(xs: T[] | null | undefined): T[] => (Array.isArray(xs) ? xs : [])

const tint = (c: string) => ({ ['--c' as string]: c }) as React.CSSProperties

function useCameras() {
  const [cameras, setCameras] = useState<Record<string, Camera>>({})
  const [connected, setConnected] = useState(false)
  const sourceRef = useRef<EventSource | null>(null)
  const retryRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const connect = useCallback(() => {
    if (retryRef.current) clearTimeout(retryRef.current)
    sourceRef.current?.close()
    const source = new EventSource(`${API}/events`)
    sourceRef.current = source

    source.onopen = () => setConnected(true)
    source.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data)
        if (data && data.type === 'camera' && typeof data.id === 'string') {
          setCameras((prev) => ({ ...prev, [data.id]: data as Camera }))
        }
      } catch {
        // ignore malformed events
      }
    }
    source.onerror = () => {
      setConnected(false)
      source.close()
      retryRef.current = setTimeout(connect, 5000)
    }
  }, [])

  useEffect(() => {
    connect()
    return () => {
      if (retryRef.current) clearTimeout(retryRef.current)
      sourceRef.current?.close()
    }
  }, [connect])

  return {
    cameras: Object.values(cameras).sort((a, b) => (a.name ?? '').localeCompare(b.name ?? '')),
    connected,
    reconnect: connect,
  }
}

function CameraCard({ camera }: { camera: Camera }) {
  const { t, ago, when } = useI18n()

  // Refresh the snapshot periodically and immediately on motion.
  const [tick, setTick] = useState(() => Date.now())
  const [snapFailed, setSnapFailed] = useState(false)
  useEffect(() => {
    const interval = setInterval(() => setTick(Date.now()), 10_000)
    return () => clearInterval(interval)
  }, [])
  useEffect(() => {
    if (camera.motion) setTick(Date.now())
  }, [camera.motion, camera.last_motion])

  // Live playback: tap the picture to start/stop the fMP4 stream.
  const [live, setLive] = useState(false)
  const [liveError, setLiveError] = useState<string | null>(null)
  const videoRef = useRef<HTMLVideoElement>(null)
  const sessionRef = useRef<LiveSession | null>(null)

  useEffect(() => {
    if (!live || !videoRef.current) return
    setLiveError(null)
    const session = startLive(videoRef.current, camera.id, (message) => {
      setLiveError(message)
      setLive(false)
    })
    sessionRef.current = session
    return () => {
      session.stop()
      sessionRef.current = null
    }
  }, [live, camera.id])

  const toggleLive = () => {
    if (!camera.online) return
    if (!liveSupported()) {
      setLiveError(t('cam.unsupported'))
      return
    }
    setLive((v) => !v)
  }

  const channels = list(camera.channels)
  const best = channels.find((c) => c.rtsp) ?? channels[0]
  const hasRing = camera.doorbell || camera.last_ring > 0

  return (
    <article className={`card cam${camera.motion ? ' motion' : ''}`}>
      {camera.online ? (
        <button
          type="button"
          className="shot"
          onClick={toggleLive}
          aria-label={live ? t('cam.stopLive') : t('cam.startLive')}
          title={live ? t('cam.stopLive') : t('cam.startLive')}
        >
          {live ? (
            <video ref={videoRef} muted autoPlay playsInline />
          ) : (
            <img
              src={`${API}/cameras/${camera.id}/snapshot?w=640&t=${tick}`}
              alt={camera.name}
              loading="lazy"
              style={snapFailed ? { visibility: 'hidden' } : undefined}
              onError={() => setSnapFailed(true)}
              onLoad={() => setSnapFailed(false)}
            />
          )}
          {!live && snapFailed && (
            <span className="absolute inset-0 grid place-items-center text-sm" style={{ color: '#8B95A7' }}>
              {t('cam.snapshotFailed')}
            </span>
          )}
          <span className="ov">
            {live && <span className="chip"><span className="dot live" style={tint('var(--ok)')} />{t('cam.live')}</span>}
            {camera.motion && <span className="chip"><span className="dot live" style={tint('var(--alert)')} />{t('cam.motion')}</span>}
          </span>
          <span className="play">{live ? <Square /> : <Play />}</span>
          {liveError && !live && <span className="err">{liveError}</span>}
        </button>
      ) : (
        <div className="shot off">{t('cam.offline')}</div>
      )}
      <div className="card-b space-y-1" style={{ paddingTop: 12 }}>
        <div className="flex items-center gap-2">
          <span className={`dot${camera.online ? '' : ' bad'}`} />
          <h3 className="min-w-0 flex-1 truncate text-[15px] font-semibold">{camera.name}</h3>
          {camera.doorbell && <span className="chip info">{t('cam.doorbell')}</span>}
          {!camera.online && <span className="chip alert">{t('cam.offline')}</span>}
        </div>
        <div className="meta num">
          {camera.model}
          {best && ` · ${best.width}×${best.height} @ ${best.fps} fps`}
          {camera.codec && ` · ${camera.codec.toUpperCase()}`}
        </div>
        <div className="meta num flex flex-wrap gap-x-3">
          <span title={when(camera.last_motion)}>{t('cam.lastMotion', { ago: ago(camera.last_motion) })}</span>
          {hasRing && <span title={when(camera.last_ring)}>{t('cam.lastRing', { ago: ago(camera.last_ring) })}</span>}
        </div>
      </div>
    </article>
  )
}

function Stats({ cameras, info }: { cameras: Camera[]; info: Info | null }) {
  const { t, num, ago } = useI18n()
  const online = cameras.filter((c) => c.online).length
  const moving = cameras.filter((c) => c.motion).length
  const last = cameras.reduce((m, c) => Math.max(m, c.last_motion || 0), 0)
  return (
    <div className="stats">
      <div className={`stat${online < cameras.length ? ' warn' : ''}`}>
        <div className="k"><CameraIcon />{t('stat.cameras')}</div>
        <div className="v">{num(online)}</div>
        <div className="s num">{t('stat.online', { ok: num(online), n: num(cameras.length) })}</div>
      </div>
      <div className={`stat${moving > 0 ? ' alert' : ''}`}>
        <div className="k"><Activity />{t('stat.motion')}</div>
        <div className="v">{num(moving)}</div>
        <div className="s">{moving > 0 ? t('stat.motionNow') : t('stat.motionNone')}</div>
      </div>
      <div className="stat">
        <div className="k"><History />{t('stat.last')}</div>
        <div className="v small">{last ? ago(last) : '–'}</div>
      </div>
      <div className={`stat${info && !info.healthy ? ' alert' : ''}`}>
        <div className="k"><CircleCheck />{t('stat.homekit')}</div>
        <div className="v small" style={info?.healthy ? { color: 'var(--ok)' } : undefined}>
          {info ? (info.healthy ? t('stat.healthy') : t('stat.unhealthy')) : '–'}
        </div>
      </div>
    </div>
  )
}

function Pairing({ info }: { info: Info }) {
  const { t } = useI18n()
  return (
    <section className="card card-b pairing" style={{ paddingTop: 16 }}>
      <img src={`${API}/qr`} alt={t('pairing.qr')} width={168} height={168} />
      <div className="min-w-0" style={{ flex: '1 1 220px' }}>
        <div className="text-sm text-muted-foreground">{t('pairing.code')}</div>
        <div className="setup-code">{info.pin}</div>
        <p className="mt-2 text-sm" style={{ color: 'var(--ink-2)' }}>{t('pairing.hint')}</p>
      </div>
    </section>
  )
}

export default function App() {
  const { t } = useI18n()
  const { cameras, connected, reconnect } = useCameras()
  const [info, setInfo] = useState<Info | null>(null)
  const [showPairing, setShowPairing] = useState(false)

  useEffect(() => {
    fetch(`${API}/info`)
      .then((r) => r.json())
      .then((d) => {
        if (d && typeof d === 'object') setInfo(d as Info)
      })
      .catch(() => {})
  }, [])

  // Re-render every 30s so the "vor x min" labels stay fresh.
  const [, setNow] = useState(0)
  useEffect(() => {
    const interval = setInterval(() => setNow(Date.now()), 30_000)
    return () => clearInterval(interval)
  }, [])

  return (
    <div className="min-h-screen">
      <header className="top-bar">
        <div className="mx-auto flex max-w-5xl items-center gap-3 px-safe py-3">
          <span className="ico"><Video /></span>
          <div className="min-w-0 flex-1">
            <h1 className="app-tt">{t('title')}</h1>
            {connected ? (
              <div className="app-st">
                <span className="dot live" />
                <span className="num truncate">
                  {t('conn.live')}
                  {info?.nvr ? ` · ${t('nvr', { nvr: info.nvr, version: info.nvr_version })}` : ''}
                </span>
              </div>
            ) : (
              <button type="button" className="app-st" onClick={reconnect} title={t('conn.reconnect')}>
                <span className="dot idle" />
                <span className="truncate">{t('conn.connecting')}</span>
              </button>
            )}
          </div>
          {info && (
            <button
              type="button"
              className="btn"
              aria-pressed={showPairing}
              aria-label={showPairing ? t('pairing.close') : t('pairing')}
              title={showPairing ? t('pairing.close') : t('pairing')}
              onClick={() => setShowPairing((v) => !v)}
            >
              {showPairing ? <X className="h-4 w-4" /> : <QrCode className="h-4 w-4" />}
              <span className="hidden sm:inline">{t('pairing')}</span>
            </button>
          )}
          <LangSwitch />
        </div>
      </header>

      <main
        className="mx-auto max-w-5xl space-y-6 px-safe pt-5"
        style={{ paddingBottom: 'calc(2.5rem + env(safe-area-inset-bottom))' }}
      >
        {showPairing && info && <Pairing info={info} />}

        <Stats cameras={cameras} info={info} />

        <section>
          <div className="sec-title">
            <h2>{t('cameras')}</h2>
          </div>
          {cameras.length === 0 ? (
            <div className="card card-b text-sm text-muted-foreground" style={{ paddingTop: 14 }}>{t('cameras.waiting')}</div>
          ) : (
            <div className="cams">
              {cameras.map((camera) => (
                <CameraCard key={camera.id} camera={camera} />
              ))}
            </div>
          )}
        </section>
      </main>
    </div>
  )
}
