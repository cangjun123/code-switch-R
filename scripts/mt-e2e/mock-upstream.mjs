// Mock Anthropic 上游：延迟 4s 后返回一条真实指纹回答（来自 golden.json gpt-5.4 case）
import { createServer } from 'node:http'
import { readFileSync } from 'node:fs'

const golden = JSON.parse(readFileSync(new URL('/home/gcj/build/code-switch-R/services/modeltrace/testdata/golden.json', 'file:///')))
const answerText = golden[0].outputs[0].text

const server = createServer((req, res) => {
  let body = ''
  req.on('data', (chunk) => { body += chunk })
  req.on('end', () => {
    console.log(`[mock] ${req.method} ${req.url}`)
    if (req.url.includes('/v1/messages')) {
      setTimeout(() => {
        const payload = {
          id: 'msg_mock',
          type: 'message',
          role: 'assistant',
          model: 'claude-3-5-sonnet',
          content: [{ type: 'text', text: answerText }],
          stop_reason: 'end_turn',
        }
        res.writeHead(200, { 'Content-Type': 'application/json' })
        res.end(JSON.stringify(payload))
        console.log('[mock] replied with', answerText.length, 'chars')
      }, 4000)
    } else {
      res.writeHead(404)
      res.end('{}')
    }
  })
})
server.listen(18888, '127.0.0.1', () => console.log('[mock] listening on 18888'))
