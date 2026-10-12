# syntax=docker/dockerfile:1@sha256:ecfaec9ed6d810b56388c508f4121597bfbba70d41a6dfeee4d8cad5f295fc32
FROM node:24-bookworm-slim@sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e AS builder
WORKDIR /src
COPY package.json package-lock.json ./
RUN npm ci
COPY tsconfig.json ./
COPY src ./src
RUN npm run build
# Release-versioned product tool layer (DES-HOR-651-01). An invalid product
# tool fails the image build rather than an install.
COPY tools/product ./tools/product
RUN PRODUCT_TOOLS_ROOT=/src/tools/product node dist/main.js validate

FROM node:24-bookworm-slim@sha256:ba849c60be29959425b8734d57b8b4b7d56f98edd9504c9af091d5281095a71e
ARG VERSION=dev
ENV TOOL_RUNNER_BUILD_VERSION=${VERSION}
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY --from=builder /src/dist ./dist
COPY --from=builder /src/tools/product ./tools/product
USER 65532:65532
ENTRYPOINT ["node", "dist/main.js"]
