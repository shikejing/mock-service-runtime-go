package mockrt

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"sync"
	"time"
)

// Runtime 管理配置与全部用例作用域。
type Runtime struct {
	cfg    *configHolder
	mu     sync.Mutex
	scopes map[*Scope]struct{}
	real   http.RoundTripper
}

// Option 自定义 Runtime。
type Option func(*Runtime)

// WithConfig 设置初始配置。
func WithConfig(c Config) Option {
	return func(r *Runtime) { r.cfg.set(c) }
}

// WithRealTransport 指定放行到真实依赖时使用的 RoundTripper（录制模式用）。
func WithRealTransport(rt http.RoundTripper) Option {
	return func(r *Runtime) {
		if rt != nil {
			r.real = rt
		}
	}
}

// New 创建运行时。
func New(opts ...Option) *Runtime {
	r := &Runtime{
		cfg:    newConfigHolder(DefaultConfig()),
		scopes: map[*Scope]struct{}{},
		real:   http.DefaultTransport,
	}
	for _, o := range opts {
		o(r)
	}
	return r
}

// UpdateConfig 更新配置，只影响之后开始的用例；进行中的用例
// 已在 Begin 时快照配置，不会停在中间状态。
func (r *Runtime) UpdateConfig(c Config) { r.cfg.set(c) }

// Config 返回当前配置快照。
func (r *Runtime) Config() Config { return r.cfg.get() }

// Begin 开启一个用例作用域，快照当前配置。
func (r *Runtime) Begin(name string) *Scope {
	s := &Scope{
		rt:   r,
		name: name,
		cfg:  r.cfg.get(),
	}
	if s.cfg.MaxConcurrency > 0 {
		s.sem = make(chan struct{}, s.cfg.MaxConcurrency)
	}
	r.mu.Lock()
	r.scopes[s] = struct{}{}
	r.mu.Unlock()
	return s
}

// Transport 返回全局 RoundTripper，按请求上下文（WithScope）解析用例。
// 上下文中没有用例时返回错误，绝不静默放行。
func (r *Runtime) Transport() http.RoundTripper {
	return roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		s, ok := req.Context().Value(ctxKey{}).(*Scope)
		if !ok || s == nil {
			return nil, errors.New("mockrt: no scope in request context")
		}
		return s.roundTrip(req)
	})
}

// Install 将全局 Transport 装到 http.DefaultTransport，返回还原函数。
// 还原后后续真实调用不受残留状态影响。
func (r *Runtime) Install() (restore func()) {
	prev := http.DefaultTransport
	http.DefaultTransport = r.Transport()
	var once sync.Once
	return func() {
		once.Do(func() { http.DefaultTransport = prev })
	}
}

// Close 关闭运行时，清理全部作用域。
func (r *Runtime) Close() error {
	r.mu.Lock()
	scopes := make([]*Scope, 0, len(r.scopes))
	for s := range r.scopes {
		scopes = append(scopes, s)
	}
	r.mu.Unlock()
	for _, s := range scopes {
		s.Close()
	}
	return nil
}

type ctxKey struct{}

// WithScope 把用例绑定到上下文，供全局 Transport 解析。
func WithScope(ctx context.Context, s *Scope) context.Context {
	return context.WithValue(ctx, ctxKey{}, s)
}

// Scope 是一个用例的隔离作用域：规则、记录、伪造种子互不串扰。
type Scope struct {
	rt   *Runtime
	name string
	cfg  Config

	mu       sync.Mutex
	rules    []*Rule
	ruleSeq  uint64
	calls    []CallRecord
	callSeq  uint64
	closed   bool
	sem      chan struct{}
	cassette *Cassette
	mode     CassetteMode
}

// Name 返回用例名。
func (s *Scope) Name() string { return s.name }

// AddRule 注册一条接管规则，返回规则指针（含裁决用的注册序号）。
func (s *Scope) AddRule(r Rule) *Rule {
	s.mu.Lock()
	defer s.mu.Unlock()
	r.seq = s.ruleSeq
	s.ruleSeq++
	s.rules = append(s.rules, &r)
	return s.rules[len(s.rules)-1]
}

// Client 返回一个 HTTP 客户端，其调用被本作用域接管。
func (s *Scope) Client() *http.Client {
	return &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			return s.roundTrip(req)
		}),
	}
}

// Handler 返回 http.Handler，可挂到 httptest.Server 上走真实 HTTP 栈。
func (s *Scope) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		nreq := requestFromHTTP(req, body)
		resp, fault, _, err := s.dispatch(nreq)
		if err != nil {
			http.Error(w, err.Error(), 599)
			return
		}
		if fault != nil {
			to := s.timeoutChan()
			if ferr := applyFaultBefore(fault, to); ferr != nil {
				if IsConnReset(ferr) {
					hijackClose(w)
					return
				}
				http.Error(w, ferr.Error(), 599)
				return
			}
		}
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		status := resp.Status
		if status == 0 {
			status = 200
		}
		w.WriteHeader(status)
		data := resp.Body
		if len(resp.Chunks) > 0 {
			data = bytes.Join(resp.Chunks, nil)
		}
		if fault != nil && fault.AbortAfter >= 0 && fault.AbortAfter < len(data) {
			data = data[:fault.AbortAfter]
			w.Write(data) //nolint:errcheck // 写部分数据后中断
			hijackClose(w)
			return
		}
		w.Write(data) //nolint:errcheck // 测试替身尽力写出
	})
}

func hijackClose(w http.ResponseWriter) {
	if hj, ok := w.(http.Hijacker); ok {
		conn, _, err := hj.Hijack()
		if err == nil {
			conn.Close()
		}
	}
}

// Close 结束用例并干净还原：规则与记录被清除，
// 后续经过本作用域的调用返回明确错误而不是残留行为。
func (s *Scope) Close() error {
	s.mu.Lock()
	s.closed = true
	s.rules = nil
	s.calls = nil
	s.mu.Unlock()
	s.rt.mu.Lock()
	delete(s.rt.scopes, s)
	s.rt.mu.Unlock()
	return nil
}

// Calls 返回本用例的调用记录快照。
func (s *Scope) Calls() []CallRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]CallRecord, len(s.calls))
	copy(out, s.calls)
	return out
}

// timeoutChan 返回在用例超时到期时关闭的通道。
func (s *Scope) timeoutChan() <-chan struct{} {
	ch := make(chan struct{})
	if s.cfg.Timeout > 0 {
		time.AfterFunc(s.cfg.Timeout, func() { close(ch) })
	}
	return ch
}

// roundTrip 是 Transport 路径的入口。
func (s *Scope) roundTrip(req *http.Request) (*http.Response, error) {
	if err := s.acquire(req.Context()); err != nil {
		return nil, err
	}
	defer s.release()

	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body.Close()
	}
	nreq := requestFromHTTP(req, body)

	resp, fault, ruleName, err := s.dispatch(nreq)
	if err != nil {
		return nil, err
	}
	if err := applyFaultBefore(fault, s.timeoutChan()); err != nil {
		return nil, fmt.Errorf("mockrt: rule %q: %w", ruleName, err)
	}
	return s.buildHTTPResponse(req, resp, fault, ruleName)
}

func (s *Scope) acquire(ctx context.Context) error {
	s.mu.Lock()
	closed := s.closed
	enabled := s.cfg.Enabled
	s.mu.Unlock()
	if closed {
		return errors.New("mockrt: scope closed")
	}
	if !enabled {
		return errors.New("mockrt: runtime disabled by config")
	}
	if s.sem == nil {
		return nil
	}
	select {
	case s.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Scope) release() {
	if s.sem != nil {
		<-s.sem
	}
}

// dispatch 匹配规则并记录调用，返回响应、故障与触发规则名。
// 未命中时按磁带模式回放/录制，否则返回 NoMatchError。
func (s *Scope) dispatch(nreq Request) (*Response, *Fault, string, error) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, nil, "", errors.New("mockrt: scope closed")
	}
	rule, nmErr := pickRule(s.rules, nreq)
	if rule != nil {
		rule.consumed++
	}
	s.callSeq++
	rec := CallRecord{Seq: s.callSeq, Request: nreq, Matched: rule != nil}
	if rule != nil {
		rec.RuleName = rule.Name
	}
	s.calls = append(s.calls, rec)
	cassette, mode := s.cassette, s.mode
	seed := s.cfg.Seed
	s.mu.Unlock()

	if rule != nil {
		resp := rule.Response
		if len(rule.FakeBody) > 0 {
			resp.Body = fakeBodyJSON(rule.FakeBody, seed, rule.Name)
		}
		return &resp, rule.Fault, rule.Name, nil
	}

	if cassette != nil {
		// 纯录制模式不查回放，始终放行并追加录制，
		// 保证同 key 的多次调用按真实顺序全部落盘。
		if mode != ModeRecord {
			if entry, ok := cassette.lookup(nreq); ok {
				resp := entry.toResponse()
				return resp, nil, "cassette:" + entry.Key, nil
			}
		}
		if mode == ModeRecord || mode == ModeAuto {
			return s.recordPassthrough(nreq, cassette)
		}
	}
	return nil, nil, "", nmErr
}

// recordPassthrough 放行到真实依赖并录制结果。
func (s *Scope) recordPassthrough(nreq Request, c *Cassette) (*Response, *Fault, string, error) {
	hreq, err := http.NewRequest(nreq.Method, nreq.URL, bytes.NewReader(nreq.Body))
	if err != nil {
		return nil, nil, "", err
	}
	if u := hreq.URL; u != nil {
		q := u.Query()
		for k, vs := range nreq.Query {
			for _, v := range vs {
				q.Add(k, v)
			}
		}
		u.RawQuery = q.Encode()
	}
	for k, vs := range nreq.Header {
		for _, v := range vs {
			hreq.Header.Add(k, v)
		}
	}
	resp, err := s.rt.real.RoundTrip(hreq)
	if err != nil {
		return nil, nil, "", err
	}
	defer resp.Body.Close()
	rbody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, "", err
	}
	entry := cassetteEntry{
		Request: nreq,
		Status:  resp.StatusCode,
		Header:  resp.Header.Clone(),
		Body:    rbody,
	}
	c.record(entry)
	out := entry.toResponse()
	return out, nil, "cassette:record", nil
}

// buildHTTPResponse 把规范化响应转成 *http.Response。
func (s *Scope) buildHTTPResponse(req *http.Request, resp *Response, fault *Fault, ruleName string) (*http.Response, error) {
	status := resp.Status
	if status == 0 {
		status = 200
	}
	var body io.ReadCloser
	if len(resp.Chunks) > 0 {
		readers := make([]io.Reader, len(resp.Chunks))
		for i, c := range resp.Chunks {
			readers[i] = bytes.NewReader(c)
		}
		body = io.NopCloser(io.MultiReader(readers...))
	} else {
		body = io.NopCloser(bytes.NewReader(resp.Body))
	}
	out := &http.Response{
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		StatusCode:    status,
		Header:        http.Header{},
		Body:          body,
		ContentLength: -1,
		Request:       req,
	}
	for k, vs := range resp.Header {
		for _, v := range vs {
			out.Header.Add(k, v)
		}
	}
	if fault != nil {
		out = wrapBodyAbort(out, fault.AbortAfter)
	}
	return out, nil
}

// fakeBodyJSON 按伪造定义生成确定性 JSON 对象。
// 字段按名排序遍历，保证与遍历顺序无关。
func fakeBodyJSON(specs map[string]FakeSpec, seed uint64, ruleName string) []byte {
	keys := make([]string, 0, len(specs))
	for k := range specs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	obj := make(map[string]any, len(specs))
	for _, k := range keys {
		obj[k] = Generate(specs[k], seed, ruleName+"."+k)
	}
	data, err := json.Marshal(obj) // encoding/json 对 map 键排序，输出确定
	if err != nil {
		panic(fmt.Sprintf("mockrt: fake body marshal: %v", err))
	}
	return data
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
