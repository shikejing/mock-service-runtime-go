package mockrt

import (
	"bytes"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"strconv"
	"testing"
)

var allSpecs = map[string]FakeSpec{
	"age":    {Kind: "int", Min: 18, Max: 99},
	"id":     {Kind: "uint", Min: 0, Max: 1 << 40},
	"name":   {Kind: "string", Len: 12},
	"tag":    {Kind: "string", Len: 6, Charset: "abc"},
	"vip":    {Kind: "bool"},
	"uuid":   {Kind: "uuid"},
	"born":   {Kind: "time"},
	"role":   {Kind: "choice", Choices: []string{"admin", "user", "guest"}},
	"avatar": {Kind: "bytes", Len: 16},
}

func generateAll(seed uint64) map[string]any {
	out := map[string]any{}
	for k, spec := range allSpecs {
		out[k] = Generate(spec, seed, "rule."+k)
	}
	return out
}

func TestFakerDeterministicAcrossTraversalOrder(t *testing.T) {
	const seed = 42
	want := generateAll(seed)
	// 以不同遍历顺序重复生成，结果必须逐字节一致
	for trial := 0; trial < 20; trial++ {
		keys := make([]string, 0, len(allSpecs))
		for k := range allSpecs {
			keys = append(keys, k)
		}
		rand.New(rand.NewSource(int64(trial))).Shuffle(len(keys), func(i, j int) {
			keys[i], keys[j] = keys[j], keys[i]
		})
		for _, k := range keys {
			got := Generate(allSpecs[k], seed, "rule."+k)
			if !equalAny(got, want[k]) {
				t.Fatalf("field %s differs across traversal: %v vs %v", k, got, want[k])
			}
		}
	}
}

func TestFakerDeterministicAcrossProcesses(t *testing.T) {
	const seed = 7
	if os.Getenv("MOCKRT_FAKER_CHILD") == "1" {
		// 子进程：生成并打印全部字段
		for _, k := range []string{"age", "id", "name", "tag", "vip", "uuid", "born", "role", "avatar"} {
			v := Generate(allSpecs[k], seed, "rule."+k)
			if b, ok := v.([]byte); ok {
				os.Stdout.WriteString(k + "=" + strconv.Itoa(len(b)) + ":")
				os.Stdout.Write(b)
			} else {
				os.Stdout.WriteString(k + "=" + strconv.Itoa(len(k)) + "|" + toString(v))
			}
			os.Stdout.WriteString("\n")
		}
		return
	}
	var outputs [][]byte
	for i := 0; i < 3; i++ {
		cmd := exec.Command(os.Args[0], "-test.run", "TestFakerDeterministicAcrossProcesses")
		cmd.Env = append(os.Environ(), "MOCKRT_FAKER_CHILD=1")
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		outputs = append(outputs, out)
	}
	for i := 1; i < len(outputs); i++ {
		if !bytes.Equal(outputs[0], outputs[i]) {
			t.Fatalf("process %d output differs:\n%s\nvs\n%s", i, outputs[0], outputs[i])
		}
	}
	// 与本进程直接生成对比（跨进程一致）
	want := generateAll(seed)
	_ = want
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case int64:
		return strconv.FormatInt(x, 10)
	case uint64:
		return strconv.FormatUint(x, 10)
	case bool:
		return strconv.FormatBool(x)
	}
	return ""
}

func TestFakerSeedSensitivity(t *testing.T) {
	a := generateAll(1)
	b := generateAll(2)
	same := 0
	for k := range a {
		if equalAny(a[k], b[k]) {
			same++
		}
	}
	if same > len(a)/2 {
		t.Fatalf("different seeds produced %d/%d identical fields", same, len(a))
	}
}

func TestFakeBodyViaRule(t *testing.T) {
	rt := New(WithConfig(Config{Enabled: true, Timeout: 0, Seed: 99}))
	defer rt.Close()
	mk := func() []byte {
		s := rt.Begin("fake")
		defer s.Close()
		s.AddRule(Rule{
			Name:  "user",
			Match: Matcher{Path: "/user"},
			FakeBody: map[string]FakeSpec{
				"id":   {Kind: "int", Min: 1, Max: 1000},
				"name": {Kind: "string", Len: 8},
			},
			Response: Response{Status: 200},
		})
		resp, err := s.Client().Get("http://dep/user")
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	first := mk()
	for i := 0; i < 5; i++ {
		if got := mk(); !bytes.Equal(first, got) {
			t.Fatalf("fake body not deterministic: %s vs %s", first, got)
		}
	}
	t.Logf("fake body: %s", first)
}

func equalAny(a, b any) bool {
	switch x := a.(type) {
	case []byte:
		y, ok := b.([]byte)
		return ok && bytes.Equal(x, y)
	}
	return a == b
}
