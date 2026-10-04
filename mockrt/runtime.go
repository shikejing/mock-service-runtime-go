package mockrt

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

// Config 是运行时配置。运行期变更只影响之后开始的用例，
// 进行中的用例持有开始时的快照，不受后续修改影响。
type Config struct {
	Enabled     bool          // 是否启用接管；false 时请求返回 MissDisabled 错误
	Timeout     time.Duration // 单次被接管调用的最大耗时（含编排延迟），<=0 表示不限
	Concurrency int           // 单用例内允许的最大并发接管数，<=0 表示不限
	Seed        int64         // 伪造数据种子
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{Enabled: true, Timeout: 30 * time.Second, Concurrency: 0, Seed: 1}
}

// Runtime 是替身运行时。接管作用域为进程内：被测代码须使用
// Runtime.Transport 作为 http.Client 的 RoundTripper。
type Runtime struct {
	mu  sync.Mutex
	cfg Config
}

// NewRuntime 以给定配置创建运行时。
func NewRuntime(cfg Config) *Runtime { return &Runtime{cfg: cfg} }

// SetConfig 更新配置，仅影响之后 Begin 的用例；进行中的用例不受影响。
func (r *Runtime) SetConfig(cfg Config) {
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
}

// Config 返回当前配置快照。
func (r *Runtime) Config() Config {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cfg
}

// Transport 返回接管用的 http.RoundTripper。
// 请求必须通过 Case.Context 派生的 context 发出，才能归属到具体用例；
// 否则返回 MissDisabled 错误，不会静默放行到真实依赖。
func (r *Runtime) Transport() http.RoundTripper { return transport{rt: r} }

// Begin 开启一个用例，捕获当前配置快照。
func (r *Runtime) Begin() *Case {
	return &Case{rt: r, cfg: r.Config(), faker: NewFaker(r.Config().Seed)}
}

type caseKey struct{}

// Case 是一个测试用例的隔离作用域：独立的规则集、调用记录与配置快照。
// 同一进程内多个 Case 可并发运行，互不可见。
type Case struct {
	rt     *Runtime
	cfg    Config
	faker  *Faker
	mu     sync.Mutex
	rules  []*Rule
	rec    *recorderState
	sem    chan struct{} // 并发度限制，nil 表示不限
	closed bool
}

// Context 返回携带用例标识的 context，被测代码的请求须使用它。
func (c *Case) Context() context.Context {
	return context.WithValue(context.Background(), caseKey{}, c)
}

// Faker 返回该用例的确定性伪造数据生成器（种子来自配置快照）。
func (c *Case) Faker() *Faker { return c.faker }

// AddRule 声明一条接管规则，返回规则指针（Times 消费计数原地维护）。
// 已关闭的用例拒绝新增规则。
func (c *Case) AddRule(rule Rule) *Rule {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	r := rule
	c.rules = append(c.rules, &r)
	return &r
}

// Recorder 返回该用例的调用记录器。
func (c *Case) Recorder() *Recorder {
	return &Recorder{state: c.recState(), rulesFn: func() []*Rule { rules, _ := c.snapshot(); return rules }}
}

// Close 结束用例并干净还原：规则与记录全部失效，
// 之后经该用例 context 发出的请求返回 MissDisabled 错误，
// 不会有残留状态污染后续真实调用。
func (c *Case) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	c.rules = nil
	c.rec = nil
	return nil
}

// recState 惰性创建记录状态。
func (c *Case) recState() *recorderState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.rec == nil {
		c.rec = &recorderState{}
	}
	return c.rec
}

// snapshot 读取当前规则与关闭标志。
func (c *Case) snapshot() ([]*Rule, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	rules := make([]*Rule, len(c.rules))
	copy(rules, c.rules)
	return rules, c.closed
}

// acquire 占用一个并发槽，返回释放函数。
func (c *Case) acquire() func() {
	c.mu.Lock()
	sem := c.sem
	cfg := c.cfg
	c.mu.Unlock()
	if cfg.Concurrency <= 0 {
		return func() {}
	}
	if sem == nil {
		c.mu.Lock()
		if c.sem == nil {
			c.sem = make(chan struct{}, cfg.Concurrency)
		}
		sem = c.sem
		c.mu.Unlock()
	}
	sem <- struct{}{}
	return func() { <-sem }
}

// handle 执行一次接管：匹配、编排、记录。全程使用用例的配置快照。
func (c *Case) handle(req *http.Request) (*http.Response, error) {
	cfg := c.cfg // 快照：运行期配置变更不影响进行中的用例
	if !cfg.Enabled {
		return nil, &UnmatchedError{Reason: MissDisabled, Method: req.Method, Path: req.URL.Path}
	}
	release := c.acquire()
	defer release()

	var deadline <-chan time.Time
	if cfg.Timeout > 0 {
		t := time.NewTimer(cfg.Timeout)
		defer t.Stop()
		deadline = t.C
	}

	var body []byte
	if req.Body != nil {
		var err error
		body, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
	}

	rules, closed := c.snapshot()
	if closed {
		return nil, &UnmatchedError{Reason: MissDisabled, Method: req.Method, Path: req.URL.Path}
	}
	res := resolve(rules, req, body)
	if res.rule == nil {
		e := &UnmatchedError{Reason: res.reason, Method: req.Method, Path: req.URL.Path}
		if res.closest != nil {
			e.Closest = res.closest.Name
		}
		return nil, e
	}
	rule := res.rule
	if !rule.tryConsume() {
		return nil, &UnmatchedError{Reason: MissExhausted, Method: req.Method, Path: req.URL.Path, RuleName: rule.Name}
	}

	// 编排延迟，受超时约束。
	if d := rule.Fault.Delay; d > 0 {
		if deadline == nil {
			time.Sleep(d)
		} else {
			select {
			case <-time.After(d):
			case <-deadline:
				return nil, &FaultError{RuleName: rule.Name, Err: errors.New("mockrt: case timeout during injected delay")}
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
	}
	// 故障：Abort / Error。
	if rule.Fault.Abort {
		return nil, &FaultError{RuleName: rule.Name, Abort: true, Err: errors.New("connection reset")}
	}
	if rule.Fault.Error != nil {
		return nil, &FaultError{RuleName: rule.Name, Err: rule.Fault.Error}
	}

	// 记录调用（命中即记录，含后续故障已在上面返回的情况不记录）。
	c.recState().add(Call{
		RuleName: rule.Name,
		Method:   req.Method,
		Path:     req.URL.Path,
		Query:    flattenQuery(req),
		Headers:  req.Header.Clone(),
		Body:     body,
	})

	resp := &http.Response{
		StatusCode: rule.Response.Status,
		Header:     make(http.Header),
		Body:       io.NopCloser(bytes.NewReader(rule.Response.Body)),
		Request:    req,
	}
	if resp.StatusCode == 0 {
		resp.StatusCode = http.StatusOK
	}
	for k, v := range rule.Response.Headers {
		resp.Header.Set(k, v)
	}
	if len(rule.Response.Chunks) > 0 {
		resp.Body = &streamBody{chunks: rule.Response.Chunks, truncate: rule.Response.TruncateAfter}
		resp.ContentLength = -1
	} else {
		resp.ContentLength = int64(len(rule.Response.Body))
	}
	return resp, nil
}

func flattenQuery(req *http.Request) map[string]string {
	q := req.URL.Query()
	out := make(map[string]string, len(q))
	for k := range q {
		out[k] = q.Get(k)
	}
	return out
}

// transport 实现 http.RoundTripper，把请求分派给 context 中的用例。
type transport struct{ rt *Runtime }

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) {
	c, ok := req.Context().Value(caseKey{}).(*Case)
	if !ok || c == nil {
		return nil, &UnmatchedError{Reason: MissDisabled, Method: req.Method, Path: req.URL.Path}
	}
	return c.handle(req)
}
