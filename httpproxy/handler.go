package httpproxy

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"syscall"

	"github.com/phuslu/glog"

	"./filters"
	"./helpers"
)

type Handler struct {
	http.Handler
	Listener         helpers.Listener
	RequestFilters   []filters.RequestFilter
	RoundTripFilters []filters.RoundTripFilter
	ResponseFilters  []filters.ResponseFilter
}

func (h Handler) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	var err error

	remoteAddr := req.RemoteAddr

	// Prepare filter.Context
	ctx := filters.NewContext(req.Context(), h, h.Listener, rw)
	req = req.WithContext(ctx)

	// Enable transport http proxy
	if req.Method != "CONNECT" && !req.URL.IsAbs() {
		if req.URL.Scheme == "" {
			if req.TLS != nil && req.ProtoMajor == 1 {
				req.URL.Scheme = "https"
			} else {
				req.URL.Scheme = "http"
			}
		}

		if req.TLS != nil {
			if req.Host == "" {
				if req.URL.Host != "" {
					req.Host = req.URL.Host
				} else {
					req.Host = req.TLS.ServerName
				}
			}
			if req.URL.Host == "" {
				if req.Host != "" {
					req.URL.Host = req.Host
				} else {
					req.URL.Host = req.TLS.ServerName
				}
			}
		}
	}

	// Filter Request
	for _, f := range h.RequestFilters {
		ctx, req, err = f.Request(ctx, req)
		// A roundtrip filter hijacked
		if filters.GetHijacked(ctx) {
			return
		}
		if err != nil {
			if err != io.EOF {
				glog.Errorf("%s Filter Request %T error: %#v", remoteAddr, f, err)
			}
			return
		}
		// Update context for request
		req = req.WithContext(ctx)
	}

	if req.Body != nil {
		defer req.Body.Close()
	}

	// Filter Request -> Response
	var resp *http.Response
	for _, f := range h.RoundTripFilters {
		ctx, resp, err = f.RoundTrip(ctx, req)
		// A roundtrip filter hijacked
		if filters.GetHijacked(ctx) {
			return
		}
		// Unexcepted errors
		if err != nil {
			filters.SetRoundTripFilter(ctx, f)
			glog.Errorf("%s Filter RoundTrip %T error: %v", remoteAddr, f, err)
			http.Error(rw, fmtError(ctx, err), http.StatusBadGateway)
			return
		}
		// Update context for request
		req = req.WithContext(ctx)
		// A roundtrip filter give a response
		if resp != nil {
			resp.Request = req
			filters.SetRoundTripFilter(ctx, f)
			break
		}
	}

	// Filter Response
	for _, f := range h.ResponseFilters {
		if resp == nil {
			return
		}
		ctx, resp, err = f.Response(ctx, resp)
		if err != nil {
			glog.Errorln("%s Filter %T Response error: %v", remoteAddr, f, err)
			http.Error(rw, fmtError(ctx, err), http.StatusBadGateway)
			return
		}
		// Update context for request
		req = req.WithContext(ctx)
	}

	if resp == nil {
		glog.Errorln("%s Handler %#v Response empty response", remoteAddr, h)
		http.Error(rw, fmtError(ctx, fmt.Errorf("empty response")), http.StatusBadGateway)
		return
	}

	for key, values := range resp.Header {
		for _, value := range values {
			rw.Header().Add(key, value)
		}
	}
	rw.WriteHeader(resp.StatusCode)
	if resp.Body != nil {
		defer resp.Body.Close()
		n, err := helpers.IoCopy(rw, resp.Body)
		if err != nil {
			if isClosedConnError(err) {
				glog.Infof("IoCopy %#v return %#v %T(%v)", resp.Body, n, err, err)
			} else {
				glog.Warningf("IoCopy %#v return %#v %T(%v)", resp.Body, n, err, err)
			}
		}
	}
}

func fmtError(ctx context.Context, err error) string {
	return fmt.Sprintf(`{
    "type": "localproxy",
    "host": "%s",
    "software": "go/%s %s/%s",
    "filter": "%T",
    "error": "%s"
}
`, filters.GetListener(ctx).Addr().String(),
		runtime.Version(), runtime.GOOS, runtime.GOARCH,
		filters.GetRoundTripFilter(ctx),
		err.Error())
}

func isClosedConnError(err error) bool {
	if err == nil {
		return false
	}

	str := err.Error()
	if strings.Contains(str, "use of closed network connection") {
		return true
	}

	if runtime.GOOS == "windows" {
		const WSAECONNABORTED = 10053
		const WSAECONNRESET = 10054
		if oe, ok := err.(*net.OpError); ok && (oe.Op == "read" || oe.Op == "write") {
			if se, ok := oe.Err.(*os.SyscallError); ok && (se.Syscall == "wsarecv" || se.Syscall == "wsasend") {
				if n, ok := se.Err.(syscall.Errno); ok {
					if n == WSAECONNRESET || n == WSAECONNABORTED {
						return true
					}
				}
			}
		}
	}
	return false
}
