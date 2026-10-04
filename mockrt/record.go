package mockrt

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// recorderState 是记录器的共享状态（每个 Case 一份，用例间互不可见）。
type recorderState struct {
	mu    sync.Mutex
	seq   int64
	calls []Call
}

func (s *recorderState) add(c Call) {
	s.mu.Lock()
	s.seq++
	c.Seq = s.seq
	s.calls = append(s.calls, c)
	s.mu.Unlock()
}

func (s *recorderState) snapshot() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Call, len(s.calls))
	copy(out, s.calls)
	return out
}

// Recorder 记录用例内实际发生的调用，并支持断言。
// 每个 Case 拥有独立 Recorder，用例之间互不可见。
type Recorder struct {
	state   *recorderState
	rulesFn func() []*Rule // 返回所属用例的当前规则集
}

// Calls 返回按发生顺序排列的调用记录快照。
func (r *Recorder) Calls() []Call { return r.state.snapshot() }

// AssertOrder 校验实际调用按给定规则名顺序发生（子序列匹配，
// 允许中间夹杂其他调用；要求每个给定规则名至少出现一次且相对顺序一致）。
func (r *Recorder) AssertOrder(ruleNames ...string) error {
	calls := r.state.snapshot()
	pos := 0
	for _, want := range ruleNames {
		found := false
		for pos < len(calls) {
			if calls[pos].RuleName == want {
				found = true
				pos++
				break
			}
			pos++
		}
		if !found {
			return fmt.Errorf("mockrt: assert order failed: rule %q not called after position %d; actual order: %s",
				want, pos, actualOrder(calls))
		}
	}
	return nil
}

// AssertCount 校验指定规则的调用次数。
func (r *Recorder) AssertCount(ruleName string, want int) error {
	n := 0
	for _, c := range r.state.snapshot() {
		if c.RuleName == ruleName {
			n++
		}
	}
	if n != want {
		return fmt.Errorf("mockrt: assert count failed: rule %q called %d times, want %d", ruleName, n, want)
	}
	return nil
}

// AssertExhausted 校验所有规则都被完全消费且没有多余调用。
// 未消费的规则与多余调用（无规则对应的调用）分别报告。
func (r *Recorder) AssertExhausted() error {
	rules := r.rulesFn()
	var unused []string
	used := map[string]int{}
	for _, c := range r.state.snapshot() {
		used[c.RuleName]++
	}
	for _, rule := range rules {
		if rule == nil {
			continue
		}
		if remaining := rule.remaining(); remaining > 0 {
			unused = append(unused, fmt.Sprintf("%s(剩余%d次)", rule.Name, remaining))
		}
	}
	var extra []string
	known := map[string]bool{}
	for _, rule := range rules {
		if rule != nil {
			known[rule.Name] = true
		}
	}
	for _, c := range r.state.snapshot() {
		if !known[c.RuleName] {
			extra = append(extra, fmt.Sprintf("%s %s", c.Method, c.Path))
		}
	}
	if len(unused) == 0 && len(extra) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString("mockrt: assert exhausted failed:")
	if len(unused) > 0 {
		sort.Strings(unused)
		b.WriteString(" 未消费的规则: " + strings.Join(unused, ", ") + ";")
	}
	if len(extra) > 0 {
		b.WriteString(" 多余调用: " + strings.Join(extra, ", ") + ";")
	}
	return fmt.Errorf("%s", b.String())
}

func actualOrder(calls []Call) string {
	names := make([]string, len(calls))
	for i, c := range calls {
		names[i] = c.RuleName
	}
	return "[" + strings.Join(names, " ") + "]"
}
