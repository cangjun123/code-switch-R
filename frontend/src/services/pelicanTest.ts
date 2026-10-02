import { Call, Events } from '@wailsio/runtime'

const SERVICE = 'codeswitch/services.PelicanTestService'

export const PELICAN_PROMPT = '创建一个 HTML，内容是 SVG 绘制一个鹈鹕骑自行车的2D 动画'

export interface PelicanTestResult {
  sessionId: string
  providerId: number
  model: string
  actualModel: string
  status: 'running' | 'completed' | 'failed' | 'cancelled'
  errorCode?: string
  message?: string
  rawOutput: string
  html: string
  previewToken?: string
  firstTokenMs: number | null
  elapsedMs: number
  totalBytes: number
}

export interface PelicanStreamEvent {
  sessionId: string
  chunk: string
  startBytes: number
  totalBytes: number
}

export const startPelicanTest = (platform: string, providerId: number, model: string): Promise<PelicanTestResult> =>
  Call.ByName(`${SERVICE}.StartTest`, platform, providerId, model)

export const getPelicanTest = (sessionId: string): Promise<PelicanTestResult> =>
  Call.ByName(`${SERVICE}.GetTest`, sessionId)

export const cancelPelicanTest = (sessionId: string): Promise<void> =>
  Call.ByName(`${SERVICE}.CancelTest`, sessionId)

export const subscribePelicanStream = (callback: (data: PelicanStreamEvent) => void): (() => void) =>
  Events.On('pelican:stream', (event) => callback((event as { data: PelicanStreamEvent }).data))

export const subscribePelicanProgress = (callback: (data: { sessionId: string; status: string }) => void): (() => void) =>
  Events.On('pelican:progress', (event) => callback((event as { data: { sessionId: string; status: string } }).data))

export const pelicanPreviewURL = (result: PelicanTestResult): string =>
  `/api/pelican/preview/${encodeURIComponent(result.sessionId)}?token=${encodeURIComponent(result.previewToken || '')}`
