package mockrt

import (
	"bytes"
	"net/http"
	"sort"
)

// matchResult 是匹配裁决结果（内部）。
type matchResult struct {
	rule    *Rule      // 命中的规则，nil 表示未命中
	reason  MissReason // 未命中原因
	closest *Rule      // 最接近的规则（差条件数最少），用于错误定位
}

// specificity 计算匹配条件的具体程度，用于裁决排序（越大越具体）。
// 方法/路径各计 1，每个查询参数、请求头各计 1，请求体计 1。
func specificity(m Matcher) int {
	n := 0
	if m.Method != "" {
		n++
	}
	if m.Path != "" {
		n++
	}
	n += len(m.Query)
	n += len(m.Headers)
	if m.Body != nil {
		n++
	}
	return n
}

// matchOne 判断请求是否命中单条规则，未命中时给出第一个不满足的条件。
// 检查顺序固定为：方法 → 路径 → 查询参数 → 请求头 → 请求体，
// 保证同一请求对同一规则集的判定结果确定。
func matchOne(m Matcher, req *http.Request, body []byte) MissReason {
	if m.Method != "" && m.Method != req.Method {
		return MissMethod
	}
	if m.Path != "" && m.Path != req.URL.Path {
		return MissPath
	}
	if len(m.Query) > 0 {
		q := req.URL.Query()
		keys := sortedKeys(m.Query)
		for _, k := range keys {
			if q.Get(k) != m.Query[k] {
				return MissQuery
			}
		}
	}
	if len(m.Headers) > 0 {
		keys := sortedKeys(m.Headers)
		for _, k := range keys {
			if req.Header.Get(k) != m.Headers[k] {
				return MissHeader
			}
		}
	}
	if m.Body != nil && !bytes.Equal(m.Body, body) {
		return MissBody
	}
	return MissNone
}

// resolve 在规则集中按确定顺序裁决，多条规则同时命中时：
//  1. 具体程度（specificity）高者优先；
//  2. 具体程度相同则声明顺序（下标）小者优先。
//
// 未命中时返回最接近的规则（差条件数最少，平局取声明顺序小者）及原因。
func resolve(rules []*Rule, req *http.Request, body []byte) matchResult {
	if len(rules) == 0 {
		return matchResult{reason: MissNoRules}
	}
	type cand struct {
		rule   *Rule
		idx    int
		spec   int
		reason MissReason
	}
	cands := make([]cand, 0, len(rules))
	for i, r := range rules {
		cands = append(cands, cand{rule: r, idx: i, spec: specificity(r.Matcher), reason: matchOne(r.Matcher, req, body)})
	}
	// 稳定排序：先具体程度降序，再声明顺序升序。
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].spec != cands[j].spec {
			return cands[i].spec > cands[j].spec
		}
		return cands[i].idx < cands[j].idx
	})
	for _, c := range cands {
		if c.reason == MissNone {
			return matchResult{rule: c.rule}
		}
	}
	// 全部未命中：取排序后第一条（最具体、声明最早）作为最接近规则。
	return matchResult{reason: cands[0].reason, closest: cands[0].rule}
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
