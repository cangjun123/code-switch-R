#!/bin/bash
# ModelTrace 进度事件端到端实测脚本
set -u
cd /home/gcj/build/code-switch-R

export MT_HOME=$(mktemp -d)
export HOME=$MT_HOME
export CODE_SWITCH_WEB_ADDR=127.0.0.1:18099

node scripts/mt-e2e/mock-upstream.mjs > /tmp/mt-mock.log 2>&1 &
MOCK_PID=$!
./codeswitch-web > /tmp/mt-app.log 2>&1 &
APP_PID=$!
sleep 3

TOKEN=$(grep -oE "setup token[^:]*: [A-Za-z0-9_-]+" /tmp/mt-app.log | grep -oE "[A-Za-z0-9_-]+$" | head -1)
echo "== setup token: ${TOKEN:0:8}..."

echo "== initialize admin"
curl -s -c /tmp/mt-cookies.txt -X POST http://127.0.0.1:18099/api/admin/initialize \
  -H "Content-Type: application/json" \
  -d "{\"username\":\"admin\",\"password\":\"test12345\",\"setupToken\":\"$TOKEN\"}" > /tmp/mt-init.json
head -c 200 /tmp/mt-init.json; echo

echo "== save provider (mock upstream)"
curl -s -b /tmp/mt-cookies.txt -X POST http://127.0.0.1:18099/api/wails/call \
  -H "Content-Type: application/json" \
  -d '{"name":"codeswitch/services.ProviderService.SaveProviders","args":["claude",[{"id":1,"name":"mock","apiUrl":"http://127.0.0.1:18888","apiKey":"sk-test","enabled":true}]]}' | head -c 300; echo

echo "== open SSE subscription"
curl -s -N -b /tmp/mt-cookies.txt http://127.0.0.1:18099/api/wails/events > /tmp/mt-sse.log 2>&1 &
SSE_PID=$!
sleep 1

echo "== trigger verification"
curl -s -b /tmp/mt-cookies.txt -X POST http://127.0.0.1:18099/api/wails/call \
  -H "Content-Type: application/json" \
  -d '{"name":"codeswitch/services.ModelTraceService.VerifyProviderModel","args":["claude",1,"gpt-5.4"]}' > /tmp/mt-verify.json &
VERIFY_PID=$!

sleep 12
echo "== SSE events received:"
cat /tmp/mt-sse.log

echo "== verify result:"
head -c 400 /tmp/mt-verify.json; echo

kill $SSE_PID $VERIFY_PID $APP_PID $MOCK_PID 2>/dev/null
wait 2>/dev/null
echo "== done"
