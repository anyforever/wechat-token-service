#!/bin/bash

# 多平台构建脚本
# 为 Linux、macOS、Windows 平台编译二进制文件

set -e

# 项目信息
APP_NAME="wechat-token-manager"
VERSION=${VERSION:-"1.0.0"}
BUILD_DIR="build"
MAIN_FILE="main.go"

# 颜色输出
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m' # No Color

echo -e "${GREEN}开始构建 ${APP_NAME} v${VERSION}${NC}"
echo ""

# 清理旧的构建目录
rm -rf ${BUILD_DIR}
mkdir -p ${BUILD_DIR}

# 构建信息
BUILD_TIME=$(date +"%Y-%m-%d %H:%M:%S")
GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")

# 通用 ldflags (-s 禁用符号表, -w 禁用 DWARF 调试信息)
LDFLAGS="-s -w"

# 定义目标平台
PLATFORMS=(
    "linux/amd64"
    "linux/arm64"
    "darwin/amd64"
    "darwin/arm64"
    "windows/amd64"
    "windows/arm64"
)

# 编译函数
build() {
    local OS=$1
    local ARCH=$2
    local OUTPUT_NAME="${APP_NAME}"
    
    # Windows 需要添加 .exe 后缀
    if [ "$OS" = "windows" ]; then
        OUTPUT_NAME="${OUTPUT_NAME}.exe"
    fi
    
    local OUTPUT_DIR="${BUILD_DIR}/${OS}-${ARCH}"
    local OUTPUT_PATH="${OUTPUT_DIR}/${OUTPUT_NAME}"
    
    echo -e "${YELLOW}正在编译: ${OS}/${ARCH}${NC}"
    
    mkdir -p "${OUTPUT_DIR}"
    
    GOOS=${OS} GOARCH=${ARCH} go build -ldflags "${LDFLAGS}" -o "${OUTPUT_PATH}" ${MAIN_FILE}
    
    # 压缩文件
    if [ "$OS" = "windows" ]; then
        cd "${OUTPUT_DIR}"
        zip -q "${APP_NAME}-${OS}-${ARCH}-${VERSION}.zip" "${OUTPUT_NAME}"
        cd - > /dev/null
        echo -e "  ${GREEN}✓${NC} ${OUTPUT_DIR}/${APP_NAME}-${OS}-${ARCH}-${VERSION}.zip"
    else
        tar -czf "${OUTPUT_DIR}/${APP_NAME}-${OS}-${ARCH}-${VERSION}.tar.gz" -C "${OUTPUT_DIR}" "${OUTPUT_NAME}"
        echo -e "  ${GREEN}✓${NC} ${OUTPUT_DIR}/${APP_NAME}-${OS}-${ARCH}-${VERSION}.tar.gz"
    fi
}

# 遍历所有平台进行编译
for PLATFORM in "${PLATFORMS[@]}"; do
    IFS='/' read -r OS ARCH <<< "${PLATFORM}"
    build "${OS}" "${ARCH}"
done

echo ""
echo -e "${GREEN}构建完成！${NC}"
echo ""
echo "生成的文件:"
ls -la ${BUILD_DIR}/*/
echo ""
echo "压缩包列表:"
find ${BUILD_DIR} -name "*.tar.gz" -o -name "*.zip" | sort
