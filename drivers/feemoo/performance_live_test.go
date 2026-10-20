package feemoo

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
)

// No credentials or signed URLs enter the measurements or logs.
type measuredUploadTransport struct {
	inner              http.RoundTripper
	mu                 sync.Mutex
	active, peak, puts int
	bytes              int64
	first, last        time.Time
}

func (m *measuredUploadTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.Method != http.MethodPut {
		return m.inner.RoundTrip(r)
	}
	m.mu.Lock()
	if m.first.IsZero() {
		m.first = time.Now()
	}
	m.active++
	m.peak = max(m.peak, m.active)
	m.puts++
	m.bytes += r.ContentLength
	m.mu.Unlock()
	response, err := m.inner.RoundTrip(r)
	m.mu.Lock()
	m.active--
	m.last = time.Now()
	m.mu.Unlock()
	return response, err
}

func (m *measuredUploadTransport) CloseIdleConnections() {
	if inner, ok := m.inner.(interface{ CloseIdleConnections() }); ok {
		inner.CloseIdleConnections()
	}
}

func TestLiveMultipartConcurrency(t *testing.T) {
	sessionPath := os.Getenv("FEEMOO_LIVE_SESSION_FILE")
	if sessionPath == "" {
		t.Skip("set FEEMOO_LIVE_SESSION_FILE to opt in to two real 51 MiB uploads")
	}
	raw, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal("cannot read local live-test session file")
	}
	var session struct {
		Cookie    string `json:"cookie"`
		Par       string `json:"par"`
		Pto       string `json:"pto"`
		UserAgent string `json:"user-agent"`
	}
	if json.Unmarshal(raw, &session) != nil {
		t.Fatal("invalid local session JSON")
	}
	d := &Feemoo{Addition: Addition{Cookie: session.Cookie, Par: session.Par, Pto: session.Pto, UserAgent: session.UserAgent, PartRetries: 2, TempDir: t.TempDir()}}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if err := d.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer d.Drop(context.Background())
	allowed := d.partConcurrency
	folderName := "OpenList_parallel_" + time.Now().UTC().Format("20060102_150405")
	folder, err := d.MakeDir(ctx, root(), folderName)
	if err != nil {
		t.Fatal(err)
	}
	transport := d.client.Transport
	for _, parallel := range []int{1, allowed} {
		file, err := os.CreateTemp(t.TempDir(), "random-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = file.Close() })
		const size = int64(51 << 20)
		if _, err := io.CopyN(file, rand.Reader, size); err != nil {
			t.Fatal(err)
		}
		// New random contents in each run prevent a deduplication hit from being
		// mistaken for improved network throughput.
		measurement := &measuredUploadTransport{inner: transport}
		d.client.Transport, d.partConcurrency = measurement, parallel
		name := fmt.Sprintf("random_%d_parts_%d.bin", parallel, time.Now().UnixNano())
		source := &stream.FileStream{Ctx: ctx, Obj: &model.Object{Name: name, Size: size}, Reader: file}
		started := time.Now()
		obj, err := d.Put(ctx, folder, source, nil)
		if err != nil {
			t.Fatal(err)
		}
		if obj.GetSize() != size || obj.GetName() != name {
			t.Fatal("remote result name/size mismatch")
		}
		measurement.mu.Lock()
		if measurement.bytes < size || measurement.puts == 0 {
			t.Fatal("no real content upload observed")
		}
		t.Logf("REAL_MULTIPART_VERIFIED folder=%s workers=%d account_workers=%d peak_active_puts=%d puts=%d bytes=%d transfer_seconds=%.3f end_to_end_seconds=%.3f; refreshed remote name/size verified",
			folderName, parallel, allowed, measurement.peak, measurement.puts, measurement.bytes, measurement.last.Sub(measurement.first).Seconds(), time.Since(started).Seconds())
		measurement.mu.Unlock()
	}
}
