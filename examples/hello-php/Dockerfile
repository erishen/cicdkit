# 纯 PHP 示例：以 php:8.3.8-cli-alpine（alpine 瘦身镜像）提供，用 PHP 内置服务器
# `php -S 0.0.0.0:8080 -t /var/www/html` 直接服务 index.php。
# 相比 php:*-apache 完整版，镜像小一个数量级（~80MB vs ~400MB+），且单容器自包含。
# 锁定到完整补丁版本保证构建可复现。
FROM php:8.3.8-cli-alpine

# 非 root 运行（与 hello-ruby / hello-python 约定一致）
RUN adduser -D -u 1000 appuser

WORKDIR /var/www/html
COPY index.php /var/www/html/index.php
RUN chown -R appuser:appuser /var/www/html

ENV APP_VERSION=dev
ENV PORT=8080
EXPOSE 8080

USER appuser
# 内置开发服务器单进程即可满足 demo；-t 指定文档根，请求 / 自动回退 index.php
CMD ["php", "-S", "0.0.0.0:8080", "-t", "/var/www/html"]
