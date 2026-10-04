package mockrt

import (
	"errors"
	"io"
	"net/http"
	"testing"
	"time"
)

func TestFaultErrorLocatesRule(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	sentinel := errors.New("backend exploded")
	c.AddRule(Rule{Name: "flaky-dep", Matcher: Matcher{Path: "/flaky"}, Fault: Fault{Error: sentinel}})

	client := &http.Client{Transport: rt.Transport()}
	_, err := client.Do(newRequest(t, "GET", "/flaky", "").WithContext(c.Context()))
	fe := unwrapFault(err)
	if fe == nil {
		t.Fatalf("expected FaultError, got %v", err)
	}
	if fe.RuleName != "flaky-dep" {
		t.Fatalf("fault not attributed to rule: %q", fe.RuleName)
	}
	if !errors.Is(err, sentinel) {
		t.Fatalf("sentinel error not unwrappable: %v", err)
	}
}

func TestFaultAbort(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{Name: "resetter", Matcher: Matcher{Path: "/r"}, Fault: Fault{Abort: true}})

	client := &http.Client{Transport: rt.Transport()}
	_, err := client.Do(newRequest(t, "GET", "/r", "").WithContext(c.Context()))
	fe := unwrapFault(err)
	if fe == nil || !fe.Abort || fe.RuleName != "resetter" {
		t.Fatalf("got %v, want abort fault on rule resetter", err)
	}
}

func TestFaultDelay(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{Name: "slow", Matcher: Matcher{Path: "/d"}, Response: Response{Status: 200, Body: []byte("ok")}, Fault: Fault{Delay: 80 * time.Millisecond}})

	client := &http.Client{Transport: rt.Transport()}
	start := time.Now()
	resp, err := client.Do(newRequest(t, "GET", "/d", "").WithContext(c.Context()))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if elapsed := time.Since(start); elapsed < 80*time.Millisecond {
		t.Fatalf("delay not applied: %v", elapsed)
	}
}

func TestStreamResponse(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{
		Name:    "stream",
		Matcher: Matcher{Path: "/s"},
		Response: Response{
			Status:        200,
			Chunks:        [][]byte{[]byte("aaa"), []byte("bbb"), []byte("ccc")},
			TruncateAfter: -1,
		},
	})
	client := &http.Client{Transport: rt.Transport()}
	resp, err := client.Do(newRequest(t, "GET", "/s", "").WithContext(c.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "aaabbbccc" {
		t.Fatalf("got %q", b)
	}
}

func TestStreamTruncatedMidway(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{
		Name:    "cut",
		Matcher: Matcher{Path: "/cut"},
		Response: Response{
			Status:        200,
			Chunks:        [][]byte{[]byte("aaaa"), []byte("bbbb"), []byte("cccc")},
			TruncateAfter: 5, // 发送 5 字节后中断
		},
	})
	client := &http.Client{Transport: rt.Transport()}
	resp, err := client.Do(newRequest(t, "GET", "/cut", "").WithContext(c.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("expected mid-stream termination, got full body %q", b)
	}
	if string(b) != "aaaab" {
		t.Fatalf("partial body = %q, want %q", b, "aaaab")
	}
}

func TestResponseHeadersAndStatus(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{
		Name:     "full",
		Matcher:  Matcher{Path: "/full"},
		Response: Response{Status: 418, Headers: map[string]string{"X-Mock": "yes"}, Body: []byte("teapot")},
	})
	client := &http.Client{Transport: rt.Transport()}
	resp, err := client.Do(newRequest(t, "GET", "/full", "").WithContext(c.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 418 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Mock") != "yes" {
		t.Fatal("response header missing")
	}
}
