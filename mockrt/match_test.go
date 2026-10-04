package mockrt

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// newRequest 构造带方法与路径的测试请求。
func newRequest(t *testing.T, method, target string, body string) *http.Request {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, "http://dep.internal"+target, rdr)
	if err != nil {
		t.Fatal(err)
	}
	return req
}

func TestSpecificityWinsOverDeclarationOrder(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	// 先声明宽泛规则，后声明具体规则；具体规则应优先。
	c.AddRule(Rule{Name: "broad", Matcher: Matcher{Method: "GET"}, Response: Response{Status: 200, Body: []byte("broad")}})
	c.AddRule(Rule{Name: "specific", Matcher: Matcher{Method: "GET", Path: "/a", Query: map[string]string{"x": "1"}}, Response: Response{Status: 200, Body: []byte("specific")}})

	client := &http.Client{Transport: rt.Transport()}
	req := newRequest(t, "GET", "/a?x=1", "").WithContext(c.Context())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "specific" {
		t.Fatalf("expected specific rule to win, got %q", b)
	}
}

func TestDeclarationOrderBreaksTie(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{Name: "first", Matcher: Matcher{Method: "GET", Path: "/a"}, Response: Response{Body: []byte("first")}})
	c.AddRule(Rule{Name: "second", Matcher: Matcher{Method: "GET", Path: "/a"}, Response: Response{Body: []byte("second")}})

	client := &http.Client{Transport: rt.Transport()}
	req := newRequest(t, "GET", "/a", "").WithContext(c.Context())
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "first" {
		t.Fatalf("tie should be broken by declaration order, got %q", b)
	}
}

func TestMissReasonsAreDistinguishable(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{
		Name:    "strict",
		Matcher: Matcher{Method: "POST", Path: "/p", Query: map[string]string{"q": "1"}, Headers: map[string]string{"X-H": "v"}, Body: []byte("payload")},
	})

	cases := []struct {
		name string
		req  *http.Request
		want MissReason
	}{
		{"method", newRequest(t, "GET", "/p?q=1", "payload"), MissMethod},
		{"path", newRequest(t, "POST", "/other?q=1", "payload"), MissPath},
		{"query", newRequest(t, "POST", "/p?q=2", "payload"), MissQuery},
		{"body", newRequest(t, "POST", "/p?q=1", "wrong"), MissBody},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name != "body" && tc.name != "query" {
				tc.req.Header.Set("X-H", "v")
			}
			if tc.name == "query" || tc.name == "body" {
				tc.req.Header.Set("X-H", "v")
			}
			client := &http.Client{Transport: rt.Transport()}
			_, err := client.Do(tc.req.WithContext(c.Context()))
			if err == nil {
				t.Fatal("expected unmatched error")
			}
			ue, ok := err.(*UnmatchedError)
			if !ok {
				// http.Client 包装错误，取根因
				for e := err; e != nil; {
					if u, ok2 := e.(*UnmatchedError); ok2 {
						ue = u
						break
					}
					type unwrapper interface{ Unwrap() error }
					w, ok2 := e.(unwrapper)
					if !ok2 {
						break
					}
					e = w.Unwrap()
				}
			}
			if ue == nil {
				t.Fatalf("expected *UnmatchedError, got %T: %v", err, err)
			}
			if ue.Reason != tc.want {
				t.Fatalf("reason = %v, want %v", ue.Reason, tc.want)
			}
			if ue.Closest != "strict" {
				t.Fatalf("closest rule = %q, want strict", ue.Closest)
			}
		})
	}
}

func TestHeaderMissReason(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{Name: "hdr", Matcher: Matcher{Headers: map[string]string{"X-Need": "yes"}}})
	client := &http.Client{Transport: rt.Transport()}
	req := newRequest(t, "GET", "/x", "").WithContext(c.Context())
	_, err := client.Do(req)
	ue := unwrapUnmatched(err)
	if ue == nil || ue.Reason != MissHeader {
		t.Fatalf("got %v, want MissHeader", err)
	}
}

func TestNoRulesAndExhausted(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	client := &http.Client{Transport: rt.Transport()}

	_, err := client.Do(newRequest(t, "GET", "/a", "").WithContext(c.Context()))
	if ue := unwrapUnmatched(err); ue == nil || ue.Reason != MissNoRules {
		t.Fatalf("got %v, want MissNoRules", err)
	}

	c.AddRule(Rule{Name: "once", Matcher: Matcher{Path: "/a"}, Times: 1, Response: Response{Status: 200}})
	resp, err := client.Do(newRequest(t, "GET", "/a", "").WithContext(c.Context()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	_, err = client.Do(newRequest(t, "GET", "/a", "").WithContext(c.Context()))
	ue := unwrapUnmatched(err)
	if ue == nil || ue.Reason != MissExhausted || ue.RuleName != "once" {
		t.Fatalf("got %v, want MissExhausted on rule once", err)
	}
}

func unwrapUnmatched(err error) *UnmatchedError {
	for err != nil {
		if ue, ok := err.(*UnmatchedError); ok {
			return ue
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil
		}
		err = u.Unwrap()
	}
	return nil
}
