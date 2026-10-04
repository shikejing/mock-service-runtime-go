// Package mockrt 提供用于测试的依赖替身运行时：
// 在运行时就地接管被测代码发出的 HTTP 调用，由测试预先声明的
// 规则决定返回结果，并保证同一用例重复运行结果完全一致。
package mockrt

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"
)

// Matcher 描述一条接管规则的请求特征。所有非空字段都必须匹配。
type Matcher struct {
	Method  string            // HTTP 方法，空串表示任意
	Path    string            // 精确路径，空串表示任意
	Query   map[string]string // 必须全部出现的查询参数
	Headers map[string]string // 必须全部出现的请求头
	Body    []byte            // 非 nil 时要求请求体逐字节相等
}

// Response 描述规则命中后返回的正常响应。
type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
	// Chunks 非空时按分片/流式返回，忽略 Body。
	Chunks [][]byte
	// TruncateAfter > 0 且 Chunks 非空时，在发送完前 TruncateAfter
	// 个字节后中途结束流（模拟连接中断）。
	TruncateAfter int
}

// Fault 描述规则命中后注入的故障。与 Response 可同时存在：
// 先应用 Delay，再按 Abort/Error/Response 的顺序生效。
type Fault struct {
	Delay time.Duration // 响应前延迟
	Error error         // 非 nil 时以该错误使请求失败
	Abort bool          // 为 true 时模拟连接被重置
}

// Rule 是一条接管规则：Matcher 命中后按 Response/Fault 编排返回。
type Rule struct {
	Name     string // 规则名，用于异常链路定位与断言报告
	Matcher  Matcher
	Response Response
	Fault    Fault
	Times    int // 可消费次数，<=0 表示不限

	used int64 // 已消费次数（原子访问）
}

// tryConsume 尝试消费一次规则，成功返回 true；并发安全。
func (r *Rule) tryConsume() bool {
	for {
		u := atomic.LoadInt64(&r.used)
		if r.Times > 0 && u >= int64(r.Times) {
			return false
		}
		if atomic.CompareAndSwapInt64(&r.used, u, u+1) {
			return true
		}
	}
}

// remaining 返回剩余可消费次数；Times<=0（不限）时返回 0。
func (r *Rule) remaining() int {
	if r.Times <= 0 {
		return 0
	}
	if rem := int64(r.Times) - atomic.LoadInt64(&r.used); rem > 0 {
		return int(rem)
	}
	return 0
}

// MissReason 区分请求未命中任何规则的原因。
type MissReason int

const (
	MissNone      MissReason = iota // 命中（内部使用）
	MissDisabled                    // 接管未启用
	MissNoRules                     // 用例未声明任何规则
	MissMethod                      // 方法不匹配
	MissPath                        // 路径不匹配
	MissQuery                       // 查询参数不匹配
	MissHeader                      // 请求头不匹配
	MissBody                        // 请求体不匹配
	MissExhausted                   // 规则次数已耗尽
)

func (r MissReason) String() string {
	switch r {
	case MissNone:
		return "matched"
	case MissDisabled:
		return "interception disabled"
	case MissNoRules:
		return "no rules declared"
	case MissMethod:
		return "method mismatch"
	case MissPath:
		return "path mismatch"
	case MissQuery:
		return "query mismatch"
	case MissHeader:
		return "header mismatch"
	case MissBody:
		return "body mismatch"
	case MissExhausted:
		return "rule times exhausted"
	default:
		return "unknown"
	}
}

// UnmatchedError 表示请求未被任何规则接管，携带可区分的原因。
type UnmatchedError struct {
	Reason   MissReason
	Closest  string // 最接近的规则名（若有）
	Method   string
	Path     string
	RuleName string // 命中但触发故障/耗尽时的规则名
}

func (e *UnmatchedError) Error() string {
	s := "mockrt: unmatched request " + e.Method + " " + e.Path + ": " + e.Reason.String()
	if e.RuleName != "" {
		s += " (rule " + strconv.Quote(e.RuleName) + ")"
	} else if e.Closest != "" {
		s += " (closest rule " + strconv.Quote(e.Closest) + ")"
	}
	return s
}

// FaultError 包装规则编排的故障，可定位到具体规则。
type FaultError struct {
	RuleName string
	Abort    bool
	Err      error
}

func (e *FaultError) Error() string {
	kind := "error"
	if e.Abort {
		kind = "connection abort"
	}
	s := "mockrt: fault injected by rule " + strconv.Quote(e.RuleName) + ": " + kind
	if e.Err != nil {
		s += ": " + e.Err.Error()
	}
	return s
}
func (e *FaultError) Unwrap() error { return e.Err }

// Call 是一次实际发生的调用记录。
type Call struct {
	Seq      int64 // 用例内单调递增序号
	RuleName string
	Method   string
	Path     string
	Query    map[string]string
	Headers  http.Header
	Body     []byte
}

// errStreamReset 表示流式响应被中途截断（模拟连接重置）。
var errStreamReset = errors.New("mockrt: stream reset by rule")

// streamBody 支持中途结束的分片响应体。
type streamBody struct {
	chunks   [][]byte
	truncate int // >0 时发送满该字节数后以 errStreamReset 结束
	chunk    int // 当前分片下标
	off      int // 当前分片内偏移
	sent     int // 已发送总字节数
}

func (s *streamBody) Read(p []byte) (int, error) {
	if s.truncate > 0 && s.sent >= s.truncate {
		return 0, errStreamReset
	}
	for s.chunk < len(s.chunks) && s.off >= len(s.chunks[s.chunk]) {
		s.chunk++
		s.off = 0
	}
	if s.chunk >= len(s.chunks) {
		return 0, io.EOF
	}
	cur := s.chunks[s.chunk]
	if s.truncate > 0 {
		if remain := s.truncate - s.sent; remain < len(cur)-s.off {
			cur = cur[:s.off+remain]
		}
	}
	n := copy(p, cur[s.off:])
	s.off += n
	s.sent += n
	return n, nil
}
func (s *streamBody) Close() error { return nil }
