import { useCallback, useEffect, useRef, useState } from 'react'
import { liveSupported, startLive, type LiveSession } from './live'

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
  channels: Channel[]
}

interface Info {
  bridge: string
  pin: string
  setup_uri: string
  standalone: boolean
  nvr: string
  nvr_version: string
  cameras: number
  healthy: boolean
}

function useCameras() {
  const [cameras, setCameras] = useState<Record<string, Camera>>({})
  const [connected, setConnected] = useState(false)
  const sourceRef = useRef<EventSource | null>(null)

  const connect = useCallback(() => {
    sourceRef.current?.close()
    const source = new EventSource(`${API}/events`)
    sourceRef.current = source

    source.onopen = () => setConnected(true)
    source.onmessage = (event) => {
      try {
        const data = JSON.parse(event.data)
        if (data.type === 'camera' && typeof data.id === 'string') {
          setCameras((prev) => ({ ...prev, [data.id]: data as Camera }))
        }
      } catch {
        // ignore malformed events
      }
    }
    source.onerror = () => {
      setConnected(false)
      source.close()
      setTimeout(connect, 5000)
    }
  }, [])

  useEffect(() => {
    connect()
    return () => sourceRef.current?.close()
  }, [connect])

  return { cameras: Object.values(cameras).sort((a, b) => a.name.localeCompare(b.name)), connected }
}

function timeAgo(ms: number): string {
  if (!ms) return '–'
  const seconds = Math.floor((Date.now() - ms) / 1000)
  if (seconds < 5) return 'now'
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  return `${Math.floor(hours / 24)}d ago`
}

function CameraCard({ camera, pairable, pin }: { camera: Camera; pairable: boolean; pin: string }) {
  // In standalone mode each camera is paired individually; show its QR on demand.
  const [showQR, setShowQR] = useState(false)

  // Refresh the snapshot periodically and immediately on motion.
  const [tick, setTick] = useState(() => Date.now())
  useEffect(() => {
    const interval = setInterval(() => setTick(Date.now()), 10_000)
    return () => clearInterval(interval)
  }, [])
  useEffect(() => {
    if (camera.motion) setTick(Date.now())
  }, [camera.motion, camera.last_motion])

  // Live playback: click the tile to start/stop the fMP4 stream.
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
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [live, camera.id])

  const toggleLive = () => {
    if (!camera.online) return
    if (!liveSupported()) {
      setLiveError('live view not supported in this browser')
      return
    }
    setLive((v) => !v)
  }

  const best = camera.channels.find((c) => c.rtsp) ?? camera.channels[0]

  return (
    <div className={`card ${camera.motion ? 'card-motion' : ''}`}>
      <div className={`snapshot ${camera.online ? 'clickable' : ''}`} onClick={toggleLive} title={live ? 'Stop live view' : 'Start live view'}>
        {live ? (
          <video ref={videoRef} muted autoPlay playsInline />
        ) : camera.online ? (
          <img src={`${API}/cameras/${camera.id}/snapshot?w=640&t=${tick}`} alt={camera.name} loading="lazy" />
        ) : (
          <div className="offline-placeholder">offline</div>
        )}
        {live && <span className="badge badge-live">LIVE</span>}
        {!live && camera.online && <span className="play-hint">▶</span>}
        {camera.motion && <span className="badge badge-motion">MOTION</span>}
        {!camera.online && <span className="badge badge-offline">OFFLINE</span>}
        {liveError && !live && <span className="live-error">{liveError}</span>}
        {showQR && (
          <div className="qr-overlay" onClick={() => setShowQR(false)} title="Close">
            <img src={`${API}/cameras/${camera.id}/qr`} alt={`Pair ${camera.name}`} width={220} height={220} />
            <div className="qr-hint">Scan in the Home app, or enter code</div>
            <div className="pin">{pin}</div>
          </div>
        )}
      </div>
      <div className="card-body">
        <div className="card-title">
          <span className={`dot ${camera.online ? 'dot-on' : 'dot-off'}`} />
          {camera.name}
          {camera.doorbell && <span className="tag">doorbell</span>}
          {pairable && (
            <button className="connect-btn" onClick={() => setShowQR((v) => !v)}>
              {showQR ? 'Close' : 'Connect'}
            </button>
          )}
        </div>
        <div className="card-meta">
          {camera.model}
          {best && ` · ${best.width}×${best.height}@${best.fps}`}
          {camera.codec && ` · ${camera.codec}`}
        </div>
        <div className="card-meta">
          motion {timeAgo(camera.last_motion)}
          {camera.doorbell && ` · ring ${timeAgo(camera.last_ring)}`}
        </div>
      </div>
    </div>
  )
}

export default function App() {
  const { cameras, connected } = useCameras()
  const [info, setInfo] = useState<Info | null>(null)
  const [showPairing, setShowPairing] = useState(false)

  useEffect(() => {
    fetch(`${API}/info`)
      .then((r) => r.json())
      .then(setInfo)
      .catch(() => {})
  }, [])

  // Re-render every 30s so the "x minutes ago" labels stay fresh.
  const [, setNow] = useState(0)
  useEffect(() => {
    const interval = setInterval(() => setNow(Date.now()), 30_000)
    return () => clearInterval(interval)
  }, [])

  return (
    <div className="app">
      <header>
        <div>
          <h1>{info?.bridge ?? 'Protect HomeKit'}</h1>
          <div className="subtitle">
            {info?.nvr && `${info.nvr} · Protect ${info.nvr_version} · `}
            <span className={`dot ${connected ? 'dot-on' : 'dot-off'}`} /> {connected ? 'live' : 'reconnecting…'}
          </div>
        </div>
        <button onClick={() => setShowPairing((v) => !v)}>{showPairing ? 'Close' : 'Pair'}</button>
      </header>

      {showPairing && info && (
        <div className="pairing">
          {info.standalone ? (
            <div className="pairing-hint">
              Secure Video is on, so each camera pairs individually — use the{' '}
              <strong>Connect</strong> button on a camera below. Setup code for all:{' '}
              <span className="pin">{info.pin}</span>
            </div>
          ) : (
            <>
              <img src={`${API}/qr`} alt="HomeKit pairing QR code" width={200} height={200} />
              <div>
                <div className="pairing-hint">Scan with the Home app or enter the setup code:</div>
                <div className="pin">{info.pin}</div>
              </div>
            </>
          )}
        </div>
      )}

      <main className="grid">
        {cameras.map((camera) => (
          <CameraCard key={camera.id} camera={camera} pairable={info?.standalone ?? false} pin={info?.pin ?? ''} />
        ))}
        {cameras.length === 0 && <div className="empty">Waiting for cameras…</div>}
      </main>
    </div>
  )
}
