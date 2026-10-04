package mockrt

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func do(t *testing.T, c *http.Client, method, url string, body string, headers map[string]string) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMatchByMethodPathQueryHeaderBody(t *testing.T) {
	rt := New()
	defer rt.Close()
	s := rt.Begin("match-all")
	s.AddRule(Rule{
		Name: "full",
		Match: Matcher{
			Method: "POST",
			Path:   "/api",
			Query:  map[string]string{"q": "1"},
			Header: map[string]string{"X-Token": "abc"},
			Body:   []byte("hello"),
		},
		Response: Response{Status: 201, Body: []byte("ok")},
	})
	c := s.Client()

	resp := do(t, c, "POST", "http://dep/api?q=1", "hello", map[string]string{"X-Token": "abc"})
	if resp.StatusCode != 201 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "ok" {
		t.Fatalf("body = %q", b)
	}

	// 查询参数不匹配 -> 明确错误，且包含原因
	_, err := c.Get("http://dep/api?q=2")
	var nm *NoMatchError
	if !errors.As(err, &nm) {
		t.Fatalf("expected NoMatchError, got %v", err)
	}
	if len(nm.Reasons) != 1 || len(nm.Reasons[0].Reasons) == 0 {
		t.Fatalf("expected mismatch reasons, got %+v", nm.Reasons)
	}
	t.Logf("mismatch reasons: %+v", nm.Reasons[0].Reasons)
}

func TestAdjudicationOrder(t *testing.T) {
	rt := New()
	defer rt.Close()

	// 1. Priority 高者优先，即使注册更晚、更笼统
	s := rt.Begin("priority")
	s.AddRule(Rule{Name: "specific", Match: Matcher{Method: "GET", Path: "/a"},
		Response: Response{Body: []byte("specific")}})
	s.AddRule(Rule{Name: "prio", Priority: 10, Match: Matcher{Path: "/a"},
		Response: Response{Body: []byte("prio")}})
	resp := do(t, s.Client(), "GET", "http://dep/a", "", nil)
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "prio" {
		t.Fatalf("priority rule should win, got %q", b)
	}
	s.Close()

	// 2. 同优先级时更具体者优先
	s2 := rt.Begin("specificity")
	s2.AddRule(Rule{Name: "loose", Match: Matcher{Path: "/a"},
		Response: Response{Body: []byte("loose")}})
	s2.AddRule(Rule{Name: "tight", Match: Matcher{Method: "GET", Path: "/a"},
		Response: Response{Body: []byte("tight")}})
	resp = do(t, s2.Client(), "GET", "http://dep/a", "", nil)
	b, _ = io.ReadAll(resp.Body)
	if string(b) != "tight" {
		t.Fatalf("more specific rule should win, got %q", b)
	}
	s2.Close()

	// 3. 完全并列时注册早者优先
	s3 := rt.Begin("registration")
	s3.AddRule(Rule{Name: "first", Match: Matcher{Path: "/a"},
		Response: Response{Body: []byte("first")}})
	s3.AddRule(Rule{Name: "second", Match: Matcher{Path: "/a"},
		Response: Response{Body: []byte("second")}})
	resp = do(t, s3.Client(), "GET", "http://dep/a", "", nil)
	b, _ = io.ReadAll(resp.Body)
	if string(b) != "first" {
		t.Fatalf("earlier registration should win, got %q", b)
	}
	s3.Close()
}

func TestTimesExhaustion(t *testing.T) {
	rt := New()
	defer rt.Close()
	s := rt.Begin("times")
	defer s.Close()
	s.AddRule(Rule{Name: "once", Times: 1, Match: Matcher{Path: "/a"},
		Response: Response{Body: []byte("once")}})
	s.AddRule(Rule{Name: "fallback", Match: Matcher{Path: "/a"},
		Response: Response{Body: []byte("fallback")}})
	c := s.Client()

	resp := do(t, c, "GET", "http://dep/a", "", nil)
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "once" {
		t.Fatalf("first call = %q", b)
	}
	resp = do(t, c, "GET", "http://dep/a", "", nil)
	b, _ = io.ReadAll(resp.Body)
	if string(b) != "fallback" {
		t.Fatalf("second call should fall back, got %q", b)
	}
	if err := s.AssertCount("once", 1); err != nil {
		t.Fatal(err)
	}
}

func TestNoMatchNeverSilent(t *testing.T) {
	rt := New()
	defer rt.Close()
	s := rt.Begin("nomatch")
	defer s.Close()
	s.AddRule(Rule{Name: "r", Match: Matcher{Path: "/x"},
		Response: Response{Body: []byte("x")}})
	_, err := s.Client().Get("http://dep/y")
	if err == nil {
		t.Fatal("unmatched request must not silently succeed")
	}
	var nm *NoMatchError
	if !errors.As(err, &nm) {
		t.Fatalf("error type = %T", err)
	}
}
