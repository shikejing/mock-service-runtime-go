package mockrt

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

func doBody(t *testing.T, rt *Runtime, c *Case, method, target string) (string, error) {
	t.Helper()
	client := &http.Client{Transport: rt.Transport()}
	req := newRequest(t, method, target, "").WithContext(c.Context())
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), nil
}

func TestParallelCasesIsolated(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c := rt.Begin()
			defer c.Close()
			// 所有用例声明同名、同特征的规则，但响应体不同。
			c.AddRule(Rule{
				Name:     "same-name",
				Matcher:  Matcher{Method: "GET", Path: "/same"},
				Response: Response{Status: 200, Body: []byte(fmt.Sprintf("case-%d", i))},
			})
			body, err := doBody(t, rt, c, "GET", "/same")
			if err != nil {
				errs <- err
				return
			}
			if want := fmt.Sprintf("case-%d", i); body != want {
				errs <- fmt.Errorf("case %d saw %q, want %q", i, body, want)
				return
			}
			// 每个用例只能看到自己的记录。
			calls := c.Recorder().Calls()
			if len(calls) != 1 || calls[0].RuleName != "same-name" {
				errs <- fmt.Errorf("case %d calls = %+v", i, calls)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestCaseCloseRestoresCleanly(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	c.AddRule(Rule{Name: "r", Matcher: Matcher{Path: "/x"}, Response: Response{Status: 200}})
	if _, err := doBody(t, rt, c, "GET", "/x"); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// 接管失效后：经该用例 context 的请求必须报 MissDisabled，不得命中残留规则。
	_, err := doBody(t, rt, c, "GET", "/x")
	ue := unwrapUnmatched(err)
	if ue == nil || ue.Reason != MissDisabled {
		t.Fatalf("after close got %v, want MissDisabled", err)
	}
	// 新用例看不到旧用例的规则与记录。
	c2 := rt.Begin()
	defer c2.Close()
	_, err = doBody(t, rt, c2, "GET", "/x")
	if ue := unwrapUnmatched(err); ue == nil || ue.Reason != MissNoRules {
		t.Fatalf("new case got %v, want MissNoRules", err)
	}
	if got := len(c2.Recorder().Calls()); got != 0 {
		t.Fatalf("new case sees %d calls, want 0", got)
	}
}

func TestConfigSnapshotAffectsOnlyLaterCases(t *testing.T) {
	rt := NewRuntime(DefaultConfig())
	// 用例 1 在启用时开始。
	c1 := rt.Begin()
	c1.AddRule(Rule{Name: "r1", Matcher: Matcher{Path: "/a"}, Response: Response{Status: 200}})
	// 运行期禁用接管。
	cfg := rt.Config()
	cfg.Enabled = false
	rt.SetConfig(cfg)
	// 进行中的用例 1 不受影响（持有快照）。
	if _, err := doBody(t, rt, c1, "GET", "/a"); err != nil {
		t.Fatalf("in-flight case should be unaffected by config change: %v", err)
	}
	// 之后开始的用例 2 受新配置影响。
	c2 := rt.Begin()
	c2.AddRule(Rule{Name: "r2", Matcher: Matcher{Path: "/a"}, Response: Response{Status: 200}})
	_, err := doBody(t, rt, c2, "GET", "/a")
	if ue := unwrapUnmatched(err); ue == nil || ue.Reason != MissDisabled {
		t.Fatalf("later case got %v, want MissDisabled", err)
	}
	c1.Close()
	c2.Close()
}

func TestTimeoutSnapshotAppliesToInflightCase(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Timeout = 50 * time.Millisecond
	rt := NewRuntime(cfg)
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{
		Name:     "slow",
		Matcher:  Matcher{Path: "/slow"},
		Response: Response{Status: 200},
		Fault:    Fault{Delay: 500 * time.Millisecond},
	})
	// 运行期把超时调大，不影响进行中的用例。
	cfg2 := rt.Config()
	cfg2.Timeout = 5 * time.Second
	rt.SetConfig(cfg2)

	start := time.Now()
	_, err := doBody(t, rt, c, "GET", "/slow")
	if err == nil {
		t.Fatal("expected timeout fault")
	}
	if elapsed := time.Since(start); elapsed > 300*time.Millisecond {
		t.Fatalf("timeout took %v, want ~50ms", elapsed)
	}
	fe := unwrapFault(err)
	if fe == nil || fe.RuleName != "slow" {
		t.Fatalf("fault should locate rule 'slow', got %v", err)
	}
}

func TestConcurrencyLimit(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Concurrency = 2
	rt := NewRuntime(cfg)
	c := rt.Begin()
	defer c.Close()
	c.AddRule(Rule{
		Name:     "limited",
		Matcher:  Matcher{Path: "/l"},
		Response: Response{Status: 200},
		Fault:    Fault{Delay: 50 * time.Millisecond},
	})
	start := time.Now()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := doBody(t, rt, c, "GET", "/l"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	// 8 次调用、每次 50ms 编排延迟、并发上限 2：
	// 串行批次至少 4 批 => 耗时下界约 200ms；若无上限则约 50ms。
	if elapsed := time.Since(start); elapsed < 180*time.Millisecond {
		t.Fatalf("elapsed %v suggests concurrency limit 2 was not enforced", elapsed)
	}
}

func unwrapFault(err error) *FaultError {
	for err != nil {
		if fe, ok := err.(*FaultError); ok {
			return fe
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return nil
		}
		err = u.Unwrap()
	}
	return nil
}
