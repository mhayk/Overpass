# web — the Next.js frontend (#166). Same shape as the service pattern in
# services/tasking-api/Dockerfile: repo-root context, one directory copied in,
# multi-stage so the shipped image runs the built app rather than carrying the
# toolchain that built it.
#
# NEXT_PUBLIC_* values are inlined at BUILD time, not read at runtime. The
# defaults baked into the source (gateway on localhost:8083, tasking-api on
# localhost:8080) are correct for this compose file, because the browser —
# not this container — is what calls those services, and the browser is on
# the host. Pointing the frontend elsewhere means rebuilding the image, which
# is a Next.js property, not a choice made here.
FROM node:22-alpine AS build
# Overridable at build time for hosts where 8080 is already taken; the
# default matches DEFAULT_TASKING_URL in src/lib/tasking.ts.
ARG NEXT_PUBLIC_TASKING_API_URL=http://localhost:8080
ENV NEXT_PUBLIC_TASKING_API_URL=${NEXT_PUBLIC_TASKING_API_URL}
WORKDIR /app
COPY web/package.json web/package-lock.json ./
COPY web/scripts ./scripts
# `npm ci`, not `npm install`: the lockfile is the authority, same rule as
# uv --frozen next door. postinstall copies the Cesium static assets, which
# is why scripts/ must arrive before it runs.
RUN npm ci
COPY web .
RUN npm run build

FROM node:22-alpine
WORKDIR /app
# --chown at copy time: `next start` writes to .next/cache, and a root-owned
# tree under a non-root user fails there at runtime, not at build.
COPY --from=build --chown=node:node /app .
USER node
EXPOSE 3000
CMD ["npm", "run", "start"]
