# xm2api —— Node 反代 + 管理界面（容器版）
#
#   构建：docker build -t xm2api .
#   运行：docker run -d -p 18787:18787 \
#           -v %CD%/data:/app/data -v %CD%/config.yaml:/app/config.yaml:ro xm2api
#   管理界面：http://<host>:18787/ui/（管理密钥见 data/admin-key.txt 或 XM2API_ADMIN_KEY）
#
# 说明：
#   - 运行依赖只有 yaml（playwright 属 devDependencies，容器里不装）；
#   - 容器内必须绑 0.0.0.0 才能被端口映射访问 —— Host 校验靠
#     XM2API_ALLOWED_HOSTS 白名单放行（见 docker-compose.yml 与 config.yaml 注释）；
#   - Windows 本机提取凭证（creds/一键提取）在容器里不可用（依赖 MiMo 桌面客户端），
#     容器场景用管理界面「在线登录」或导入 mimo.json 灌凭证。
FROM node:24-alpine
WORKDIR /app

COPY package.json package-lock.json ./
RUN npm ci --omit=dev

COPY lib/ lib/
COPY ui/ ui/
COPY server.mjs menu.mjs creds.mjs cpa-auth.mjs config.yaml ./

ENV XM2API_HOST=0.0.0.0
VOLUME ["/app/data", "/app/logs"]
EXPOSE 18787

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s \
  CMD node -e "fetch('http://127.0.0.1:18787/__xm2api').then(r=>process.exit(r.ok?0:1)).catch(()=>process.exit(1))"

CMD ["node", "server.mjs"]
