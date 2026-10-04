// Package mockrt 提供用于测试的依赖替身运行时：
// 按规则接管被测代码发出的 HTTP 调用，返回预声明的响应或故障，
// 并保证同种子下伪造数据逐字节确定。
package mockrt

import (
	"net/http"
	"time"
)

// Request 是被接管调用的规范化表示，用于匹配与记录。
type Request struct {
	Method string
	URL    string // 完整 URL（含 scheme 与 host），录制放行时使用
	Path   string
	Query  map[string][]string
	Header map[string][]string
	Body   []byte
}

// Response 是规则声明的返回内容。
// Chunks 非空时按分片流式发送，忽略 Body。
type Response struct {
	Status int
	Header map[string][]string
	Body   []byte
	Chunks [][]byte
}

// Fault 描述一次故障注入。字段可组合：
// Delay 先生效；Err/Reset 直接使调用失败；
// AbortAfter >= 0 时响应流在写出指定字节后中断。
type Fault struct {
	Delay      time.Duration
	Err        error
	Reset      bool
	AbortAfter int // 负数表示不中断
}

// Matcher 描述请求特征。所有非空字段必须同时满足（AND）。
type Matcher struct {
	Method       string // 为空匹配任意方法
	Path         string // 精确匹配；PathPrefix 为真时按前缀
	PathPrefix   bool
	Query        map[string]string // 子集匹配
	Header       map[string]string // 子集匹配
	Body         []byte            // 精确匹配整个请求体
	BodyContains string            // 请求体包含子串
}

// Rule 是一条接管规则。
type Rule struct {
	Name     string
	Priority int // 裁决顺序见 README：Priority 高者优先
	Match    Matcher
	Response Response
	Fault    *Fault
	Times    int // 0 表示不限次数；>0 表示可被消费的次数
	// FakeBody 非空时按确定性伪造定义生成 JSON 响应体，覆盖 Response.Body。
	FakeBody map[string]FakeSpec

	seq      uint64 // 注册序号，裁决最终按注册先后
	consumed int    // 已消费次数
}

// CallRecord 记录一次实际发生的调用。
type CallRecord struct {
	Seq      uint64
	RuleName string // 未命中时为空
	Matched  bool
	Request  Request
}

// FakeSpec 描述一个确定性伪造字段。
type FakeSpec struct {
	Kind    string   // int|uint|string|bool|uuid|time|choice|bytes
	Min     int64    // int/uint 下界（含）
	Max     int64    // int/uint 上界（含）
	Len     int      // string/bytes 长度
	Charset string   // string 字符集，为空用默认字母数字
	Choices []string // choice 候选
}

// Config 是运行期配置。更新只影响之后开始的用例。
type Config struct {
	Enabled        bool          // 关闭时 Transport 直接返回未启用错误
	Timeout        time.Duration // 单次被接管调用的最大耗时（含故障延迟）
	MaxConcurrency int           // 每个用例允许并发处理的调用数，<=0 表示不限
	Seed           uint64        // 伪造数据种子
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{
		Enabled:        true,
		Timeout:        5 * time.Second,
		MaxConcurrency: 0,
		Seed:           1,
	}
}

// NoMatchError 表示请求未命中任何规则，包含每条规则的未命中原因。
type NoMatchError struct {
	Request Request
	Reasons []RuleMismatch
}

func (e *NoMatchError) Error() string {
	return "mockrt: no rule matched " + e.Request.Method + " " + e.Request.Path
}

// RuleMismatch 记录一条规则未命中的具体原因。
type RuleMismatch struct {
	RuleName string
	Reasons  []string
}

// VerificationError 分别报告未被消费的规则与多余调用。
type VerificationError struct {
	Unconsumed []string     // 声明了次数但未消费完的规则名
	Extra      []CallRecord // 未命中任何规则的调用
}

func (e *VerificationError) Error() string {
	return "mockrt: verification failed"
}

// httpRequest 工具：从 *http.Request 构造规范化 Request（在 runtime.go 中使用）。
func requestFromHTTP(req *http.Request, body []byte) Request {
	r := Request{
		Method: req.Method,
		URL:    req.URL.String(),
		Path:   req.URL.Path,
		Query:  map[string][]string{},
		Header: map[string][]string{},
		Body:   body,
	}
	for k, vs := range req.URL.Query() {
		vv := make([]string, len(vs))
		copy(vv, vs)
		r.Query[k] = vv
	}
	for k, vs := range req.Header {
		vv := make([]string, len(vs))
		copy(vv, vs)
		r.Header[http.CanonicalHeaderKey(k)] = vv
	}
	return r
}
