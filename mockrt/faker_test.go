package mockrt

import (
	"bytes"
	"fmt"
	"testing"
)

func TestFakerDeterministicAcrossCalls(t *testing.T) {
	f1 := NewFaker(42)
	f2 := NewFaker(42)
	for i := 0; i < 100; i++ {
		path := fmt.Sprintf("/field/%d", i)
		if f1.Value(path) != f2.Value(path) {
			t.Fatalf("Value mismatch at %s", path)
		}
		if f1.Int(path, -1000, 1000) != f2.Int(path, -1000, 1000) {
			t.Fatalf("Int mismatch at %s", path)
		}
		if !bytes.Equal(f1.Bytes(path, 32), f2.Bytes(path, 32)) {
			t.Fatalf("Bytes mismatch at %s", path)
		}
		if f1.UUID(path) != f2.UUID(path) {
			t.Fatalf("UUID mismatch at %s", path)
		}
	}
}

func TestFakerSeedChangesOutput(t *testing.T) {
	a := NewFaker(1).Value("/x")
	b := NewFaker(2).Value("/x")
	if a == b {
		t.Fatal("different seeds should produce different values")
	}
}

func TestFakerRepeatSameInstance(t *testing.T) {
	f := NewFaker(7)
	first := f.Value("/stable")
	for i := 0; i < 50; i++ {
		if got := f.Value("/stable"); got != first {
			t.Fatalf("call %d: got %q, want %q", i, got, first)
		}
	}
}

func TestFakerFillTraversalOrderIndependent(t *testing.T) {
	def := map[string]any{
		"name":  "{{name}}",
		"email": "{{email}}",
		"id":    "{{uuid}}",
		"age":   "{{int:18:80}}",
		"tag":   "{{string:8}}",
		"nested": map[string]any{
			"a": "{{int:0:9}}",
			"b": "{{string:4}}",
			"c": "{{bytes:4}}",
		},
		"list": []any{"{{int:0:100}}", "{{string:2}}"},
	}
	want := NewFaker(99).Fill(def)
	// 多次填充（每次重新构造 map，遍历顺序由 Go 随机化）结果必须逐字节一致。
	for i := 0; i < 50; i++ {
		fresh := map[string]any{
			"tag":   "{{string:8}}",
			"age":   "{{int:18:80}}",
			"id":    "{{uuid}}",
			"email": "{{email}}",
			"name":  "{{name}}",
			"nested": map[string]any{
				"c": "{{bytes:4}}",
				"b": "{{string:4}}",
				"a": "{{int:0:9}}",
			},
			"list": []any{"{{int:0:100}}", "{{string:2}}"},
		}
		got := NewFaker(99).Fill(fresh)
		if fmt.Sprintf("%#v", got) != fmt.Sprintf("%#v", want) {
			t.Fatalf("iteration %d: fill result differs", i)
		}
	}
}

func TestFakerIntRange(t *testing.T) {
	f := NewFaker(3)
	for i := 0; i < 1000; i++ {
		v := f.Int(fmt.Sprintf("/r/%d", i), 5, 9)
		if v < 5 || v > 9 {
			t.Fatalf("Int out of range: %d", v)
		}
	}
	if f.Int("/const", 7, 7) != 7 {
		t.Fatal("degenerate range should return min")
	}
}
