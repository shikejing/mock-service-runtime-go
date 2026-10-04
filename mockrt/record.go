package mockrt

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// CassetteMode 录制/回放模式。
type CassetteMode int

const (
	ModeReplay CassetteMode = iota // 只回放，未命中报错
	ModeRecord                     // 只录制，放行到真实依赖
	ModeAuto                       // 命中回放，未命中放行并录制
)

// cassetteEntry 是一条录制条目。
type cassetteEntry struct {
	Key     string              `json:"key"`
	Request Request             `json:"request"`
	Status  int                 `json:"status"`
	Header  map[string][]string `json:"header,omitempty"`
	Body    []byte              `json:"body,omitempty"`
}

func (e *cassetteEntry) toResponse() *Response {
	return &Response{Status: e.Status, Header: e.Header, Body: e.Body}
}

// cassetteFile 是磁带的磁盘格式。
type cassetteFile struct {
	Version int             `json:"version"`
	Entries []cassetteEntry `json:"entries"`
}

// Cassette 是录制内容的集合。
// 新增条目以追加方式合并：既有条目永不改写，
// 因此补充录制不会破坏已提交用例的回放结果。
type Cassette struct {
	mu       sync.Mutex
	path     string
	entries  map[string][]cassetteEntry // key -> 按录制顺序
	replayIx map[string]int             // key -> 回放消费进度
	pending  []cassetteEntry            // 待保存的新条目
}

// LoadCassette 加载磁带文件，不存在时返回空磁带。
func LoadCassette(path string) (*Cassette, error) {
	c := &Cassette{
		path:     path,
		entries:  map[string][]cassetteEntry{},
		replayIx: map[string]int{},
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var f cassetteFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("mockrt: load cassette %s: %w", path, err)
	}
	for _, e := range f.Entries {
		if e.Key == "" {
			e.Key = requestKey(e.Request)
		}
		c.entries[e.Key] = append(c.entries[e.Key], e)
	}
	return c, nil
}

// Save 将新增条目追加合并写回文件，不改动既有条目。
func (c *Cassette) Save() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.pending) == 0 {
		return nil
	}
	var f cassetteFile
	data, err := os.ReadFile(c.path)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &f); err != nil {
			return fmt.Errorf("mockrt: merge cassette %s: %w", c.path, err)
		}
	case errors.Is(err, os.ErrNotExist):
		// 新文件
	default:
		return err
	}
	if f.Version == 0 {
		f.Version = 1
	}
	f.Entries = append(f.Entries, c.pending...)
	out, err := json.MarshalIndent(&f, "", "  ")
	if err != nil {
		return err
	}
	c.pending = nil
	return os.WriteFile(c.path, out, 0o644)
}

// lookup 按请求查找回放条目；同一 key 的多条记录按录制顺序消费。
func (c *Cassette) lookup(req Request) (*cassetteEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	key := requestKey(req)
	list := c.entries[key]
	ix := c.replayIx[key]
	if ix >= len(list) {
		return nil, false
	}
	c.replayIx[key] = ix + 1
	e := list[ix]
	return &e, true
}

// record 追加一条录制条目。
func (c *Cassette) record(e cassetteEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e.Key = requestKey(e.Request)
	c.entries[e.Key] = append(c.entries[e.Key], e)
	c.pending = append(c.pending, e)
}

// requestKey 计算请求的规范化摘要：
// 方法 + 路径 + 排序后的查询参数 + 请求体。
// 不含易变的请求头（如 Date/User-Agent），保证回放稳定。
func requestKey(r Request) string {
	var b strings.Builder
	b.WriteString(strings.ToUpper(r.Method))
	b.WriteByte(' ')
	b.WriteString(r.Path)
	keys := make([]string, 0, len(r.Query))
	for k := range r.Query {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		vs := append([]string(nil), r.Query[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			b.WriteByte('\n')
			b.WriteString(k)
			b.WriteByte('=')
			b.WriteString(v)
		}
	}
	b.WriteString("\n\n")
	b.WriteString(base64.StdEncoding.EncodeToString(r.Body))
	return fmt.Sprintf("%016x", fnv1a64([]byte(b.String())))
}

// UseCassette 让用例进入录制/回放模式。
func (s *Scope) UseCassette(c *Cassette, mode CassetteMode) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cassette = c
	s.mode = mode
}
