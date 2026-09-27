# mimo-cpa-plugin 构建 / 打包
#
# Windows 上如果没有 make，直接看 README 里的 PowerShell 等价命令。

PLUGIN_ID := mimo
VERSION   ?= 0.1.0
GOOS      ?= $(shell go env GOOS)
GOARCH    ?= $(shell go env GOARCH)
# 插件元数据里的仓库地址（官方插件商店 registry 要求 github.com/{owner}/{repo}）；
# CI 仍用 -X main.pluginRepo=https://github.com/<owner>/<repo> 注入当前仓库值，
# 本地构建可用 make build PLUGIN_REPO=... 覆盖
PLUGIN_REPO ?= https://github.com/qizhuxu/xm2api

# 动态库后缀按平台走，与 CPA loader 的约定一致
EXT_linux   := so
EXT_darwin  := dylib
EXT_windows := dll
EXT         := $(EXT_$(GOOS))

DIST := dist
LIB  := $(DIST)/$(PLUGIN_ID).$(EXT)
ZIP  := $(DIST)/$(PLUGIN_ID)_$(VERSION)_$(GOOS)_$(GOARCH).zip

# -buildmode=c-shared 必须有 CGO 和 C 工具链
export CGO_ENABLED = 1

.PHONY: all build test test-live vet fmt clean package list

all: build

## 编译出当前平台的动态库
build:
	@mkdir -p $(DIST)
	go build -buildmode=c-shared -trimpath -ldflags "-s -w -X main.pluginRepo=$(PLUGIN_REPO)" -o $(LIB) .
	@echo "built $(LIB)"

## 单元测试（不需要网络和凭证）
test:
	go test ./...

## 实网测试：真的跑一次 SSO 两阶段换 token
##   用法: make test-live MIMO_AUTH_FILE=/path/to/mimo.json
test-live:
	MIMO_AUTH_FILE=$(MIMO_AUTH_FILE) go test -run Live -v ./...

vet:
	go vet ./...

fmt:
	gofmt -l -w .

clean:
	rm -rf $(DIST)

## 打插件商店要求的 release 资产: <id>_<version>_<goos>_<goarch>.zip + checksums.txt
## zip 根目录必须直接放动态库，不能套子目录。
package: build
	@mkdir -p $(DIST)/pkg
	cp $(LIB) $(DIST)/pkg/
	cd $(DIST)/pkg && zip -q ../$(notdir $(ZIP)) $(PLUGIN_ID).$(EXT)
	cd $(DIST) && sha256sum $(notdir $(ZIP)) > checksums.txt
	@echo "packaged:"
	@ls -l $(ZIP) $(DIST)/checksums.txt

## 看看有哪些目标
list:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
