# mock-service-runtime-go

用于测试的依赖替身运行时（包 `mockrt`）。被测代码在运行时发出的 HTTP 调用被就地接管，
由测试预先声明的规则决定返回结果或注入故障；同一用例重复运行得到完全一致的结果。

## 快速开始

```go
rt := mockrt.New()
defer rt.Close()

s := rt.Begin("my-case")          // 每个用例一个隔离作用域
defer s.Close()

s.AddRule(mockrt.Rule{
    Name:  "get-user",
    Match: mockrt.Matcher{Method: "GET", Path: "/user", Query: map[string]string{"id": "1"}},
    Response: mockrt.Response{Status: 200, Body: []byte(`{"name":"ada"}`)},
})

client := s.Client()              // 该客户端的调用被本用例接管
resp, _ := client.Get("http://dep/user?id=1")
```

也可以用 `rt.Install()` 替换 `http.DefaultTransport`，并通过
`mockrt.WithScope(ctx, s)` 把用例绑定到请求上下文；上下文中没有用例时
请求直接报错，绝不静默放行到真实依赖。`Install` 返回的 `restore` 函数负责干净还原。

## 匹配语义与裁决顺序

`Matcher` 的非空字段之间是 AND 关系：

| 字段 | 语义 |
|---|---|
| `Method` | 方法相等（大小写不敏感），空表示任意 |
| `Path` / `PathPrefix` | 精确匹配；`PathPrefix=true` 时按前缀 |
| `Query` | 子集匹配：每个键存在且至少一个值相等 |
| `Header` | 子集匹配，键按 HTTP 规范化处理 |
| `Body` | 请求体逐字节相等 |
| `BodyContains` | 请求体包含子串 |

多条规则同时命中时，按以下确定顺序裁决：

1. **`Priority` 高者优先**；
2. 并列时**具体程度高者优先**（声明的约束越多越具体，精确路径比前缀更具体）；
3. 再并列时**注册早者优先**。

声明了 `Times > 0` 的规则被消费完次数后退出匹配。请求未命中任何规则时返回
`*NoMatchError`，其中逐条列出每条规则未命中的具体原因（如
`query: q want 1, got 2`），不会静默返回空结果，也不会放行到真实依赖。

## 返回值与故障编排

`Response` 支持状态码、响应头、响应体；`Chunks` 非空时按分片流式返回。
`Fault` 可组合：

- `Delay`：延迟生效，超过用例 `Timeout` 报超时错误；
- `Err`：调用直接以该错误失败；
- `Reset`：模拟连接中断（`mockrt.IsConnReset(err)` 可识别）；
- `AbortAfter >= 0`：响应流在写出指定字节后中断（分片/流式响应可中途结束）。

Transport 路径返回的错误均包装了触发规则的名称（`mockrt: rule "boom": ...`），
异常链路可定位到具体规则。

## 确定性伪造数据

`Rule.FakeBody` 按 `FakeSpec` 生成 JSON 响应体；也可独立使用
`mockrt.NewFaker(seed)` / `mockrt.Generate(spec, seed, key)`。

保证：同一份伪造定义配合同一个种子，在**任意次数、任意进程、任意遍历顺序**下
生成的字段值逐字节一致。实现只依赖 splitmix64 纯整数运算（种子与字段 key 派生，
拒绝采样避免取模偏差），不读取系统时间、环境变量或运行时随机源；
`time` 类型字段基于固定基准时刻（2000-01-01 UTC）加确定偏移。

支持的 `Kind`：`int`、`uint`、`string`（可指定 `Charset`）、`bool`、`uuid`、
`time`（RFC3339）、`choice`、`bytes`。

## 调用记录与断言

- `s.Calls()`：本用例的调用记录（含序号、命中规则名、请求）；
- `s.AssertOrder("a", "b")`：按顺序（子序列）校验；
- `s.AssertCount("a", 3)`：按次数校验；
- `s.Verify()`：返回 `*VerificationError`，**分别**报告
  `Unconsumed`（声明了 `Times` 但未消费完的规则）与
  `Extra`（未命中任何规则的多余调用）。

记录按作用域隔离，各用例之间互不可见。

## 并行隔离

每个 `Scope` 拥有独立的规则表、调用记录与种子快照。同一进程内多个用例
（`t.Parallel` 或 goroutine）声明同名/同特征规则不会串扰；测试套件含
16 并发用例的 race 检测。`s.Close()` 后规则与记录被清除，经过该作用域的
后续调用返回明确错误而非残留行为；`rt.Install()` 的还原函数恢复原始
`http.DefaultTransport`。

## 录制与回放

```go
c, _ := mockrt.LoadCassette("fixtures/login.cassette.json")
s.UseCassette(c, mockrt.ModeRecord) // 放行到真实依赖并录制
// ModeReplay：只回放，未命中报错；ModeAuto：命中回放，未命中放行并录制
c.Save()
```

- 录制键 = 方法 + 路径 + 排序后的查询参数 + 请求体 的摘要（不含易变请求头）；
- 同一键的多次调用按录制顺序逐条消费，消费完后再次请求报错而非循环；
- `Save` 采用**追加合并**：既有条目永不改写，新增/补充录制不会破坏
  已提交用例的回放结果。

## 配置项

```go
rt.UpdateConfig(mockrt.Config{
    Enabled:        true,            // 关闭时所有接管调用报错
    Timeout:        5 * time.Second, // 单次调用最大耗时（含故障延迟）
    MaxConcurrency: 0,               // 每用例并发处理上限，<=0 不限
    Seed:           1,               // 伪造数据种子
})
```

配置以原子快照保存：`Begin` 时快照到用例，运行期的 `UpdateConfig`
**只影响之后开始的用例**，进行中的用例按旧配置走完，不会停在中间状态。

## 测试

```sh
go test ./... -race
```

覆盖：匹配裁决（优先级/具体度/注册序/次数耗尽/未命中原因）、确定性
（跨遍历顺序、跨进程、种子敏感性）、并行隔离（16 并发用例 + race）、
配置热更新、录制回放（含追加稳定性与顺序消费）、故障注入
（错误/连接重置/延迟超时/流式中断）与断言报告。
