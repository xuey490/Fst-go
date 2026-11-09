#!/bin/bash
# FST 移动端核心端到端联调：两个守护进程互相发现、发消息、传文件并校验 SHA-256。
set -u

A_DIR=$(mktemp -d)
B_DIR=$(mktemp -d)
LOG_A=/tmp/ea.log
LOG_B=/tmp/eb.log
: > $LOG_A
: > $LOG_B

FST_DATA_DIR=$A_DIR FST_TOKEN=ta FST_DEVICE_NAME=A FST_PORT=8787 timeout 200 /tmp/fstcored.exe > $LOG_A 2>&1 &
PA=$!
sleep 2
# B 必须换端口：Windows 允许两个进程同时绑 8787，不隔离就分不清连接进了谁。
FST_DATA_DIR=$B_DIR FST_TOKEN=tb FST_DEVICE_NAME=B FST_PORT=18787 timeout 200 /tmp/fstcored.exe > $LOG_B 2>&1 &
PB=$!

A=""
B=""
for _ in $(seq 1 40); do
  A=$(grep -o "127\.0\.0\.1:[0-9]*" $LOG_A | head -1)
  B=$(grep -o "127\.0\.0\.1:[0-9]*" $LOG_B | head -1)
  [ -n "$A" ] && [ -n "$B" ] && break
  sleep 0.5
done
echo "A=$A B=$B"

AID=$(curl -s -H "X-FST-Token: ta" "http://$A/self" | grep -o '"peer_id":"[^"]*"' | cut -d'"' -f4)
BID=$(curl -s -H "X-FST-Token: tb" "http://$B/self" | grep -o '"peer_id":"[^"]*"' | cut -d'"' -f4)
echo "AID=$AID"
echo "BID=$BID"

echo "--- 注入对端地址（等价 Android 的 NsdManager）---"
# 移动端核心以 DisableMDNS 运行：Android 11+ 禁 NETLINK_ROUTE，Go 侧 mDNS 不可用，
# 发现由系统 DNS-SD 代劳后再 POST /discovered 回灌。这里用 /self 里的地址做同样的事，
# 保证脚本验证的是真机实际走的那条路径。
pick_addr() {
  local tok=$1 base=$2
  local a
  a=$(curl -s -H "X-FST-Token: $tok" "http://$base/self" \
    | grep -o '/ip4/[0-9.]*/tcp/[0-9]*' | grep -v '127\.0\.0\.1' | head -1)
  if [ -z "$a" ]; then
    local p
    p=$(curl -s -H "X-FST-Token: $tok" "http://$base/self" | grep -o '"port":[0-9]*' | head -1 | cut -d: -f2)
    a="/ip4/127.0.0.1/tcp/$p"
  fi
  echo "$a"
}
AADDR=$(pick_addr ta "$A")
BADDR=$(pick_addr tb "$B")
echo "A=$AADDR B=$BADDR"
curl -s -X POST -H "X-FST-Token: ta" -d "{\"addr\":\"$BADDR/p2p/$BID\"}" "http://$A/discovered"; echo
curl -s -X POST -H "X-FST-Token: tb" -d "{\"addr\":\"$AADDR/p2p/$AID\"}" "http://$B/discovered"; echo

echo "--- 等待互相发现 ---"
for _ in $(seq 1 20); do
  curl -s -H "X-FST-Token: ta" "http://$A/peers" | grep -q "$BID" && break
  sleep 0.5
done
echo -n "A 看到 B: "
curl -s -H "X-FST-Token: ta" "http://$A/peers" | grep -c "$BID"

echo "--- 发文本 ---"
curl -s -X POST -H "X-FST-Token: ta" -d "{\"peer\":\"$BID\",\"text\":\"hello-from-A\"}" "http://$A/send"
echo
sleep 2
echo -n "B 收到的消息: "
curl -s -H "X-FST-Token: tb" "http://$B/messages?peer=$AID&limit=5"
echo

echo "--- 传文件 ---"
head -c 1048576 /dev/urandom > /tmp/payload.bin
WPAY=$(cygpath -w /tmp/payload.bin | sed 's|\\|/|g')
WANT=$(sha256sum /tmp/payload.bin | cut -d' ' -f1)
echo "payload=$WPAY want=$WANT"
curl -s -X POST -H "X-FST-Token: ta" -d "{\"peer\":\"$BID\",\"paths\":[\"$WPAY\"]}" "http://$A/sendfiles"
echo
sleep 3

TR=$(curl -s -H "X-FST-Token: tb" "http://$B/transfers?limit=5")
echo "B 的传输: $TR"
BATCH=$(echo "$TR" | grep -o '"batch_id":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "batch=$BATCH"
curl -s -X POST -H "X-FST-Token: tb" -d "{\"batch\":\"$BATCH\"}" "http://$B/accept"
echo
for _ in $(seq 1 40); do
  TR=$(curl -s -H "X-FST-Token: tb" "http://$B/transfers?limit=5")
  echo "$TR" | grep -q '"status":"done"' && break
  sleep 1
done
echo "最终: $TR"
LP=$(echo "$TR" | grep -o '"local_path":"[^"]*"' | head -1 | cut -d'"' -f4)
echo "local_path=$LP"
if [ -n "$LP" ] && [ -f "$LP" ]; then
  echo "got =$(sha256sum "$LP" | cut -d' ' -f1)"
  echo "want=$WANT"
else
  echo "本地文件缺失"
fi

kill $PA $PB 2>/dev/null
