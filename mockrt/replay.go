package mockrt

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
)

// CassetteVersion 是当前录制格式版本。
// 兼容性约定：只允许新增可选字段，不得修改或删除既有字段语义；
// 加载时拒绝更高版本的文件，低版本文件按缺失字段为零值处理。
const CassetteVersion = 1

// Cassette 是一份录制内容：一次真实交互的有序记录，可持久化后回放。
// 新增条目只允许追加到 Entries 末尾，保证既有已提交用例的回放稳定。
type Cassette struct {
	Version int     `json:"version"`
	Entries []Entry `json:"entries"`
}

// Entry 是录制的一次请求/响应交换。
type Entry struct {
	Request  RecordedRequest  `json:"request"`
	Response RecordedResponse `json:"response"`
}

// RecordedRequest 录制的请求特征。
type RecordedRequest struct {
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// RecordedResponse 录制的响应。
type RecordedResponse struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers,omitempty"`
	Body    []byte            `json:"body,omitempty"`
}

// Save 将录制内容写入路径（JSON，键序确定，字节级稳定）。
func Save(path string, c *Cassette) error {
	if c.Version == 0 {
		c.Version = CassetteVersion
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// Load 从路径读取录制内容。
func Load(path string) (*Cassette, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Cassette
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.Version > CassetteVersion {
		return nil, fmt.Errorf("mockrt: cassette %s has version %d, this runtime supports up to %d", path, c.Version, CassetteVersion)
	}
	return &c, nil
}

// ReplayRules 把录制内容按原始顺序转换为一组接管规则：
// 每条规则 Times=1，按录制顺序各消费一次，回放结果与录制时一致。
func ReplayRules(c *Cassette) []Rule {
	rules := make([]Rule, 0, len(c.Entries))
	for i, e := range c.Entries {
		rules = append(rules, Rule{
			Name: "replay#" + itoa(i),
			Matcher: Matcher{
				Method:  e.Request.Method,
				Path:    e.Request.Path,
				Query:   e.Request.Query,
				Headers: e.Request.Headers,
				Body:    e.Request.Body,
			},
			Response: Response{
				Status:  e.Response.Status,
				Headers: e.Response.Headers,
				Body:    e.Response.Body,
			},
			Times: 1,
		})
	}
	return rules
}

// RecordTransport 包装真实 RoundTripper：把每次真实交互追加到录制内容。
// 录制与真实调用一一对应，顺序即发生顺序。
func RecordTransport(real http.RoundTripper, c *Cassette) http.RoundTripper {
	if real == nil {
		real = http.DefaultTransport
	}
	return &recordTransport{real: real, cassette: c}
}

type recordTransport struct {
	real     http.RoundTripper
	cassette *Cassette
	mu       sync.Mutex
}

func (t *recordTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var reqBody []byte
	if req.Body != nil {
		var err error
		reqBody, err = io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		req.Body = io.NopCloser(bytes.NewReader(reqBody))
	}
	resp, err := t.real.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	var respBody []byte
	if resp.Body != nil {
		respBody, err = io.ReadAll(resp.Body)
		if err != nil {
			return nil, err
		}
		resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))
	}
	entry := Entry{
		Request: RecordedRequest{
			Method:  req.Method,
			Path:    req.URL.Path,
			Query:   flattenQuery(req),
			Headers: firstValues(req.Header),
			Body:    reqBody,
		},
		Response: RecordedResponse{
			Status:  resp.StatusCode,
			Headers: firstValues(resp.Header),
			Body:    respBody,
		},
	}
	t.mu.Lock()
	t.cassette.Entries = append(t.cassette.Entries, entry)
	t.mu.Unlock()
	return resp, nil
}

func firstValues(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	out := make(map[string]string, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
