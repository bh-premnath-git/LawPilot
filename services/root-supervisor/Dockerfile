FROM node:22-alpine
WORKDIR /workspace
COPY package.json ./
COPY packages/contracts/package.json packages/contracts/package.json
COPY apps/platform-api/package.json apps/platform-api/package.json
COPY services/root-supervisor/package.json services/root-supervisor/package.json
RUN npm install --no-audit --no-fund
COPY packages/contracts packages/contracts
COPY services/root-supervisor services/root-supervisor
COPY contracts contracts
RUN npm run build -w @lawpilot/contracts && npm run build -w @lawpilot/root-supervisor
ENV CAPABILITY_REGISTRY_PATH=/workspace/contracts/capabilities/registry.v1.json
EXPOSE 3001
CMD ["npm","run","start","-w","@lawpilot/root-supervisor"]
