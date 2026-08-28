FROM oven/bun:1.4.0-alpine@sha256:07235578f79ef8c6f97d94aee7938e76f5cdba5f21ae5dbfdd3d3d38058437eb

LABEL org.opencontainers.image.source="https://github.com/novelKR/OpenCodex-OCI-Gateway"
LABEL org.opencontainers.image.version="v0.3.0"
LABEL org.opencontainers.image.licenses="Apache-2.0 AND MIT"

WORKDIR /opt/opencodex
COPY package.json bun.lock ./
RUN bun install --frozen-lockfile --production \
    && test "$(bun -e 'console.log(require("./node_modules/@bitkyc08/opencodex/package.json").version)')" = "2.33.0" \
    && rm -rf /root/.bun/install/cache

ENV HOME=/var/lib/opencodex
ENV NODE_ENV=production
ENV OCX_SERVICE=1

RUN mkdir -p /var/lib/opencodex \
    && chown -R 10001:10001 /var/lib/opencodex \
    && chmod -R a-w /opt/opencodex

USER 10001:10001
EXPOSE 10100
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["bun", "-e", "fetch('http://127.0.0.1:10100/health').then(r=>{if(!r.ok)process.exit(1)}).catch(()=>process.exit(1))"]
ENTRYPOINT ["/opt/opencodex/node_modules/.bin/ocx"]
CMD ["start", "--port", "10100"]
