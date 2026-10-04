package mockrt

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func realBackend() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "real")
		w.WriteHeader(200)
		w.Write([]byte("real:" + r.URL.Path)) //nolint:errcheck
	}))
}

func getBody(t *testing.T, c *http.Client, url string) string {
	t.Helper()
	resp, err := c.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestRecordThenReplay(t *testing.T) {
	backend := realBackend()
	defer backend.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "case.cassette.json")

	// 录制：放行到真实依赖并落盘
	rt := New()
	c1, _ := LoadCassette(path)
	s := rt.Begin("record")
	s.UseCassette(c1, ModeRecord)
	want := getBody(t, s.Client(), backend.URL+"/hello")
	if want != "real:/hello" {
		t.Fatalf("record got %q", want)
	}
	s.Close()
	if err := c1.Save(); err != nil {
		t.Fatal(err)
	}
	rt.Close()

	// 回放：真实依赖已不可达，结果必须与录制一致
	rt2 := New()
	defer rt2.Close()
	c2, err := LoadCassette(path)
	if err != nil {
		t.Fatal(err)
	}
	s2 := rt2.Begin("replay")
	defer s2.Close()
	s2.UseCassette(c2, ModeReplay)
	got := getBody(t, s2.Client(), backend.URL+"/hello")
	if got != want {
		t.Fatalf("replay = %q, want %q", got, want)
	}

	// 回放未录制的请求 -> 明确错误
	if _, err := s2.Client().Get(backend.URL + "/unknown"); err == nil {
		t.Fatal("replay of unrecorded request must fail")
	}
}

func TestCassetteAppendOnlyStability(t *testing.T) {
	backend := realBackend()
	defer backend.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "stable.cassette.json")

	rt := New()
	defer rt.Close()

	// 第一批录制
	c1, _ := LoadCassette(path)
	s1 := rt.Begin("batch1")
	s1.UseCassette(c1, ModeRecord)
	first := getBody(t, s1.Client(), backend.URL+"/a")
	s1.Close()
	if err := c1.Save(); err != nil {
		t.Fatal(err)
	}
	committed, _ := os.ReadFile(path)

	// 第二批录制：追加新条目，不得改写既有条目
	c2, _ := LoadCassette(path)
	s2 := rt.Begin("batch2")
	s2.UseCassette(c2, ModeRecord)
	getBody(t, s2.Client(), backend.URL+"/b")
	s2.Close()
	if err := c2.Save(); err != nil {
		t.Fatal(err)
	}
	merged, _ := os.ReadFile(path)
	if len(merged) <= len(committed) {
		t.Fatal("expected cassette to grow after appending")
	}

	// 既有用例的回放结果不受新增条目影响
	c3, _ := LoadCassette(path)
	s3 := rt.Begin("replay-old")
	defer s3.Close()
	s3.UseCassette(c3, ModeReplay)
	if got := getBody(t, s3.Client(), backend.URL+"/a"); got != first {
		t.Fatalf("existing case destabilized: %q vs %q", got, first)
	}
	if got := getBody(t, s3.Client(), backend.URL+"/b"); got != "real:/b" {
		t.Fatalf("new entry not replayable: %q", got)
	}
}

func TestReplayRepeatedCallsInOrder(t *testing.T) {
	// 后端对同一路径返回递增计数，录制后回放必须按序重现
	var n int
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		w.Write([]byte{byte('0' + n)}) //nolint:errcheck
	}))
	defer backend.Close()
	path := filepath.Join(t.TempDir(), "seq.cassette.json")

	rt := New()
	defer rt.Close()
	c1, _ := LoadCassette(path)
	s1 := rt.Begin("rec")
	s1.UseCassette(c1, ModeRecord)
	a := getBody(t, s1.Client(), backend.URL+"/seq")
	b := getBody(t, s1.Client(), backend.URL+"/seq")
	s1.Close()
	if err := c1.Save(); err != nil {
		t.Fatal(err)
	}

	c2, _ := LoadCassette(path)
	s2 := rt.Begin("replay")
	defer s2.Close()
	s2.UseCassette(c2, ModeReplay)
	if got := getBody(t, s2.Client(), backend.URL+"/seq"); got != a {
		t.Fatalf("first replay = %q, want %q", got, a)
	}
	if got := getBody(t, s2.Client(), backend.URL+"/seq"); got != b {
		t.Fatalf("second replay = %q, want %q", got, b)
	}
	// 录制条目消费完后，再次请求必须报错而不是循环
	if _, err := s2.Client().Get(backend.URL + "/seq"); err == nil {
		t.Fatal("exhausted cassette must not silently cycle")
	}
}
