#!/usr/bin/env bash
# 测试：从本地 token 服务获取 access_token，再调用微信关注列表接口

set -e

BASE_URL="http://localhost:8080"
CLIENT_APPID="client-1"
CLIENT_SECRET="your_client_secret"
WECHAT_APPID="wechat_appid"
PRIVATE_KEY="./config/rsa_private_key.pem"

echo "===== Step 1: 生成 RSA 签名 ====="
TIMESTAMP=$(date +%s)
SIGN_STR="${CLIENT_APPID}|${CLIENT_SECRET}|${TIMESTAMP}"
echo "签名原文: ${SIGN_STR}"

SIGNATURE=$(echo -n "${SIGN_STR}" | openssl dgst -sha256 -sign "${PRIVATE_KEY}" | base64)
echo "签名(Base64): ${SIGNATURE:0:40}..."

echo ""
echo "===== Step 2: 从 token 服务获取 access_token ====="
TOKEN_RESP=$(curl -s \
  -H "X-AppId: ${CLIENT_APPID}" \
  -H "X-Timestamp: ${TIMESTAMP}" \
  -H "X-Signature: ${SIGNATURE}" \
  "${BASE_URL}/api/v1/wechat/token?appid=${WECHAT_APPID}")

echo "响应: ${TOKEN_RESP}"

ACCESS_TOKEN=$(echo "${TOKEN_RESP}" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d['data']['access_token'])" 2>/dev/null)

if [ -z "${ACCESS_TOKEN}" ]; then
  echo "❌ 获取 access_token 失败，终止测试"
  exit 1
fi

echo "✅ access_token: ${ACCESS_TOKEN:0:20}..."

echo ""
echo "===== Step 3: 调用微信关注列表接口 ====="
FOLLOWERS_RESP=$(curl -s \
  "https://api.weixin.qq.com/cgi-bin/user/get?access_token=${ACCESS_TOKEN}&next_openid=")

echo "响应: ${FOLLOWERS_RESP}"

TOTAL=$(echo "${FOLLOWERS_RESP}" | python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('count', d.get('errcode','unknown')))" 2>/dev/null)
echo ""
echo "===== 测试结果 ====="
if echo "${FOLLOWERS_RESP}" | grep -q '"count"'; then
  echo "✅ token 有效，关注人数: ${TOTAL}"
else
  echo "❌ 微信接口返回错误: ${FOLLOWERS_RESP}"
fi
