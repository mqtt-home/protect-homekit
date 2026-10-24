import { useMemo } from 'react'
import { createI18n, LangSwitch as BaseLangSwitch, type I18n as BaseI18n } from '@mqtt-home/ui/i18n'

export { LANGS, resolveText, type Lang, type Text } from '@mqtt-home/ui/i18n'

// UI strings only. Camera names, models, NVR names and browser/stream error
// details come from elsewhere and stay as they are.

// German is the house language; English stays one tap away (per device).
const dict = {
  de: {
    title: 'Kameras',
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
    'crash.title': 'Die Seite ist auf einen Fehler gestoßen',
    'crash.body': 'Die Bridge läuft weiter. Rohdaten:',
    'crash.reload': 'Neu laden',
  },
  en: {
    title: 'Cameras',
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
    'crash.title': 'The page hit an error',
    'crash.body': 'The bridge keeps running. Raw data:',
    'crash.reload': 'Reload',
  },
}

export type Key = keyof (typeof dict)['de']

/** The house helper, with times as Unix milliseconds (the camera API's unit). */
export type I18n = Omit<BaseI18n<Key>, 'num' | 'ago'> & {
  /** A number in the current locale; null counts as 0. */
  num: (n: number | null | undefined, decimals?: number) => string
  /** "vor 3 min" / "3 min ago" for a Unix time in milliseconds; 0 = never. */
  ago: (ms: number | null | undefined, now?: number) => string
  /** Date and 24h time for a Unix time in milliseconds: "18.10., 11:49". */
  when: (ms: number | null | undefined) => string
}

const base = createI18n(dict)
export const I18nProvider = base.I18nProvider

export function useI18n(): I18n {
  const i = base.useI18n()
  return useMemo(() => {
    const num: I18n['num'] = (n, decimals) => i.num(n ?? 0, decimals)
    const ago: I18n['ago'] = (ms, now) => (ms ? i.ago(new Date(ms).toISOString(), now) : i.t('ago.never'))
    const dtf = new Intl.DateTimeFormat(i.locale, {
      day: '2-digit', month: '2-digit', hour: '2-digit', minute: '2-digit', hourCycle: 'h23',
    })
    const when: I18n['when'] = (ms) => (ms ? dtf.format(new Date(ms)) : '–')
    return { ...i, num, ago, when }
  }, [i])
}

/** The DE/EN switch. */
export function LangSwitch() {
  return <BaseLangSwitch i18n={base.useI18n()} />
}
