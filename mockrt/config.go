package mockrt

import "sync/atomic"

// configHolder 以原子快照方式保存配置，运行期更新不影响进行中的用例。
type configHolder struct {
	v atomic.Value // Config
}

func newConfigHolder(c Config) *configHolder {
	h := &configHolder{}
	h.v.Store(c)
	return h
}

func (h *configHolder) get() Config  { return h.v.Load().(Config) }
func (h *configHolder) set(c Config) { h.v.Store(c) }
