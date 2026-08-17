# 纯 .NET 示例：编译型语言，需多阶段构建（SDK 编译、runtime 运行）。
# 锁定到完整补丁版本保证构建可复现。dotnet restore / publish 需要访问 nuget（构建期联网）。
FROM mcr.microsoft.com/dotnet/sdk:8.0-bookworm-slim AS builder
WORKDIR /app
COPY hello.csproj ./
RUN dotnet restore
COPY . ./
RUN dotnet publish -c Release -o /out

# 最终运行阶段用 aspnet 运行时（含 Kestrel / Microsoft.AspNetCore.App 框架）；
# 仅用 runtime 镜像会因缺少 ASP.NET Core 框架而无法启动 Kestrel。
FROM mcr.microsoft.com/dotnet/aspnet:8.0-bookworm-slim
WORKDIR /app
COPY --from=builder /out ./
ENV PORT=8080
ENV APP_VERSION=dev
EXPOSE 8080
# .NET runtime 镜像不含预置非 root 用户，显式创建 appuser(uid 1000) 并授权目录后切换
RUN useradd -M -u 1000 appuser && chown -R appuser:appuser /app
USER appuser
ENTRYPOINT ["dotnet", "hello.dll"]
