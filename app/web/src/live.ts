// Live playback of the /api/cameras/{id}/live fMP4 websocket stream via the
// MediaSource API (ManagedMediaSource on iOS 17.1+).

export function mediaSourceClass(): typeof MediaSource | null {
  const w = window as unknown as { ManagedMediaSource?: typeof MediaSource; MediaSource?: typeof MediaSource }
  return w.ManagedMediaSource ?? w.MediaSource ?? null
}

export function liveSupported(): boolean {
  return mediaSourceClass() !== null
}

// parseCodecs extracts the MediaSource codec string from the fMP4 init
// segment: the avcC box carries profile/compat/level; an mp4a box means an
// AAC-LC audio track is present.
function parseCodecs(init: Uint8Array): string | null {
  const find = (needle: string): number => {
    const bytes = [...needle].map((c) => c.charCodeAt(0))
    outer: for (let i = 0; i + bytes.length <= init.length; i++) {
      for (let j = 0; j < bytes.length; j++) {
        if (init[i + j] !== bytes[j]) continue outer
      }
      return i
    }
    return -1
  }

  const avcc = find('avcC')
  if (avcc < 0 || avcc + 8 > init.length) return null
  const hex = (b: number) => b.toString(16).padStart(2, '0')
  // avcC layout: 4CC, configurationVersion, profile, compat, level.
  let codecs = `avc1.${hex(init[avcc + 5])}${hex(init[avcc + 6])}${hex(init[avcc + 7])}`
  if (find('mp4a') >= 0) {
    codecs += ', mp4a.40.2'
  }
  return `video/mp4; codecs="${codecs}"`
}

export interface LiveSession {
  stop: () => void
}

export function startLive(
  video: HTMLVideoElement,
  cameraId: string,
  onError: (message: string) => void,
): LiveSession {
  const MS = mediaSourceClass()
  if (!MS) {
    onError('MediaSource not supported by this browser')
    return { stop: () => {} }
  }

  const wsURL = new URL(`./api/cameras/${cameraId}/live`, window.location.href)
  wsURL.protocol = wsURL.protocol === 'https:' ? 'wss:' : 'ws:'

  const mediaSource = new MS()
  // Required for ManagedMediaSource (iOS).
  video.disableRemotePlayback = true
  video.src = URL.createObjectURL(mediaSource)

  let sourceBuffer: SourceBuffer | null = null
  let sourceOpen = false
  let stopped = false
  let initBuffer = new Uint8Array(0)
  const queue: Uint8Array[] = []

  const socket = new WebSocket(wsURL)
  socket.binaryType = 'arraybuffer'

  const fail = (message: string) => {
    if (stopped) return
    stop()
    onError(message)
  }

  const pump = () => {
    if (!sourceBuffer || sourceBuffer.updating || queue.length === 0) return
    try {
      sourceBuffer.appendBuffer(queue.shift()! as BufferSource)
    } catch {
      fail('playback buffer error')
    }
  }

  const keepLive = () => {
    // Stay close to the live edge: if buffering ran ahead (tab was hidden,
    // network hiccup), jump forward instead of playing minutes behind.
    if (video.buffered.length > 0) {
      const end = video.buffered.end(video.buffered.length - 1)
      if (end - video.currentTime > 3) {
        video.currentTime = end - 0.5
      }
      // Trim history so long sessions don't grow the buffer unbounded.
      const start = video.buffered.start(0)
      if (sourceBuffer && !sourceBuffer.updating && video.currentTime - start > 30) {
        try {
          sourceBuffer.remove(start, video.currentTime - 15)
        } catch {
          // non-fatal
        }
      }
    }
  }

  const tryInitSourceBuffer = () => {
    if (sourceBuffer || !sourceOpen || initBuffer.length === 0) return
    const mime = parseCodecs(initBuffer)
    if (!mime) {
      // moov not complete yet; wait for more data (bail out at 512 KiB).
      if (initBuffer.length > 512 * 1024) fail('could not detect stream codec')
      return
    }
    if (!MS.isTypeSupported(mime)) {
      fail(`codec not supported: ${mime}`)
      return
    }
    sourceBuffer = mediaSource.addSourceBuffer(mime)
    sourceBuffer.addEventListener('updateend', () => {
      keepLive()
      pump()
    })
    queue.push(initBuffer)
    initBuffer = new Uint8Array(0)
    pump()
    video.play().catch(() => {})
  }

  mediaSource.addEventListener('sourceopen', () => {
    sourceOpen = true
    tryInitSourceBuffer()
  })

  socket.onmessage = (event) => {
    const chunk = new Uint8Array(event.data as ArrayBuffer)
    if (!sourceBuffer) {
      const merged = new Uint8Array(initBuffer.length + chunk.length)
      merged.set(initBuffer)
      merged.set(chunk, initBuffer.length)
      initBuffer = merged
      tryInitSourceBuffer()
      return
    }
    queue.push(chunk)
    // Drop backlog if the browser cannot keep up.
    while (queue.length > 60) queue.shift()
    pump()
  }

  socket.onerror = () => fail('stream connection failed')
  socket.onclose = () => {
    if (!stopped && sourceBuffer === null) fail('stream ended before playback started')
  }

  const stop = () => {
    if (stopped) return
    stopped = true
    socket.close()
    try {
      if (mediaSource.readyState === 'open') mediaSource.endOfStream()
    } catch {
      // already detached
    }
    video.removeAttribute('src')
    video.load()
  }

  return { stop }
}
