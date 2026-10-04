package mockrt

import (
	"strings"
	"testing"
)

func TestRecorderOrderAndCount(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{Name: "a", Matcher: Matcher{Path: "/a"}, Response: Response{Status: 200}})
	c.AddRule(Rule{Name: "b", Matcher: Matcher{Path: "/b"}, Response: Response{Status: 200}})

	doBody(t, rt, c, "GET", "/a")
	doBody(t, rt, c, "GET", "/b")
	doBody(t, rt, c, "GET", "/a")

	rec := c.Recorder()
	if err := rec.AssertOrder("a", "b", "a"); err != nil {
		t.Fatal(err)
	}
	if err := rec.AssertOrder("b", "a"); err != nil {
		t.Fatal(err)
	}
	if err := rec.AssertOrder("b", "a", "a"); err == nil {
		t.Fatal("expected order assertion to fail")
	}
	if err := rec.AssertCount("a", 2); err != nil {
		t.Fatal(err)
	}
	if err := rec.AssertCount("a", 1); err == nil {
		t.Fatal("expected count assertion to fail")
	}
	calls := rec.Calls()
	if len(calls) != 3 || calls[0].Seq >= calls[1].Seq || calls[1].Seq >= calls[2].Seq {
		t.Fatalf("calls not sequentially ordered: %+v", calls)
	}
}

func TestAssertExhaustedReportsSeparately(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{Name: "used", Matcher: Matcher{Path: "/u"}, Times: 1, Response: Response{Status: 200}})
	c.AddRule(Rule{Name: "unused", Matcher: Matcher{Path: "/n"}, Times: 2, Response: Response{Status: 200}})

	doBody(t, rt, c, "GET", "/u")

	err := c.Recorder().AssertExhausted()
	if err == nil {
		t.Fatal("expected exhaustion failure")
	}
	if !strings.Contains(err.Error(), "未消费的规则") || !strings.Contains(err.Error(), "unused") {
		t.Fatalf("unconsumed rules not reported: %v", err)
	}

	// 全部消费后应通过。
	doBody(t, rt, c, "GET", "/n")
	doBody(t, rt, c, "GET", "/n")
	if err := c.Recorder().AssertExhausted(); err != nil {
		t.Fatal(err)
	}
}

func TestRecordsIsolatedBetweenCases(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c1 := rt.Begin()
	defer c1.Close()
	c2 := rt.Begin()
	defer c2.Close()
	c1.AddRule(Rule{Name: "r", Matcher: Matcher{Path: "/x"}, Response: Response{Status: 200}})
	c2.AddRule(Rule{Name: "r", Matcher: Matcher{Path: "/x"}, Response: Response{Status: 200}})

	doBody(t, rt, c1, "GET", "/x")
	doBody(t, rt, c1, "GET", "/x")
	doBody(t, rt, c2, "GET", "/x")

	if got := len(c1.Recorder().Calls()); got != 2 {
		t.Fatalf("case1 sees %d calls, want 2", got)
	}
	if got := len(c2.Recorder().Calls()); got != 1 {
		t.Fatalf("case2 sees %d calls, want 1", got)
	}
}
