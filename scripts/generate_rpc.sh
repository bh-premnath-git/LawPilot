#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")/.."
# Pinned protoc 3.19.1 is supplied by grpc-tools 1.13.0.
# Go generators: protobuf v1.36.6, grpc v1.5.1; Python grpcio-tools 1.76.0.
export PATH="${RPC_GO_BIN:-$PWD/work/bin}:$PATH"
python_bin="${RPC_PYTHON:-python3}"
mapfile -t protos < <(rg --files contracts/proto -g '*.proto' | sort)
node_modules/.bin/grpc_tools_node_protoc -Icontracts/proto -Inode_modules/grpc-tools/bin \
 --plugin=protoc-gen-ts_proto=node_modules/.bin/protoc-gen-ts_proto \
 --ts_proto_out=packages/rpc/src/generated \
 --ts_proto_opt=outputServices=grpc-js,esModuleInterop=true,useExactTypes=false,lowerCaseServiceMethods=true,emitDefaultValues=json-methods \
 --go_out=contracts/gen/go --go_opt=paths=source_relative \
 --go-grpc_out=contracts/gen/go --go-grpc_opt=paths=source_relative \
 --descriptor_set_out=contracts/gen/contracts.pb --include_imports "${protos[@]}"
"$python_bin" -m grpc_tools.protoc -Icontracts/proto \
 --python_out=contracts/gen/python --grpc_python_out=contracts/gen/python "${protos[@]}"
