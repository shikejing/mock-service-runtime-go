package mockrt

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFaultError(t *testing.T) {
	rt := New()
	defer rt.Close()
	s := rt.Begin("err")
	defer s.Close()
	sentinel := errors.New("backend exploded")
	s.AddRule(Rule{Name: "boom", Match: Matcher{Path: "/boom"},
		Fault: &Fault{Err: sentinel}})
	_, err := s.Client().Get("http://dep/boom")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	// 异常链路能定位到具体规则
	if !strings.Contains(err.Error(), `"boom"`) {
		t.Fatalf("error should name the triggering rule, got %v", err)
	}
}

func TestFaultConnReset(t *testing.T) {
	rt := New()
	defer rt.Close()
	s := rt.Begin("reset")
	defer s.Close()
	s.AddRule(Rule{Name: "rst", Match: Matcher{Path: "/rst"},
		Fault: &Fault{Reset: true}})
	_, err := s.Client().Get("http://dep/rst")
	if !IsConnReset(err) {
		t.Fatalf("expected conn reset, got %v", err)
	}
}

func TestFaultDelayAndTimeout(t *testing.T) {
	rt := New(WithConfig(Config{Enabled: true, Timeout: 100 * time.Millisecond}))
	defer rt.Close()
	s := rt.Begin("delay")
	defer s.Close()
	s.AddRule(Rule{Name: "slow", Match: Matcher{Path: "/slow"},
		Response: Response{Body: []byte("late")},
		Fault:    &Fault{Delay: 50 * time.Millisecond}})
	start := time.Now()
	resp, err := s.Client().Get("http://dep/slow")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Fatalf("delay not applied: %v", d)
	}

	// 延迟超过用例超时 -> 超时错误
	s.AddRule(Rule{Name: "too-slow", Match: Matcher{Path: "/too"},
		Response: Response{Body: []byte("never")},
		Fault:    &Fault{Delay: time.Second}})
	if _, err := s.Client().Get("http://dep/too"); err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestFaultStreamAbort(t *testing.T) {
	rt := New()
	defer rt.Close()
	s := rt.Begin("abort")
	defer s.Close()
	s.AddRule(Rule{
		Name:  "stream",
		Match: Matcher{Path: "/stream"},
		Response: Response{
			Status: 200,
			Chunks: [][]byte{[]byte("chunk1-"), []byte("chunk2-"), []byte("chunk3")},
		},
		Fault: &Fault{AbortAfter: 8}, // 只放行 8 字节后中断
	})
	resp, err := s.Client().Get("http://dep/stream")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if !IsConnReset(err) {
		t.Fatalf("expected mid-stream reset, got data=%q err=%v", data, err)
	}
	if string(data) != "chunk1-c" {
		t.Fatalf("partial data = %q, want %q", data, "chunk1-c")
	}
}

func TestFaultStreamComplete(t *testing.T) {
	rt := New()
	defer rt.Close()
	s := rt.Begin("stream-ok")
	defer s.Close()
	s.AddRule(Rule{
		Name:  "stream",
		Match: Matcher{Path: "/s"},
		Response: Response{
			Chunks: [][]byte{[]byte("a"), []byte("b"), []byte("c")},
		},
	})
	resp, err := s.Client().Get("http://dep/s")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil || string(data) != "abc" {
		t.Fatalf("data=%q err=%v", data, err)
	}
}

func TestFaultViaHandler(t *testing.T) {
	rt := New()
	defer rt.Close()
	s := rt.Begin("handler")
	defer s.Close()
	s.AddRule(Rule{Name: "ok", Match: Matcher{Path: "/ok"},
		Response: Response{Status: 202, Body: []byte("hi"),
			Header: map[string][]string{"X-Mock": {"yes"}}}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "http://dep/ok", nil)
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 202 || rec.Header().Get("X-Mock") != "yes" {
		t.Fatalf("status=%d header=%v", rec.Code, rec.Header())
	}
}

func TestAssertOrderAndVerify(t *testing.T) {
	rt := New()
	defer rt.Close()
	s := rt.Begin("assert")
	s.AddRule(Rule{Name: "a", Times: 1, Match: Matcher{Path: "/a"},
		Response: Response{Body: []byte("a")}})
	s.AddRule(Rule{Name: "b", Times: 1, Match: Matcher{Path: "/b"},
		Response: Response{Body: []byte("b")}})
	c := s.Client()
	doReq(c, "/a")
	doReq(c, "/b")
	if err := s.AssertOrder("a", "b"); err != nil {
		t.Fatal(err)
	}
	if err := s.AssertOrder("b", "a"); err == nil {
		t.Fatal("wrong order must fail")
	}
	if err := s.Verify(); err != nil {
		t.Fatalf("clean verify: %v", err)
	}
	s.Close()

	// 未消费规则与多余调用分别报告
	s2 := rt.Begin("verify")
	defer s2.Close()
	s2.AddRule(Rule{Name: "needed", Times: 2, Match: Matcher{Path: "/n"},
		Response: Response{Body: []byte("n")}})
	doReq(s2.Client(), "/n")     // 消费 1/2
	doReq(s2.Client(), "/stray") // 多余调用
	err := s2.Verify()
	verr, ok := err.(*VerificationError)
	if !ok {
		t.Fatalf("err type = %T", err)
	}
	if len(verr.Unconsumed) != 1 || !strings.Contains(verr.Unconsumed[0], "needed") {
		t.Fatalf("unconsumed = %v", verr.Unconsumed)
	}
	if len(verr.Extra) != 1 || verr.Extra[0].Request.Path != "/stray" {
		t.Fatalf("extra = %+v", verr.Extra)
	}
}

func doReq(c *http.Client, path string) {
	resp, err := c.Get("http://dep" + path)
	if err == nil {
		resp.Body.Close()
	}
}
