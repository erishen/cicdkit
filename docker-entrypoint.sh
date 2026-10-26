#!/bin/sh
# cicdkit 容器入口：改写 kubeconfig + 把挂载的 .env 导出为真实环境变量后启动 server。
# 注意：.env 故意挂到 /app/secrets/.env（非 Go LoadDotEnv 的搜索路径 "."/exeDir/".."/
# -config 目录），避免其把 API_TOKEN 冲掉。这里显式 export 后，Go 进程的
# processKeys 快照里就有 API_TOKEN，applyEnv 必然拿到显式令牌，非回环绑定即可启动。

# 1) 宿主 kubeconfig 的 server 是 https://127.0.0.1:26443（宿主机回环，容器内不可达），
#    改写为容器内可达的 host.docker.internal（OrbStack 已通过 extra_hosts 映射）。
if [ -f /mnt/host-kubeconfig/config ]; then
  mkdir -p /root/.kube
  sed 's#https://127.0.0.1:26443#https://host.docker.internal:26443#g' \
    /mnt/host-kubeconfig/config > /root/.kube/config
  chmod 600 /root/.kube/config
fi
# 始终以改写后的路径为准（覆盖 .env 里可能带出的其他 KUBECONFIG 值）。
export KUBECONFIG=/root/.kube/config

# 2) 把挂载的 .env 逐行解析并 export 为真实环境变量（与 Go LoadDotEnv 同语义：
#    KEY=VALUE、# 注释、空行、可选引号；不做变量展开以避免执行副作用）。
if [ -f /app/secrets/.env ]; then
  while IFS= read -r line || [ -n "$line" ]; do
    line=$(printf '%s' "$line" | sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//')
    [ -z "$line" ] && continue
    case "$line" in \#*) continue ;; esac
    case "$line" in
      *=*)
        key=${line%%=*}
        val=${line#*=}
        case "$val" in
          \"*\") val=${val#\"}; val=${val%\"} ;;
          \'*\') val=${val#\'}; val=${val%\'} ;;
        esac
        export "$key=$val"
        ;;
    esac
  done < /app/secrets/.env
fi

exec /app/server "$@"
