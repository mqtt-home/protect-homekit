import { createContext, useCallback, useContext, useMemo, useState } from 'react'

// UI strings only. Camera names, models, NVR names and browser/stream error
// details come from elsewhere and stay as they are.

export type Lang = 'de' | 'en'
export const LANGS: Lang[] = ['de', 'en']

const LANG_KEY = 'home.lang'

// German is the house language; English stays one tap away. Per device.
function initialLang(): Lang {
  try {
    const v = localStorage.getItem(LANG_KEY)
    if (v === 'de' || v === 'en') return v
  } catch {
    // storage blocked
  }
  return 'de'
}

const dict = {
  de: {
    title: 'Kameras',
    language: 'Sprache',
    'conn.live': 'verbunden',
    'conn.connecting': 'Verbindung wird aufgebaut …',
    'conn.reconnect': 'Erneut verbinden',
    'nvr': '{nvr} · Protect {version}',
    pairing: 'HomeKit-Kopplung',
    'pairing.close': 'Kopplung schließen',
    'pairing.hint': 'In der Home-App „Gerät hinzufügen“ wählen und den Code scannen oder eingeben.',
    'pairing.code': 'Setup-Code',
    'pairing.qr': 'HomeKit-Kopplungscode als QR-Code',
    'stat.cameras': 'Kameras',
    'stat.online': '{ok} von {n} online',
    'stat.motion': 'Bewegung',
    'stat.motionNow': 'gerade erkannt',
    'stat.motionNone': 'keine aktuell',
    'stat.last': 'Letzte Bewegung',
    'stat.homekit': 'HomeKit',
    'stat.healthy': 'bereit',
    'stat.unhealthy': 'gestört',
    cameras: 'Kameras',
    'cameras.waiting': 'Warte auf Kameras …',
    'cam.live': 'Livebild',
    'cam.motion': 'Bewegung',
    'cam.offline': 'Offline',
    'cam.doorbell': 'Klingel',
    'cam.startLive': 'Livebild starten',
    'cam.stopLive': 'Livebild beenden',
    'cam.lastMotion': 'Bewegung {ago}',
    'cam.lastRing': 'Klingel {ago}',
    'cam.unsupported': 'Livebild wird von diesem Browser nicht unterstützt',
    'cam.snapshotFailed': 'Kein Standbild',
    'ago.never': 'nie',
    'ago.now': 'gerade eben',
    'ago.min': 'vor {n} min',
    'ago.h': 'vor {n} h',
    'ago.d': 'vor {n} d',
    'crash.title': 'Die Seite ist auf einen Fehler gestoßen',
    'crash.body': 'Die Bridge läuft weiter. Rohdaten:',
    'crash.reload': 'Neu laden',
  },
  en: {
    title: 'Cameras',
    language: 'Language',
    'conn.live': 'connected',
    'conn.connecting': 'Connecting …',
    'conn.reconnect': 'Reconnect',
    'nvr': '{nvr} · Protect {version}',
    pairing: 'HomeKit pairing',
    'pairing.close': 'Close pairing',
    'pairing.hint': 'In the Home app choose “Add Accessory” and scan or enter the code.',
    'pairing.code': 'Setup code',
    'pairing.qr': 'HomeKit pairing code as QR code',
    'stat.cameras': 'Cameras',
    'stat.online': '{ok} of {n} online',
    'stat.motion': 'Motion',
    'stat.motionNow': 'detected now',
    'stat.motionNone': 'none right now',
    'stat.last': 'Last motion',
    'stat.homekit': 'HomeKit',
    'stat.healthy': 'ready',
    'stat.unhealthy': 'unhealthy',
    cameras: 'Cameras',
    'cameras.waiting': 'Waiting for cameras …',
    'cam.live': 'Live',
    'cam.motion': 'Motion',
    'cam.offline': 'Offline',
    'cam.doorbell': 'Doorbell',
    'cam.startLive': 'Start live view',
    'cam.stopLive': 'Stop live view',
    'cam.lastMotion': 'Motion {ago}',
    'cam.lastRing': 'Ring {ago}',
    'cam.unsupported': 'Live view is not supported in this browser',
    'cam.snapshotFailed': 'No snapshot',
    'ago.never': 'never',
    'ago.now': 'just now',
    'ago.min': '{n} min ago',
    'ago.h': '{n} h ago',
    'ago.d': '{n} d ago',
    'crash.title': 'The page hit an error',
    'crash.body': 'The bridge keeps running. Raw data:',
    'crash.reload': 'Reload',
  },
} satisfies Record<Lang, Record<string, string>>

export type Key = keyof (typeof dict)['de']

export interface I18n {
  lang: Lang
  locale: string
  setLang: (l: Lang) => void
  /** A UI string with {placeholders}. */
  t: (key: Key, vars?: Record<string, string | number>) => string
  /** A number in the current locale. */
  num: (n: number | null | undefined, decimals?: number) => string
  /** "vor 3 min" / "3 min ago" for a Unix time in milliseconds; 0 = never. */
  ago: (ms: number | null | undefined, now?: number) => string
  /** Date and 24h time for a Unix time in milliseconds: "18.10., 11:49". */
  when: (ms: number | null | undefined) => string
}

const Ctx = createContext<I18n | null>(null)

export function I18nProvider({ children }: { children: React.ReactNode }) {
  const [lang, setLangState] = useState<Lang>(initialLang)

  const setLang = useCallback((l: Lang) => {
    setLangState(l)
    try {
      localStorage.setItem(LANG_KEY, l)
    } catch {
      // convenience only
    }
  }, [])

  const value = useMemo<I18n>(() => {
    const locale = lang === 'de' ? 'de-DE' : 'en-GB'
    document.documentElement.lang = lang
    const t: I18n['t'] = (key, vars) =>
      (dict[lang][key] ?? dict.en[key] ?? key).replace(/\{(\w+)\}/g, (_, k) => String(vars?.[k] ?? ''))
    const fmt = new Map<number, Intl.NumberFormat>()
    const num: I18n['num'] = (n, decimals = 0) => {
      let f = fmt.get(decimals)
      if (!f) {
        f = new Intl.NumberFormat(locale, { minimumFractionDigits: decimals, maximumFractionDigits: decimals })
        fmt.set(decimals, f)
      }
      return f.format(n ?? 0)
    }
    const ago: I18n['ago'] = (ms, now = Date.now()) => {
      if (!ms) return t('ago.never')
      const s = Math.max(0, Math.round((now - ms) / 1000))
      if (s < 60) return t('ago.now')
      if (s < 3600) return t('ago.min', { n: Math.floor(s / 60) })
      if (s < 86400) return t('ago.h', { n: Math.floor(s / 3600) })
      return t('ago.d', { n: Math.floor(s / 86400) })
    }
    const dtf = new Intl.DateTimeFormat(locale, {
      day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
    })
    const when: I18n['when'] = (ms) => (ms ? dtf.format(new Date(ms)) : '–')
    return { lang, locale, setLang, t, num, ago, when }
  }, [lang, setLang])

  return <Ctx.Provider value={value}>{children}</Ctx.Provider>
}

export function useI18n() {
  const v = useContext(Ctx)
  if (!v) throw new Error('useI18n outside I18nProvider')
  return v
}
