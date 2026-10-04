package mockrt

import (
	"encoding/hex"
	"fmt"
	"time"
)

// defaultCharset 是 string 类型字段的默认字符集（固定顺序，保证确定性）。
const defaultCharset = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"

// Faker 生成确定性伪造数据。同种子、同 key 在任意进程与任意
// 遍历顺序下产生逐字节一致的结果；不读取系统时间、环境变量
// 或运行时随机源。
type Faker struct {
	seed uint64
}

// NewFaker 以给定种子创建 Faker。
func NewFaker(seed uint64) *Faker {
	return &Faker{seed: seed}
}

// Field 返回以 key 派生的独立字段生成器。
// 派生只依赖 (seed, key)，与调用顺序无关。
func (f *Faker) Field(key string) *FieldGen {
	h := fnv1a64([]byte(key))
	// 用 splitmix64 终末混合把种子与 key 散列充分打散。
	state := mix64(f.seed + mix64(h))
	return &FieldGen{state: state}
}

// FieldGen 是单个字段的确定性随机流（splitmix64）。
type FieldGen struct {
	state uint64
}

// Uint64 返回流中下一个值。
func (g *FieldGen) Uint64() uint64 {
	g.state += 0x9E3779B97F4A7C15
	return mix64(g.state)
}

func mix64(z uint64) uint64 {
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func fnv1a64(b []byte) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for _, c := range b {
		h ^= uint64(c)
		h *= prime
	}
	return h
}

// Intn 返回 [0, n) 内的确定值，n<=0 时返回 0。
// 使用拒绝采样避免取模偏差，且与平台无关。
func (g *FieldGen) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(g.Uint64n(uint64(n)))
}

// Uint64n 返回 [0, n) 内的确定值。
func (g *FieldGen) Uint64n(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	// 拒绝采样，保证均匀且跨平台一致。
	limit := ^uint64(0) - (^uint64(0) % n)
	for {
		v := g.Uint64()
		if v < limit {
			return v % n
		}
	}
}

// Bool 返回确定布尔值。
func (g *FieldGen) Bool() bool { return g.Uint64()&1 == 1 }

// String 返回长度为 n 的确定字符串。
func (g *FieldGen) String(n int, charset string) string {
	if charset == "" {
		charset = defaultCharset
	}
	if n <= 0 {
		return ""
	}
	out := make([]byte, n)
	for i := range out {
		out[i] = charset[g.Uint64n(uint64(len(charset)))]
	}
	return string(out)
}

// Bytes 返回长度为 n 的确定字节串。
func (g *FieldGen) Bytes(n int) []byte {
	if n <= 0 {
		return nil
	}
	out := make([]byte, n)
	for i := 0; i < n; i += 8 {
		v := g.Uint64()
		for j := 0; j < 8 && i+j < n; j++ {
			out[i+j] = byte(v >> (8 * j))
		}
	}
	return out
}

// UUID 返回确定的 RFC 4122 v4 形式 UUID 字符串。
func (g *FieldGen) UUID() string {
	b := g.Bytes(16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	var buf [36]byte
	hex.Encode(buf[0:8], b[0:4])
	buf[8] = '-'
	hex.Encode(buf[9:13], b[4:6])
	buf[13] = '-'
	hex.Encode(buf[14:18], b[6:8])
	buf[18] = '-'
	hex.Encode(buf[19:23], b[8:10])
	buf[23] = '-'
	hex.Encode(buf[24:36], b[10:16])
	return string(buf[:])
}

// epochBase 是 time 字段的固定基准，避免依赖系统时间。
var epochBase = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// Time 返回确定的 UTC 时间：固定基准加上 [0, maxSec) 秒偏移。
func (g *FieldGen) Time(maxSec uint64) time.Time {
	if maxSec == 0 {
		maxSec = 100 * 365 * 24 * 3600
	}
	return epochBase.Add(time.Duration(g.Uint64n(maxSec)) * time.Second)
}

// Generate 按 FakeSpec 生成确定性字段值。返回值类型：
//
//	int -> int64, uint -> uint64, string -> string, bool -> bool,
//	uuid -> string, time -> string(RFC3339), choice -> string, bytes -> []byte
func Generate(spec FakeSpec, seed uint64, key string) any {
	g := NewFaker(seed).Field(key)
	switch spec.Kind {
	case "int":
		lo, hi := spec.Min, spec.Max
		if hi < lo {
			hi = lo
		}
		return lo + int64(g.Uint64n(uint64(hi-lo+1)))
	case "uint":
		lo, hi := uint64(spec.Min), uint64(spec.Max)
		if hi < lo {
			hi = lo
		}
		return lo + g.Uint64n(hi-lo+1)
	case "string":
		n := spec.Len
		if n <= 0 {
			n = 8
		}
		return g.String(n, spec.Charset)
	case "bool":
		return g.Bool()
	case "uuid":
		return g.UUID()
	case "time":
		return g.Time(0).Format(time.RFC3339)
	case "choice":
		if len(spec.Choices) == 0 {
			return ""
		}
		return spec.Choices[g.Uint64n(uint64(len(spec.Choices)))]
	case "bytes":
		return g.Bytes(spec.Len)
	default:
		panic(fmt.Sprintf("mockrt: unknown fake kind %q", spec.Kind))
	}
}
