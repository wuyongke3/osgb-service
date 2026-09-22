#!/usr/bin/env bash
set -Eeuo pipefail

# Docker/Linux installation entry point. The actual package installation stays
# in Dockerfile so builds are repeatable and the running Web service never gets
# apt or Docker-socket privileges.
cd "$(dirname "${BASH_SOURCE[0]}")/.."

command -v docker >/dev/null 2>&1 || {
  echo "未检测到 docker，请先安装 Docker Engine 或 Docker Desktop。" >&2
  exit 1
}
docker compose version >/dev/null 2>&1 || {
  echo "未检测到 docker compose 插件，请先安装 Docker Compose v2。" >&2
  exit 1
}

echo "校验 Compose 配置..."
docker compose config >/dev/null
echo "构建镜像并安装/更新 COLMAP、OpenMVS、OpenSceneGraph..."
docker compose build --pull
echo "重新创建服务..."
docker compose up -d --force-recreate
echo "完成。查看状态：docker compose ps"
