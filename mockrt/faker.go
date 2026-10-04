package mockrt

import (
	"encoding/binary"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Faker 是确定性伪造数据生成器：同一定义配合同一种子，
// 任意次数、任意进程、任意遍历顺序下生成逐字节一致的结果。
// 实现只依赖 (seed, 字段路径) 的纯函数，不读取系统时间、
// 环境变量或运行时随机源；遍历 map 时按键排序，避免 Go 的
// 随机遍历顺序影响输出。
type Faker struct {
	seed int64
}

// NewFaker 以种子创建 Faker。
func NewFaker(seed int64) *Faker { return &Faker{seed: seed} }

// splitmix64 是确定性的 64 位混合函数。
func splitmix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	x = (x ^ (x >> 30)) * 0xbf58476d1ce4e5b9
	x = (x ^ (x >> 27)) * 0x94d049bb133111eb
	return x ^ (x >> 31)
}

// fnv1a 计算字符串的 FNV-1a 64 位散列（与平台无关）。
func fnv1a(s string) uint64 {
	const (
		offset = 14695981039346656037
		prime  = 1099511628211
	)
	h := uint64(offset)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= prime
	}
	return h
}

// keyed 返回 (seed, path) 派生的确定性随机流起始状态。
func (f *Faker) keyed(path string) uint64 {
	return splitmix64(uint64(f.seed) ^ splitmix64(fnv1a(path)))
}

// next 推进状态并返回新值。
func next(state *uint64) uint64 {
	*state = splitmix64(*state)
	return *state
}

const letterSet = "abcdefghijklmnopqrstuvwxyz"

// Value 按字段路径生成确定性字符串值（16 个小写字母）。
func (f *Faker) Value(path string) string { return f.StringN(path, 16) }

// StringN 生成 n 个字符的确定性小写字符串。
func (f *Faker) StringN(path string, n int) string {
	state := f.keyed(path)
	var b strings.Builder
	b.Grow(n)
	for b.Len() < n {
		v := next(&state)
		for i := 0; i < 8 && b.Len() < n; i++ {
			b.WriteByte(letterSet[v%26])
			v /= 26
		}
	}
	return b.String()
}

// Int 生成范围 [min,max] 内的确定性整数。
func (f *Faker) Int(path string, min, max int64) int64 {
	if max <= min {
		return min
	}
	state := f.keyed(path)
	return min + int64(next(&state)%uint64(max-min+1))
}

// Bytes 生成 n 字节的确定性字节串。
func (f *Faker) Bytes(path string, n int) []byte {
	state := f.keyed(path)
	out := make([]byte, n)
	for i := 0; i < n; i += 8 {
		v := next(&state)
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], v)
		copy(out[i:], buf[:])
	}
	return out
}

// Name 生成确定性的伪造人名。
func (f *Faker) Name(path string) string {
	first := []string{"Alice", "Bob", "Carol", "David", "Erin", "Frank", "Grace", "Heidi"}
	last := []string{"Smith", "Jones", "Brown", "Taylor", "Wilson", "Davies", "Evans", "Thomas"}
	return first[f.Int(path+"/first", 0, int64(len(first)-1))] + " " +
		last[f.Int(path+"/last", 0, int64(len(last)-1))]
}

// Email 生成确定性的伪造邮箱。
func (f *Faker) Email(path string) string {
	return f.StringN(path+"/local", 10) + "@example.com"
}

// UUID 生成确定性的伪 UUID（版本 4 格式，但完全由种子决定）。
func (f *Faker) UUID(path string) string {
	b := f.Bytes(path, 16)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// Fill 按字段路径递归填充定义中的占位符，返回新 map（不改入参）。
// 支持的占位符：
//
//	{{string}} / {{string:N}}  确定性字符串（默认 16 字符）
//	{{int:MIN:MAX}}            范围整数
//	{{name}}                   人名
//	{{email}}                  邮箱
//	{{uuid}}                   伪 UUID
//	{{bytes:N}}                N 字节（以十六进制字符串返回）
//
// 嵌套 map 与切片按排序后的键/下标展开为字段路径，保证遍历顺序无关。
func (f *Faker) Fill(def map[string]any) map[string]any {
	out, _ := f.fill(def, "").(map[string]any)
	return out
}

func (f *Faker) fill(v any, path string) any {
	switch t := v.(type) {
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		out := make(map[string]any, len(t))
		for _, k := range keys {
			out[k] = f.fill(t[k], path+"/"+k)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = f.fill(e, path+"/"+strconv.Itoa(i))
		}
		return out
	case string:
		return f.expand(t, path)
	default:
		return v
	}
}

// expand 展开单个占位符字符串；非占位符原样返回。
func (f *Faker) expand(s, path string) any {
	if !strings.HasPrefix(s, "{{") || !strings.HasSuffix(s, "}}") {
		return s
	}
	inner := s[2 : len(s)-2]
	parts := strings.Split(inner, ":")
	switch parts[0] {
	case "string":
		n := 16
		if len(parts) == 2 {
			if v, err := strconv.Atoi(parts[1]); err == nil {
				n = v
			}
		}
		return f.StringN(path, n)
	case "int":
		if len(parts) == 3 {
			lo, err1 := strconv.ParseInt(parts[1], 10, 64)
			hi, err2 := strconv.ParseInt(parts[2], 10, 64)
			if err1 == nil && err2 == nil {
				return f.Int(path, lo, hi)
			}
		}
		return f.Int(path, 0, 1<<31-1)
	case "name":
		return f.Name(path)
	case "email":
		return f.Email(path)
	case "uuid":
		return f.UUID(path)
	case "bytes":
		n := 8
		if len(parts) == 2 {
			if v, err := strconv.Atoi(parts[1]); err == nil {
				n = v
			}
		}
		return fmt.Sprintf("%x", f.Bytes(path, n))
	default:
		return s
	}
}
