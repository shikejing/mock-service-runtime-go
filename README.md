# mock-service-runtime-go

用于测试的依赖替身运行时（包 `mockrt`）。被测代码在运行时发出的 HTTP 调用被
就地接管，由测试预先声明的规则决定返回结果；同一用例重复运行得到完全一样的结果。

## 快速开始

```go
rt := mockrt.NewRuntime(mockrt.DefaultConfig())
c := rt.Begin()                 // 开启用例，捕获配置快照
defer c.Close()                 // 结束时干净还原

c.AddRule(mockrt.Rule{
    Name:    "get-order",
    Matcher: mockrt.Matcher{Method: "GET", Path: "/orders/1"},
    Response: mockrt.Response{Status: 200, Body: []byte(`{"id":1}`)},
    Times:   1,
})

client := &http.Client{Transport: rt.Transport()}
req, _ := http.NewRequest("GET", "http://order-svc/orders/1", nil)
resp, err := client.Do(req.WithContext(c.Context())) // 请求必须携带用例 context
```

## 匹配语义与裁决顺序

`Matcher` 的每个非空字段都是一个条件，**全部满足**才命中：

| 字段 | 语义 |
|---|---|
| `Method` | HTTP 方法精确匹配，空串表示任意 |
| `Path` | URL 路径精确匹配，空串表示任意 |
| `Query` | 列出的查询参数必须全部出现且值相等 |
| `Headers` | 列出的请求头必须全部出现且值相等（键大小写不敏感） |
| `Body` | 非 nil 时请求体逐字节相等 |

多条规则同时命中时，裁决顺序是**确定的**：

1. **具体程度高者优先**：具体程度 = 已设置条件的数量
   （方法、路径各计 1，每个查询参数/请求头各计 1，请求体计 1）；
2. **具体程度相同，声明顺序早者优先**。

该顺序与请求内容无关，同一规则集对同一请求的裁决结果恒定。

### 未命中原因

未命中的请求**不会**静默返回空结果，也**不会**放行到真实依赖，而是返回
`*mockrt.UnmatchedError`，其 `Reason` 字段区分原因：

`MissDisabled`（接管未启用/用例已关闭）、`MissNoRules`（未声明任何规则）、
`MissMethod`、`MissPath`、`MissQuery`、`MissHeader`、`MissBody`、
`MissExhausted`（规则次数已耗尽，此时 `RuleName` 定位到具体规则）。
错误同时携带 `Closest`（最接近的规则名）便于定位。

## 返回值与故障编排

`Rule.Response` 支持状态码、响应头、响应体；`Chunks` 非空时按分片/流式返回，
`TruncateAfter > 0` 时在发送满该字节数后**中途结束流**（模拟连接中断）。

`Rule.Fault` 的生效顺序固定为：`Delay`（响应前延迟，受用例超时约束）→
`Abort`（连接重置）→ `Error`（返回错误）→ 正常 `Response`。
故障以 `*mockrt.FaultError` 返回，`RuleName` 字段定位到触发它的具体规则，
`Unwrap` 可取出原始错误（支持 `errors.Is`）。

## 确定性保证

`Faker` 生成的所有字段值是 `(种子, 字段路径)` 的**纯函数**：

- 不读取系统时间、环境变量或运行时随机源；
- 内部使用 splitmix64/FNV-1a 等与平台无关的算法；
- 遍历 map 时按键排序，不受 Go 随机遍历顺序影响。

因此同一份伪造定义配合同一个种子，在任意次数、任意进程、任意遍历顺序下
生成的字段值**逐字节一致**。支持的占位符见 `Faker.Fill` 文档
（`{{string:N}}`、`{{int:MIN:MAX}}`、`{{name}}`、`{{email}}`、`{{uuid}}`、`{{bytes:N}}`）。

## 调用记录与断言

每个用例有独立的 `Recorder`（`c.Recorder()`），用例之间互不可见：

- `Calls()`：按发生顺序（用例内单调递增序号）返回调用记录；
- `AssertOrder(names...)`：按顺序校验（子序列匹配）；
- `AssertCount(name, n)`：按次数校验；
- `AssertExhausted()`：校验所有规则被完全消费且无多余调用，
  **未消费的规则与多余调用在错误中分别报告**。

## 并行隔离

`Runtime` 可全局唯一，每个 `Case` 是独立作用域：独立的规则集、调用记录与
配置快照。同一进程内多个用例并发运行、声明同名或同特征的规则时互不串扰。
请求通过 `Case.Context()` 归属到用例；不携带用例 context 的请求直接返回
`MissDisabled` 错误，不会落到真实依赖。`Case.Close()` 后规则与记录全部失效，
后续真实调用不受残留状态污染。

## 录制与回放

```go
// 录制：包装真实 Transport，每次真实交互追加到 cassette
cassette := &mockrt.Cassette{}
client := &http.Client{Transport: mockrt.RecordTransport(nil, cassette)}
// ... 真实调用 ...
mockrt.Save("cassette.json", cassette)

// 回放：无外部依赖环境下按录制内容接管
loaded, _ := mockrt.Load("cassette.json")
for _, r := range mockrt.ReplayRules(loaded) {
    c.AddRule(r) // 每条规则 Times=1，按录制顺序各消费一次
}
```

兼容性约定：cassette 格式只允许新增可选字段；新条目只允许**追加**到
`Entries` 末尾，不得修改既有条目，保证已提交用例的回放稳定；
加载时拒绝更高版本的文件。

## 配置项

`Config` 字段：

| 字段 | 含义 | 默认 |
|---|---|---|
| `Enabled` | 是否启用接管；false 时请求返回 `MissDisabled` | true |
| `Timeout` | 单次被接管调用的最大耗时（含编排延迟），<=0 不限 | 30s |
| `Concurrency` | 单用例内最大并发接管数，<=0 不限 | 0（不限） |
| `Seed` | 伪造数据种子 | 1 |

`Runtime.SetConfig` 在运行期变更配置**只影响之后 `Begin` 的用例**；
进行中的用例持有开始时的快照，不会停在中间状态。

## 测试

```sh
go test ./...          # 单元测试
go test ./... -race    # 含竞态检测
```

覆盖：匹配裁决（具体程度/声明顺序/未命中原因）、确定性（多种子/遍历顺序）、
并行隔离（同名规则不串扰、关闭后干净还原）、配置快照、录制回放、
故障注入（延迟/错误/连接中断/流式截断/超时）。
