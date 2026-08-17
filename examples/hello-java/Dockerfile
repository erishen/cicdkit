# Java (Maven)：多阶段构建 jar → 运行（按需调整 jar 名与 ENTRYPOINT）
# 瘦身：构建/运行时均用 Alpine 变体，最终镜像约 120–170MB（原 Debian JRE 约 300MB，瘦 50%+）。
# 本项目零外部依赖（仅用 JDK 自带 com.sun.net.httpserver），musl libc 无影响。
FROM maven:3.9-eclipse-temurin-21-alpine AS build
WORKDIR /src
COPY pom.xml ./
RUN mvn -q dependency:go-offline || true
COPY src ./src
RUN mvn -q -DskipTests package
FROM eclipse-temurin:21-jre-alpine
WORKDIR /app
COPY --from=build /src/target/*.jar /app/app.jar
ENV PORT=8080
EXPOSE 8080
# MaxRAMPercentage 让 JVM 按容器内存上限（如 128Mi） sizing 堆，避免默认按宿主机内存分配导致被 OOMKill
ENTRYPOINT ["java", "-XX:MaxRAMPercentage=75.0", "-XX:InitialRAMPercentage=50.0", "-jar", "/app/app.jar"]
