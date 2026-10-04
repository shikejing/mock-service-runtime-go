package mockrt

import "fmt"

// AssertOrder 校验给定规则名按顺序（作为子序列）出现在调用记录中。
func (s *Scope) AssertOrder(names ...string) error {
	calls := s.Calls()
	i := 0
	for _, c := range calls {
		if !c.Matched {
			continue
		}
		if i < len(names) && c.RuleName == names[i] {
			i++
		}
	}
	if i < len(names) {
		return fmt.Errorf("mockrt: call order mismatch: matched %d of %d expected rules in order (next expected %q)",
			i, len(names), names[i])
	}
	return nil
}

// AssertCount 校验指定规则被命中的次数。
func (s *Scope) AssertCount(ruleName string, n int) error {
	got := 0
	for _, c := range s.Calls() {
		if c.Matched && c.RuleName == ruleName {
			got++
		}
	}
	if got != n {
		return fmt.Errorf("mockrt: rule %q called %d times, want %d", ruleName, got, n)
	}
	return nil
}

// Verify 分别报告未被消费的规则与多余调用（未命中任何规则的调用）。
// 全部满足时返回 nil，否则返回 *VerificationError。
func (s *Scope) Verify() error {
	s.mu.Lock()
	verr := &VerificationError{}
	for _, r := range s.rules {
		if r.Times > 0 && r.consumed < r.Times {
			verr.Unconsumed = append(verr.Unconsumed,
				fmt.Sprintf("%s (consumed %d/%d)", r.Name, r.consumed, r.Times))
		}
	}
	for _, c := range s.calls {
		if !c.Matched {
			verr.Extra = append(verr.Extra, c)
		}
	}
	s.mu.Unlock()
	if len(verr.Unconsumed) == 0 && len(verr.Extra) == 0 {
		return nil
	}
	return verr
}
