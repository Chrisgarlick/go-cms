package web

import (
	"fmt"
	"io"
	"sync/atomic"
	"time"
)

// Minimal request metrics without a Prometheus client dependency.
var (
	reqTotal   [6]atomic.Int64 // by status class 0..5 (1xx..5xx)
	latencyBkt = []time.Duration{5 * time.Millisecond, 20 * time.Millisecond, 50 * time.Millisecond, 100 * time.Millisecond,
		300 * time.Millisecond, time.Second, 5 * time.Second}
	latencyCnt [8]atomic.Int64
	latencySum atomic.Int64 // microseconds
)

func observe(status int, d time.Duration) {
	c := status / 100
	if c < 0 || c > 5 {
		c = 0
	}
	reqTotal[c].Add(1)
	i := 0
	for i < len(latencyBkt) && d > latencyBkt[i] {
		i++
	}
	latencyCnt[i].Add(1)
	latencySum.Add(d.Microseconds())
}

// WriteRequestMetrics writes Prometheus text for HTTP requests handled by Go.
func WriteRequestMetrics(w io.Writer) {
	fmt.Fprintf(w, "# TYPE cms_http_requests_total counter\n")
	for c := 1; c <= 5; c++ {
		fmt.Fprintf(w, "cms_http_requests_total{class=\"%dxx\"} %d\n", c, reqTotal[c].Load())
	}
	fmt.Fprintf(w, "# TYPE cms_http_request_duration_seconds histogram\n")
	var cum int64
	for i, b := range latencyBkt {
		cum += latencyCnt[i].Load()
		fmt.Fprintf(w, "cms_http_request_duration_seconds_bucket{le=\"%g\"} %d\n", b.Seconds(), cum)
	}
	cum += latencyCnt[len(latencyBkt)].Load()
	fmt.Fprintf(w, "cms_http_request_duration_seconds_bucket{le=\"+Inf\"} %d\n", cum)
	fmt.Fprintf(w, "cms_http_request_duration_seconds_sum %g\n", float64(latencySum.Load())/1e6)
	fmt.Fprintf(w, "cms_http_request_duration_seconds_count %d\n", cum)
}
