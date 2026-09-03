FROM oven/bun:1.4.0-alpine@sha256:07235578f79ef8c6f97d94aee7938e76f5cdba5f21ae5dbfdd3d3d38058437eb AS common

ARG IMAGE_VERSION=unreleased
ARG OPENCODEX_VERSION=unknown
ARG OPENCODEX_REVISION=unknown
ARG PUBLIC_CORE_REVISION=unknown
LABEL org.opencontainers.image.source="https://github.com/novelKR/OpenCodex-OCI-Gateway"
LABEL org.opencontainers.image.version="${IMAGE_VERSION}"
LABEL org.opencontainers.image.licenses="Apache-2.0 AND MIT"
LABEL io.github.novelkr.opencodex.upstream.version="${OPENCODEX_VERSION}"
LABEL io.github.novelkr.opencodex.upstream.revision="${OPENCODEX_REVISION}"
LABEL io.github.novelkr.opencodex.public-core.revision="${PUBLIC_CORE_REVISION}"

WORKDIR /opt/opencodex
COPY package.json bun.lock upstream.lock.json ./
RUN bun install --frozen-lockfile --production \
    && bun -e 'const lock = await Bun.file("upstream.lock.json").json(); const declared = require("./package.json").dependencies["@bitkyc08/opencodex"]; const installed = require("./node_modules/@bitkyc08/opencodex/package.json"); if (lock.schema !== 1 || lock.npm.package !== "@bitkyc08/opencodex" || lock.version !== lock.npm.version || declared !== lock.version || installed.name !== lock.npm.package || installed.version !== lock.version) process.exit(1)' \
    && rm -rf /root/.bun/install/cache
COPY --chmod=0444 bootstrap.mjs ./bootstrap.mjs

ENV HOME=/var/lib/opencodex
ENV NODE_ENV=production
ENV OCX_SERVICE=1

RUN mkdir -p /var/lib/opencodex \
    && chown -R 10001:10001 /var/lib/opencodex \
    && chmod -R a-w /opt/opencodex

USER 10001:10001
EXPOSE 10100
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD ["bun", "-e", "fetch('http://127.0.0.1:10100/healthz').then(r=>{if(!r.ok)process.exit(1)}).catch(()=>process.exit(1))"]

FROM common AS runtime
ENV OPENCODEX_HOME=/var/lib/opencodex
ENTRYPOINT ["bun", "/opt/opencodex/bootstrap.mjs"]

# Keep the pre-existing gateway as the default final stage for direct builds
# and compose users that do not select a target. The runtime lifecycle always
# selects the separate `runtime` stage explicitly.
FROM common AS gateway
ENTRYPOINT ["/opt/opencodex/node_modules/.bin/ocx"]
CMD ["start", "--port", "10100"]
