# raincut — 暴雨污染最小关管成本

暴雨把污染物冲进了供水管网：污染从若干**供水入口（sources）**沿有向管道流向若干**取水口（sinks）**。每根管道 `i` 有一个关管成本 `cost_i`。求：**关闭哪些管道，才能阻断所有入口到所有取水口的水流，且总成本最小**。

本服务将该问题建模为有向图上的**多源多汇最小割**，通过自实现的最高标号推进-重标号（push-relabel）最大流算法求解（最大流 = 最小割，未使用任何外部求解器，也不枚举割集），以 HTTP API 提供计算结果。

## 快速开始

```bash
# 启动 API（宿主机端口由 API_PORT 控制，默认 8080）
docker compose up --build api
API_PORT=9000 docker compose up --build api

# 运行一次性验收服务：先跑 go test ./...，再对真实 API 做黑盒验收
docker compose up --build --exit-code-from verify
```

`verify` 服务全部通过时退出码为 0，否则非 0。

本地开发（需要 Go 1.23+）：

```bash
go test ./...                 # 单元测试
go run ./cmd/server           # 监听 :8080（PORT 环境变量可改；SHUTDOWN_TIMEOUT 可改排空窗口）
go run ./cmd/verify           # 对 API_URL（默认 http://localhost:8080）做验收
```

## API

### `POST /minimum-shutdown-cost`

请求体（仅普通 JSON 基础类型；节点编号为 `0` 至 `n-1`）：

```json
{
  "n": 3,
  "edges": [
    {"from": 0, "to": 1, "cost": 3},
    {"from": 0, "to": 1, "cost": 4},
    {"from": 1, "to": 2, "cost": 10}
  ],
  "sources": [0],
  "sinks": [2]
}
```

```bash
curl -s -X POST http://localhost:8080/minimum-shutdown-cost \
  -H 'Content-Type: application/json' \
  -d '{"n":3,"edges":[{"from":0,"to":1,"cost":3},{"from":0,"to":1,"cost":4},{"from":1,"to":2,"cost":10}],"sources":[0],"sinks":[2]}'
```

成功响应 `200 OK`（64 位整数成本）：

```json
{"minimum_shutdown_cost": 7}
```

上例中两条 `0→1` 平行边须各自计费（3+4=7），优于切断 `1→2`（10）。

### 健康检查

`GET /healthz` → `200 {"status":"ok"}`

## 优雅停机

收到 `SIGTERM` 或 `SIGINT` 后，服务会立即关闭监听器（拒绝新请求），随后调用 HTTP Server 的 `Shutdown` 等待已有请求完成并写完响应。排空窗口由 `SHUTDOWN_TIMEOUT`（Go duration，默认 `15s`）控制：

- 在窗口内完成排空：进程以 `0` 退出；
- 超过窗口仍有在途请求：强制关闭剩余连接，进程以非 `0` 状态退出，避免把未完成的停机误报为成功。

Compose 的 `stop_grace_period` 为 `20s`，略大于应用排空超时，确保先由应用报告排空失败，而不是被编排平台无条件 `SIGKILL`。镜像内置 `/healthz` 健康检查，供容器编排系统在滚动更新时摘除正在停机的实例。

## 输入约束与错误格式

| 字段 | 约束 |
| --- | --- |
| `n` | 整数，`2 ≤ n ≤ 20000` |
| `edges` | 至多 `100000` 条；`from`/`to` 必须是 `[0, n-1]` 内的节点 |
| `cost` | 整数，`1 ≤ cost ≤ 10^9` |
| `sources` / `sinks` | 非空节点数组，两组互斥（无公共节点） |

语义说明：图为**有向图**；**平行边各自计费**；**自环不影响结果**（自环不可能跨越任何割）；原本不存在 source 到 sink 的通路时，答案为 `0`。

任何非法输入（越界、非法节点引用、空端点组、端点重叠、类型错误、JSON 格式错误等）都会在进入求解器之前被拒绝，统一返回 **422** 与稳定错误结构：

```json
{"error": {"code": "invalid_graph", "message": "edges[0].to must be a node id in [0, 2], got 3"}}
```

`code` 取值：`invalid_json`（请求体不是合法 JSON 文档）或 `invalid_graph`（图数据违反约束）。所有非法输入的响应体都保持 `{"error": {"code", "message"}}` 这一固定结构。

## 算法

1. 加入超级源 `S` 与超级汇 `T`：`S` 向每个 source 连边，每个 sink 向 `T` 连边，容量均为「所有真实管道成本之和 + 1」。
2. 由于该容量超过任何真实割的代价，最小割绝不会切断超级边，因此 `S→T` 最小割的值恰好等于阻断所有 source→sink 通路的最小关管成本。
3. 用最高标号 push-relabel（带 gap 启发式）求最大流，即最小割值；容量与结果全程使用 64 位整数。

规模上限（n=20000、边数=100000）下，单次求解实测约 0.1 秒量级，远低于 10 秒请求超时。

## 项目结构

```
cmd/server/       API 服务入口（PORT 环境变量，默认 8080）
cmd/verify/       一次性验收客户端（API_URL 指向被测服务）
internal/api/     HTTP 层：请求校验（422 稳定错误结构）+ 建图求解
internal/maxflow/ 自实现最大流/最小割（push-relabel，64 位容量）
Dockerfile        多阶段：runtime（API）与 verify（验收）
docker-compose.yml  api 服务（API_PORT 控制宿主端口）+ verify 一次性服务
```

## 验收内容（verify 服务）

- `go test ./...`：最大流正确性（含与独立 Edmonds-Karp 实现的对拍）、API 校验与精确结果。
- 真实 HTTP 调用：平行边、自环、多源多汇、无通路为 0、64 位成本等精确代价。
- 非法输入：全部返回 422 且错误结构一致，不进入求解。
- 大图：20000 节点 / 100000 边、答案构造可知，在 10 秒请求超时内核对精确值。
