#!/bin/bash
# ModelTrace 流式回答 E2E 实测：mock 流式上游 + 真实二进制 + SSE 订阅
set -u
cd /home/gcj/build/code-switch-R

export MT_HOME=$(mktemp -d)
export HOME=$MT_HOME
export CODE_SWITCH_WEB_ADDR=127.0.0.1:18099

node scripts/mt-e2e/mock-upstream-stream.mjs > /tmp/mt-mock.log 2>&1 &
MOCK_PID=$!
./codeswitch-web > /tmp/mt-app.log 2>&1 &
APP_PID=$!
sleep 3

TOKEN=$(grep -oE "setup token[^:]*: [A-Za-z0-9_-]+" /tmp/mt-app.log | grep -oE "[A-Za-z0-9_-]+$" | head -1)

curl -s -c /tmp/mt-cookies.txt -X POST http://127.0.0.1:18099/api/admin/initialize \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"admin\",\"password\":\"test12345\",\"setupToken\":\"$TOKEN\"}" > /dev/null

curl -s -b /tmp/mt-cookies.txt -X POST http://127.0.0.1:18099/api/wails/call \
  -H "Content-Type: application/json" \
  -d '{"name":"codeswitch/services.ProviderService.SaveProviders","args":["claude",[{"id":1,"name":"mock","apiUrl":"http://127.0.0.1:18888","apiKey":"sk-test","enabled":true}]]}' > /dev/null

curl -s -N -b /tmp/mt-cookies.txt http://127.0.0.1:18099/api/wails/events > /tmp/mt-sse.log 2>&1 &
SSE_PID=$!
sleep 1

curl -s -b /tmp/mt-cookies.txt -X POST http://127.0.0.1:18099/api/wails/call \
  -H "Content-Type: application/json" \
  -d '{"name":"codeswitch/services.ModelTraceService.VerifyProviderModel","args":["claude",1,"gpt-5.4"]}' > /tmp/mt-verify.json &
VERIFY_PID=$!

# 流式回答 802 字符 / 30字符片 / 200ms ≈ 5.4s
sleep 10

echo "== stream events count & samples:"
grep -c "modeltrace:stream" /tmp/mt-sse.log
grep "modeltrace:stream" /tmp/mt-sse.log | head -3
echo "== progress events:"
grep -oE "\"detail\":\"[^\"]+\"" /tmp/mt-sse.log | head -5
echo "== verify verdict:"
grep -oE '"verdict":"[a-z]+"|"topModel":"[a-z0-9.-]+"' /tmp/mt-verify.json | head -3
echo "== total stream chars reassembled:"
node -e "
const lines = require('fs').readFileSync('/tmp/mt-sse.log','utf8').split('\n');
let total = 0;
for (let i = 0; i < lines.length - 1; i++) {
  if (lines[i].startsWith('event: modeltrace:stream')) {
    const data = JSON.parse(lines[i+1].replace('data: ',''));
    total += data.chunk.length;
  }
}
console.log(total, '(原文 802)')"

kill $SSE_PID $VERIFY_PID $APP_PID $MOCK_PID 2>/dev/null
wait 2>/dev/null
echo "== done"
