# 发布说明

## 发布内容

版本标签触发 GitHub Actions，依次执行：

1. 格式、静态检查、带 race detector 的测试、工作流校验、Linux amd64 / arm64 交叉编译。
2. 构建单架构测试镜像，以非 root、只读根文件系统和持久卷启动，验证健康检查。
3. 构建并推送 GHCR 多架构镜像，附带 SBOM 和构建来源证明。
4. 创建 GitHub Release，上传两个架构的 Linux 二进制压缩包和 `checksums.txt`。

普通 `main` 提交和 PR 执行 CI；不会获得镜像发布权限。Action 引用固定到完整提交 SHA，Go 构建镜像固定到版本及 manifest digest。

## 发布版本

确认 `main` 中的版本内容已合并，创建并推送符合格式的标签：

```sh
git tag -a v0.1.0 -m 'Release v0.1.0'
git push origin v0.1.0
```

标签格式为 `vMAJOR.MINOR.PATCH`，也可以是 `v0.2.0-rc.1` 一类预发布。稳定标签 `v0.1.0` 发布 `:0.1.0`、`:0.1` 和 `:latest`；预发布只发布自己的完整版本标签。补丁版本发布后，对应 `MAJOR.MINOR` 和 `latest` 会移动。生产部署建议固定完整版本或镜像 digest。

如果某次发布失败，可以在 GitHub Actions 中重跑失败的工作流。已存在的 GitHub Release 会更新同名制品；已发布的容器标签也可能被重建覆盖，因此已经对外发布的版本应尽量通过新补丁版本修复。

## GHCR 权限

无需自建 PAT 或保存 Docker 密码。镜像发布 job 使用该次运行的 `GITHUB_TOKEN`，权限为 `contents: read`、`packages: write`；只有创建 GitHub Release 的 job 需要 `contents: write`。

GHCR 新建的容器包可能默认私有。维护者首次发布后需在 GitHub package 的设置中将其可见性设为 Public，确认匿名用户可以拉取；仓库公开不代表包自动公开。镜像包含 `org.opencontainers.image.source` 标签，将包关联到来源仓库。如果复用已有包，还需在包设置中允许该仓库的 Actions 写入。

```sh
docker buildx imagetools inspect ghcr.io/myth815/xunlei-api:0.1.0
docker pull ghcr.io/myth815/xunlei-api:0.1.0
```

Linux 架构之外可能看到 `unknown/unknown` manifest，它们用于镜像证明信息，不是额外的运行平台。SBOM 和 provenance 描述构建及依赖，不等同于独立的代码安全审计。

## 更新构建依赖

当前构建使用 Go `1.27.1` 和官方 `golang:1.27.1-alpine3.24`。更新 Go 时同步 Dockerfile、CI、release workflow 和 README；为新的官方 image index 校验 digest。更新 GitHub Action 时，从原仓库校验 release 标签对应的 commit，并保留版本注释。

来源：[Go 发布](https://go.dev/dl/)、[官方 Go 镜像](https://github.com/docker-library/golang)、[GitHub Packages 容器注册表](https://docs.github.com/en/packages/working-with-a-github-packages-registry/working-with-the-container-registry)、[Docker 构建证明](https://docs.docker.com/build/metadata/attestations/)。
