FROM node:22-alpine
WORKDIR /workspace
COPY package.json package-lock.json ./
COPY packages/telemetry/package.json packages/telemetry/package.json
COPY packages/contracts/package.json packages/contracts/package.json
COPY packages/rpc/package.json packages/rpc/package.json
COPY apps/platform-api/package.json apps/platform-api/package.json
COPY services/root-supervisor/package.json services/root-supervisor/package.json
RUN npm ci --no-audit --no-fund
COPY packages/telemetry packages/telemetry
COPY packages/contracts packages/contracts
COPY packages/rpc packages/rpc
COPY apps/platform-api apps/platform-api
COPY contracts contracts
RUN npm run build -w @lawpilot/telemetry && npm run build -w @lawpilot/contracts && npm run build -w @lawpilot/rpc && npm run build -w @lawpilot/platform-api
ENV CAPABILITY_REGISTRY_PATH=/workspace/contracts/capabilities/registry.v1.json
EXPOSE 3000
USER node
CMD ["npm","run","start","-w","@lawpilot/platform-api"]
