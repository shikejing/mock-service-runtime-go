package mockrt

import (
	"bytes"
	"net/http"
	"sort"
	"strings"
)

// matchRequest 判断请求是否满足匹配器，返回未满足项的原因列表。
// 返回空切片表示命中。所有非空字段为 AND 关系。
func matchRequest(m Matcher, req Request) []string {
	var reasons []string
	if m.Method != "" && !strings.EqualFold(m.Method, req.Method) {
		reasons = append(reasons, "method: want "+m.Method+", got "+req.Method)
	}
	if m.Path != "" {
		if m.PathPrefix {
			if !strings.HasPrefix(req.Path, m.Path) {
				reasons = append(reasons, "path: want prefix "+m.Path+", got "+req.Path)
			}
		} else if req.Path != m.Path {
			reasons = append(reasons, "path: want "+m.Path+", got "+req.Path)
		}
	}
	// 查询参数子集匹配：键存在且至少一个值相等。
	keys := make([]string, 0, len(m.Query))
	for k := range m.Query {
		keys = append(keys, k)
	}
	sort.Strings(keys) // 保证原因顺序确定
	for _, k := range keys {
		want := m.Query[k]
		got, ok := req.Query[k]
		if !ok {
			reasons = append(reasons, "query: missing key "+k)
			continue
		}
		if !containsString(got, want) {
			reasons = append(reasons, "query: "+k+" want "+want+", got "+strings.Join(got, ","))
		}
	}
	hkeys := make([]string, 0, len(m.Header))
	for k := range m.Header {
		hkeys = append(hkeys, k)
	}
	sort.Strings(hkeys)
	for _, k := range hkeys {
		want := m.Header[k]
		got, ok := req.Header[canonicalKey(k)]
		if !ok {
			reasons = append(reasons, "header: missing key "+k)
			continue
		}
		if !containsString(got, want) {
			reasons = append(reasons, "header: "+k+" want "+want+", got "+strings.Join(got, ","))
		}
	}
	if m.Body != nil && !bytes.Equal(m.Body, req.Body) {
		reasons = append(reasons, "body: exact mismatch")
	}
	if m.BodyContains != "" && !strings.Contains(string(req.Body), m.BodyContains) {
		reasons = append(reasons, "body: missing substring "+m.BodyContains)
	}
	return reasons
}

func containsString(vs []string, want string) bool {
	for _, v := range vs {
		if v == want {
			return true
		}
	}
	return false
}

func canonicalKey(k string) string {
	return http.CanonicalHeaderKey(k)
}

// specificity 计算匹配器的具体程度：声明的约束越多越具体。
func specificity(m Matcher) int {
	n := 0
	if m.Method != "" {
		n++
	}
	if m.Path != "" {
		n++
		if !m.PathPrefix {
			n++ // 精确路径比前缀更具体
		}
	}
	n += len(m.Query)
	n += len(m.Header)
	if m.Body != nil {
		n++
	}
	if m.BodyContains != "" {
		n++
	}
	return n
}

// pickRule 在规则中按确定顺序裁决：
//  1. Priority 高者优先；
//  2. 并列时具体程度（specificity）高者优先；
//  3. 再并列时注册早者优先。
//
// 已消费完次数（Times>0 且 consumed>=Times）的规则不再参与匹配。
// 未命中时返回带每条规则未命中原因的错误。
func pickRule(rules []*Rule, req Request) (*Rule, *NoMatchError) {
	type cand struct {
		r       *Rule
		reasons []string
	}
	var matched []*Rule
	nm := &NoMatchError{Request: req}
	for _, r := range rules {
		if r.Times > 0 && r.consumed >= r.Times {
			nm.Reasons = append(nm.Reasons, RuleMismatch{
				RuleName: r.Name,
				Reasons:  []string{"exhausted: consumed all declared times"},
			})
			continue
		}
		if reasons := matchRequest(r.Match, req); reasons != nil {
			nm.Reasons = append(nm.Reasons, RuleMismatch{RuleName: r.Name, Reasons: reasons})
			continue
		}
		matched = append(matched, r)
	}
	if len(matched) == 0 {
		return nil, nm
	}
	sort.SliceStable(matched, func(i, j int) bool {
		a, b := matched[i], matched[j]
		if a.Priority != b.Priority {
			return a.Priority > b.Priority
		}
		sa, sb := specificity(a.Match), specificity(b.Match)
		if sa != sb {
			return sa > sb
		}
		return a.seq < b.seq
	})
	return matched[0], nil
}
