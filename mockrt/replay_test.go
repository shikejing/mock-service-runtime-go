package mockrt

import (
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestRecordAndReplay(t *testing.T) {
	// 真实依赖：一个本地 HTTP 服务。
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Backend", "real")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte("real-response:" + r.URL.Path))
	}))
	defer backend.Close()

	// 录制：通过 RecordTransport 走真实交互。
	cassette := &Cassette{}
	recClient := &http.Client{Transport: RecordTransport(nil, cassette)}
	req, _ := http.NewRequest("POST", backend.URL+"/orders?id=7", nil)
	req.Header.Set("X-Req", "1")
	resp, err := recClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(b) != "real-response:/orders" {
		t.Fatalf("record pass-through got %q", b)
	}
	if len(cassette.Entries) != 1 {
		t.Fatalf("cassette has %d entries", len(cassette.Entries))
	}

	// 持久化并重新加载。
	path := filepath.Join(t.TempDir(), "cassette.json")
	if err := Save(path, cassette); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}

	// 回放：无外部依赖（backend 已不可达也无妨），结果须与录制一致。
	rt := NewRuntime(DefaultConfig())
	c := rt.Begin()
	defer c.Close()
	for _, r := range ReplayRules(loaded) {
		c.AddRule(r)
	}
	replayClient := &http.Client{Transport: rt.Transport()}
	req2, _ := http.NewRequest("POST", "http://dep.internal/orders?id=7", nil)
	req2.Header.Set("X-Req", "1")
	resp2, err := replayClient.Do(req2.WithContext(c.Context()))
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	b2, _ := io.ReadAll(resp2.Body)
	if resp2.StatusCode != http.StatusCreated {
		t.Fatalf("replayed status = %d", resp2.StatusCode)
	}
	if string(b2) != "real-response:/orders" {
		t.Fatalf("replayed body = %q", b2)
	}
	if resp2.Header.Get("X-Backend") != "real" {
		t.Fatal("replayed header missing")
	}
	// 每条录制规则只消费一次，第二次同样请求报 MissExhausted。
	req3, _ := http.NewRequest("POST", "http://dep.internal/orders?id=7", nil)
	req3.Header.Set("X-Req", "1")
	_, err = replayClient.Do(req3.WithContext(c.Context()))
	if ue := unwrapUnmatched(err); ue == nil || ue.Reason != MissExhausted {
		t.Fatalf("second replay got %v, want MissExhausted", err)
	}
}

func TestCassetteAppendKeepsExistingEntriesStable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "c.json")
	c1 := &Cassette{Entries: []Entry{
		{Request: RecordedRequest{Method: "GET", Path: "/a"}, Response: RecordedResponse{Status: 200, Body: []byte("A")}},
	}}
	if err := Save(path, c1); err != nil {
		t.Fatal(err)
	}
	// 追加新条目，既有条目内容不得改变。
	loaded, _ := Load(path)
	loaded.Entries = append(loaded.Entries, Entry{
		Request: RecordedRequest{Method: "GET", Path: "/b"}, Response: RecordedResponse{Status: 200, Body: []byte("B")},
	})
	if err := Save(path, loaded); err != nil {
		t.Fatal(err)
	}
	reloaded, _ := Load(path)
	if len(reloaded.Entries) != 2 {
		t.Fatalf("entries = %d", len(reloaded.Entries))
	}
	first := reloaded.Entries[0]
	if first.Request.Path != "/a" || string(first.Response.Body) != "A" {
		t.Fatalf("existing entry mutated: %+v", first)
	}
}

func TestLoadRejectsNewerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.json")
	if err := Save(path, &Cassette{Version: CassetteVersion + 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected version error")
	}
}
