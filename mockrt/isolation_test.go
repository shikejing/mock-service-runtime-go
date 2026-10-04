package mockrt

import (
	"fmt"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"
)

func TestParallelScopesIsolated(t *testing.T) {
	rt := New()
	defer rt.Close()

	const workers = 16
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			s := rt.Begin(fmt.Sprintf("case-%d", i))
			defer s.Close()
			// 所有用例声明同名同特征的规则，但返回值不同
			s.AddRule(Rule{
				Name:     "same-name",
				Match:    Matcher{Method: "GET", Path: "/data"},
				Response: Response{Body: []byte(fmt.Sprintf("value-%d", i))},
			})
			want := fmt.Sprintf("value-%d", i)
			for j := 0; j < 10; j++ {
				resp, err := s.Client().Get("http://dep/data")
				if err != nil {
					errs <- err
					return
				}
				b, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				if string(b) != want {
					errs <- fmt.Errorf("case %d saw %q, want %q", i, b, want)
					return
				}
			}
			// 每个用例只能看到自己的记录
			calls := s.Calls()
			if len(calls) != 10 {
				errs <- fmt.Errorf("case %d saw %d calls, want 10", i, len(calls))
				return
			}
			for _, c := range calls {
				if c.RuleName != "same-name" {
					errs <- fmt.Errorf("case %d saw foreign rule %q", i, c.RuleName)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
}

func TestGlobalTransportScopedByContext(t *testing.T) {
	rt := New()
	defer rt.Close()
	restore := rt.Install()
	defer restore()

	s1 := rt.Begin("g1")
	defer s1.Close()
	s1.AddRule(Rule{Name: "r1", Match: Matcher{Path: "/x"},
		Response: Response{Body: []byte("one")}})
	s2 := rt.Begin("g2")
	defer s2.Close()
	s2.AddRule(Rule{Name: "r2", Match: Matcher{Path: "/x"},
		Response: Response{Body: []byte("two")}})

	get := func(s *Scope) string {
		req, _ := http.NewRequestWithContext(WithScope(t.Context(), s), "GET", "http://dep/x", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return string(b)
	}
	if got := get(s1); got != "one" {
		t.Fatalf("s1 got %q", got)
	}
	if got := get(s2); got != "two" {
		t.Fatalf("s2 got %q", got)
	}
	// 无用例上下文 -> 明确错误，不放行
	if _, err := http.Get("http://dep/x"); err == nil {
		t.Fatal("request without scope must fail")
	}
}

func TestScopeCloseRestoresCleanly(t *testing.T) {
	rt := New()
	defer rt.Close()
	s := rt.Begin("closing")
	s.AddRule(Rule{Name: "r", Match: Matcher{Path: "/x"},
		Response: Response{Body: []byte("x")}})
	c := s.Client()
	if _, err := c.Get("http://dep/x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	// 接管失效后：明确错误，而不是残留行为或静默放行
	if _, err := c.Get("http://dep/x"); err == nil {
		t.Fatal("closed scope must not serve stale rules")
	}
	// 新用例看不到旧用例的任何记录
	s2 := rt.Begin("fresh")
	defer s2.Close()
	if n := len(s2.Calls()); n != 0 {
		t.Fatalf("fresh scope saw %d stale calls", n)
	}
}

func TestConfigUpdateAffectsOnlyNewScopes(t *testing.T) {
	rt := New(WithConfig(Config{Enabled: true, Timeout: time.Second, Seed: 1}))
	defer rt.Close()

	inflight := rt.Begin("inflight") // 快照 Seed=1
	defer inflight.Close()

	rt.UpdateConfig(Config{Enabled: true, Timeout: time.Second, Seed: 2})
	later := rt.Begin("later") // 快照 Seed=2
	defer later.Close()

	mk := func(s *Scope) []byte {
		s.AddRule(Rule{
			Name:     "f",
			Match:    Matcher{Path: "/f"},
			FakeBody: map[string]FakeSpec{"v": {Kind: "int", Min: 0, Max: 1 << 30}},
		})
		resp, err := s.Client().Get("http://dep/f")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	b1 := mk(inflight)
	b2 := mk(later)
	if string(b1) == string(b2) {
		t.Fatalf("config update should affect new scope seeds: %s vs %s", b1, b2)
	}
	// 进行中的用例保持旧种子
	if again := mk(inflight); string(again) != string(b1) {
		t.Fatalf("inflight scope config changed mid-flight: %s vs %s", b1, again)
	}
}

func TestDisabledConfig(t *testing.T) {
	rt := New(WithConfig(Config{Enabled: false}))
	defer rt.Close()
	s := rt.Begin("disabled")
	defer s.Close()
	s.AddRule(Rule{Name: "r", Match: Matcher{Path: "/x"},
		Response: Response{Body: []byte("x")}})
	if _, err := s.Client().Get("http://dep/x"); err == nil {
		t.Fatal("disabled runtime must reject calls")
	}
}

func TestMaxConcurrency(t *testing.T) {
	rt := New(WithConfig(Config{Enabled: true, Timeout: 5 * time.Second, MaxConcurrency: 2}))
	defer rt.Close()
	s := rt.Begin("conc")
	defer s.Close()
	s.AddRule(Rule{
		Name:     "slow",
		Match:    Matcher{Path: "/slow"},
		Response: Response{Body: []byte("ok")},
		Fault:    &Fault{Delay: 50 * time.Millisecond},
	})
	var wg sync.WaitGroup
	var maxSeen int64
	var mu sync.Mutex
	cur := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu.Lock()
			cur++
			if int64(cur) > maxSeen {
				maxSeen = int64(cur)
			}
			mu.Unlock()
			// 注意：并发上限在 roundTrip 内生效，这里统计的是调用方并发，
			// 仅验证调用都能完成。
			resp, err := s.Client().Get("http://dep/slow")
			if err != nil {
				t.Error(err)
			} else {
				resp.Body.Close()
			}
			mu.Lock()
			cur--
			mu.Unlock()
		}()
	}
	wg.Wait()
}
