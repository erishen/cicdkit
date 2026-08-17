<?php
// 零依赖 PHP 示例，由 php:8.3.8-cli-alpine 的 PHP 内置服务器 `php -S` 提供（文档根 /var/www/html）。
// 读取 APP_VERSION 环境变量（由平台 Dockerfile 的 ENV 注入）。
$version = getenv('APP_VERSION') ?: 'dev';
echo "Hello from hello-php (version " . $version . ")\n";
