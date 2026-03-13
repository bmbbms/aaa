# Copaw K8s Provisioner API (Go)

提供一个 HTTP API，用于根据 `username` 与 `employee_id` 在 Kubernetes 中创建/更新 Copaw 实例资源：

- Deployment（镜像：`agentscope/copaw:latest`）
- Service（`8088`）
- Ingress（同域名 + 按工号 path 路由并 rewrite）
- 每个工号独立 PVC（挂载到 `/app/working`）
- 共享 Secret（挂载到 `/app/working.secret`）

## API

`POST /api/v1/copaw/deploy`

请求体：

```json
{
  "username": "alice",
  "employee_id": "10001"
}
```

返回示例：

```json
{
  "message": "copaw deployed successfully",
  "deployment": "copaw-10001",
  "service": "copaw-10001",
  "ingress": "copaw-10001",
  "workspace_pvc": "copaw-ws-10001",
  "access_url": "https://copaw.example.com/copaw/10001/"
}
```

## 关键环境变量

- `NAMESPACE`：资源创建命名空间，默认 `default`
- `INGRESS_HOST`：统一访问域名，默认 `copaw.example.com`
- `COPAW_IMAGE`：默认 `agentscope/copaw:latest`
- `SHARED_SECRET_NAME`：共享 API key 的 Secret 名称，默认 `copaw-shared-secrets`
- `WORKSPACE_STORAGE_CLASS`：PVC 使用的 StorageClass（可选）
- `WORKSPACE_BASE_PATH`：工作目录挂载点，默认 `/app/working`
- `LISTEN_ADDR`：服务监听地址，默认 `:8080`

## Ingress 路由规则

每个用户生成独立 ingress（host 相同），path 形如：

- `/copaw/{employee_id}(/|$)(.*)`

并使用注解：

- `nginx.ingress.kubernetes.io/use-regex: "true"`
- `nginx.ingress.kubernetes.io/rewrite-target: "/$2"`

实现访问 `https://<host>/copaw/<employee_id>/...` 转发到该用户对应 Service 的 `/...`。
