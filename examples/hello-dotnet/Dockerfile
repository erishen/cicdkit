# syntax=docker/dockerfile:1
# 纯 .NET 示例：编译型语言，需多阶段构建（SDK 编译、runtime 运行）。
# 锁定到完整补丁版本保证构建可复现。dotnet restore / publish 需要访问 nuget（构建期联网）。
#
# BASE_REGISTRY 指向基础镜像所在 registry，用于国内网络规避 mcr.microsoft.com 直连慢的问题：
#   - 默认 mcr.microsoft.com（公网可移植）
#   - 国内加速可用 mcr.m.daocloud.io（由 cicdkit project.json 的 build_args.BASE_REGISTRY 注入）
# 仅替换域名前缀即可，标签路径保持一致。
ARG BASE_REGISTRY=mcr.microsoft.com
FROM ${BASE_REGISTRY}/dotnet/sdk:8.0-bookworm-slim AS builder
WORKDIR /app
COPY hello.csproj nuget.config ./
# NuGet 包缓存挂载（BuildKit）：跨构建持久化到构建缓存，
# 即使 csproj 变更导致 restore 层失效，也无需重新下载全部包。
RUN --mount=type=cache,target=/root/.nuget/packages,id=dotnet-nuget dotnet restore
COPY . ./
RUN --mount=type=cache,target=/root/.nuget/packages,id=dotnet-nuget dotnet publish -c Release -o /out

# 最终运行阶段用 aspnet 运行时（含 Kestrel / Microsoft.AspNetCore.App 框架）；
# 仅用 runtime 镜像会因缺少 ASP.NET Core 框架而无法启动 Kestrel。
FROM ${BASE_REGISTRY}/dotnet/aspnet:8.0-bookworm-slim
WORKDIR /app
COPY --from=builder /out ./
ENV PORT=8080
ENV APP_VERSION=dev
EXPOSE 8080
# .NET runtime 镜像不含预置非 root 用户，显式创建 appuser(uid 1000) 并授权目录后切换
RUN useradd -M -u 1000 appuser && chown -R appuser:appuser /app
USER appuser
ENTRYPOINT ["dotnet", "hello.dll"]
