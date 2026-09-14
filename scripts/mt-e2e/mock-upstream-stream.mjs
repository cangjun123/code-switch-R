// Mock Anthropic 上游（流式 SSE）：按 200ms/片 流式吐出真实指纹回答
import { createServer } from 'node:http'
import { readFileSync } from 'node:fs'

const golden = JSON.parse(readFileSync('/home/gcj/build/code-switch-R/services/modeltrace/testdata/golden.json', 'utf8'))
const answerText = golden[0].outputs[0].text

const server = createServer((req, res) => {
  let body = ''
  req.on('data', (chunk) => { body += chunk })
  req.on('end', () => {
    console.log(`[mock] ${req.method} ${req.url} stream=${body.includes('"stream":true')}`)
    if (!req.url.includes('/v1/messages')) {
      res.writeHead(404)
      res.end('{}')
      return
    }
    res.writeHead(200, {
      'Content-Type': 'text/event-stream',
      'Cache-Control': 'no-cache',
    })
    // 按每片 30 字符、200ms 间隔流式发送
    const chunkSize = 30
    let offset = 0
    const timer = setInterval(() => {
      if (offset >= answerText.length) {
        clearInterval(timer)
        res.write('event: message_delta\ndata: {"type":"message_delta","delta":{},"stop_reason":"end_turn"}\n\n')
        res.write('event: message_stop\ndata: {"type":"message_stop"}\n\n')
        res.end()
        return
      }
      const piece = answerText.slice(offset, offset + chunkSize)
      offset += chunkSize
      const payload = { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: piece } }
      res.write(`event: content_block_delta\ndata: ${JSON.stringify(payload)}\n\n`)
    }, 200)
  })
})
server.listen(18888, '127.0.0.1', () => console.log('[mock-stream] listening on 18888'))
