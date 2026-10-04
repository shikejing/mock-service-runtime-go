package mockrt

import (
	"errors"
	"io"
	"net"
	"net/http"
	"syscall"
	"time"
)

// errConnReset 是连接中断故障返回的错误，可用 errors.Is 识别。
var errConnReset = &net.OpError{
	Op:  "read",
	Net: "tcp",
	Err: syscall.ECONNRESET,
}

// IsConnReset 判断错误是否为由故障注入产生的连接中断。
func IsConnReset(err error) bool {
	return errors.Is(err, syscall.ECONNRESET)
}

// applyFaultBefore 处理调用前的故障（延迟/错误/连接重置）。
// timeout 在用例配置的超时到期时关闭；延迟被超时截断时返回超时错误。
// 返回非 nil 错误时调用直接失败。
func applyFaultBefore(f *Fault, timeout <-chan struct{}) error {
	if f == nil {
		return nil
	}
	if f.Delay > 0 {
		t := time.NewTimer(f.Delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-timeout:
			return &net.OpError{Op: "mockrt", Net: "mock", Err: errTimeout}
		}
	}
	if f.Err != nil {
		return f.Err
	}
	if f.Reset {
		return errConnReset
	}
	return nil
}

var errTimeout = errors.New("mockrt: rule delay exceeded scope timeout")

// wrapBodyAbort 包装响应体，使其在写出指定字节后中断。
// abortAfter < 0 时原样返回。
func wrapBodyAbort(resp *http.Response, abortAfter int) *http.Response {
	if abortAfter < 0 || resp == nil || resp.Body == nil {
		return resp
	}
	resp.Body = &abortReader{inner: resp.Body, remain: abortAfter}
	return resp
}

// abortReader 在成功返回 remain 字节后，下一次读取返回连接重置错误。
type abortReader struct {
	inner  io.ReadCloser
	remain int
}

func (a *abortReader) Read(p []byte) (int, error) {
	if a.remain <= 0 {
		a.inner.Close()
		return 0, errConnReset
	}
	if len(p) > a.remain {
		p = p[:a.remain]
	}
	n, err := a.inner.Read(p)
	a.remain -= n
	if err == io.EOF && a.remain > 0 {
		// 底层数据不足即结束，视为正常 EOF。
		return n, io.EOF
	}
	return n, nil
}

func (a *abortReader) Close() error { return a.inner.Close() }
