#!/usr/bin/env bash
set -euo pipefail

SCRIPT_ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
CODEGEN_PKG=${CODEGEN_PKG:-$(go env GOPATH)/pkg/mod/k8s.io/code-generator@v0.35.0}

# Reason: kube_codegen.sh 是函数库脚本，不是直接带 all 参数执行的命令。
source "${CODEGEN_PKG}/kube_codegen.sh"

# Reason: 先生成 deepcopy 等 helpers，避免后续 typed client 编译/引用缺失。
kube::codegen::gen_helpers \
  --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
  "${SCRIPT_ROOT}/pkg/apis"

# Reason: 生成 clientset/listers/informers，--with-watch 会启用 watch 相关产物。
kube::codegen::gen_client \
  --with-watch \
  --output-dir "${SCRIPT_ROOT}/pkg/client" \
  --output-pkg "k8s-demo/exercise4/pkg/client" \
  --boilerplate "${SCRIPT_ROOT}/hack/boilerplate.go.txt" \
  "${SCRIPT_ROOT}/pkg/apis"
