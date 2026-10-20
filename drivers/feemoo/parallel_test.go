package feemoo

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"os"
	"path"
	"sync"
	"testing"
	"time"
)

func TestAccountPartConcurrencyLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed scalar
		setting int
		want    int
	}{
		{"missing", "", 0, 1}, {"zero", "0", 0, 1},
		{"invalid", "bad", 0, 1}, {"negative", "-2", 0, 1},
		{"official_auto", "5", 0, 5}, {"manual_lower", "5", 2, 2},
		{"respect_permission", "2", 8, 2}, {"local_bound", "100", 0, 8},
		{"fallback_with_manual", "", 5, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, m := newMock(t)
			m.maxPart, d.PartConcurrency = tc.allowed, tc.setting
			if err := d.authenticate(context.Background()); err != nil {
				t.Fatal(err)
			}
			if d.partConcurrency != tc.want {
				t.Fatalf("concurrency=%d, want %d", d.partConcurrency, tc.want)
			}
		})
	}
	for _, invalid := range []int{-1, 9} {
		d := &Feemoo{Addition: Addition{PartConcurrency: invalid}}
		if err := d.Init(context.Background()); err == nil {
			t.Fatalf("accepted invalid concurrency %d", invalid)
		}
	}
}

func TestParallelPartsBoundProgressAndRetry(t *testing.T) {
	d, m := newMock(t)
	m.maxPart = "3"
	if err := d.authenticate(context.Background()); err != nil {
		t.Fatal(err)
	}
	m.failParts = 1
	var mu sync.Mutex
	active, peak := 0, 0
	ready, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	m.onPut = func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		active++
		peak = max(peak, active)
		if active == 3 {
			once.Do(func() { close(ready) })
		}
		mu.Unlock()
		select {
		case <-release:
		case <-r.Context().Done():
		}
		mu.Lock()
		active--
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	defer unblock()
	data := bytes.Repeat([]byte{0x63}, 6*65536+123)
	var progress []float64
	result := make(chan error, 1)
	go func() {
		_, err := d.Put(ctx, root(), source("parallel.bin", data, false), func(p float64) { progress = append(progress, p) })
		result <- err
	}()
	select {
	case <-ready:
	case <-ctx.Done():
		t.Fatal("three part uploads did not overlap")
	}
	// Three successful PUTs are held open, so no fourth worker may enter.
	m.mu.Lock()
	puts := m.putCalls
	m.mu.Unlock()
	if puts != 4 { // three active PUTs and one retried 503
		t.Fatalf("part worker bound violated: %d PUTs before release", puts)
	}
	unblock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	if peak != 3 || active != 0 {
		t.Fatalf("unexpected active workers: peak=%d active=%d", peak, active)
	}
	mu.Unlock()
	for i, p := range progress {
		if p < 0 || p > 100 || i > 0 && p < progress[i-1] {
			t.Fatalf("progress regressed or over-counted a retry: %v", progress)
		}
	}
	if len(progress) == 0 || progress[len(progress)-1] != 100 {
		t.Fatal("verified completion missing")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.putCalls != 8 || m.initCalls != 1 || m.cancelCalls != 0 {
		t.Fatalf("unexpected retry/session lifecycle: PUT=%d init=%d cancel=%d", m.putCalls, m.initCalls, m.cancelCalls)
	}
	for _, task := range m.tasks {
		var uploaded []byte
		for i := 1; i <= len(task.parts); i++ {
			uploaded = append(uploaded, task.parts[i]...)
		}
		if !bytes.Equal(uploaded, data) {
			t.Fatal("out-of-order upload corrupted content")
		}
	}
}

func TestParallelFailureAndCancellationCleanUp(t *testing.T) {
	for _, userCancel := range []bool{false, true} {
		t.Run(map[bool]string{false: "part_failure", true: "user_cancel"}[userCancel], func(t *testing.T) {
			d, m := newMock(t)
			m.maxPart = "3"
			if err := d.authenticate(context.Background()); err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			active := 0
			ready, finished := make(chan struct{}), make(chan struct{}, 3)
			m.onPut = func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				active++
				if active == 3 {
					close(ready)
				}
				mu.Unlock()
				if !userCancel && path.Base(r.URL.Path) == "1" {
					<-ready
					w.WriteHeader(http.StatusForbidden)
				} else {
					<-r.Context().Done()
				}
				finished <- struct{}{}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result := make(chan error, 1)
			lastProgress := float64(0)
			go func() {
				_, err := d.Put(ctx, root(), source("failure.bin", bytes.Repeat([]byte("x"), 6*65536), false), func(p float64) { lastProgress = p })
				result <- err
			}()
			select {
			case <-ready:
			case <-ctx.Done():
				t.Fatal("parallel PUTs did not start")
			}
			if userCancel {
				cancel()
			}
			select {
			case err := <-result:
				if err == nil || userCancel && !errors.Is(err, context.Canceled) {
					t.Fatalf("unexpected failure result: %v", err)
				}
				if !userCancel {
					var status *httpError
					if !errors.As(err, &status) || status.code != http.StatusForbidden {
						t.Fatalf("lost original part failure: %v", err)
					}
				}
			case <-time.After(5 * time.Second):
				t.Fatal("parallel cancellation did not finish")
			}
			for range 3 {
				select {
				case <-finished:
				case <-time.After(5 * time.Second):
					t.Fatal("sibling PUT was left running")
				}
			}
			if lastProgress == 100 {
				t.Fatal("failed upload reported success")
			}
			m.mu.Lock()
			if m.cancelCalls != 1 || len(m.items["0"]) != 0 || m.putCalls != 3 {
				t.Errorf("unexpected failed session lifecycle: cancel=%d files=%d PUT=%d", m.cancelCalls, len(m.items["0"]), m.putCalls)
			}
			m.mu.Unlock()
			entries, _ := os.ReadDir(d.TempDir)
			if len(entries) != 0 {
				t.Fatal("parallel failure leaked a temporary file")
			}
		})
	}
}

func TestProgressCountsEachPartOnce(t *testing.T) {
	var values []float64
	p := &uploadProgress{sent: make([]int64, 3), total: 30, up: func(v float64) { values = append(values, v) }}
	p.update(2, 5)
	p.update(0, 10)
	p.update(2, 3) // retry starts from zero
	p.update(2, 5) // duplicate callback
	p.update(1, 10)
	p.update(2, 10)
	if len(values) != 4 || values[len(values)-1] != 95 || p.done != 30 {
		t.Fatalf("retried/out-of-order bytes counted incorrectly: %v, bytes=%d", values, p.done)
	}
	p.close()
	p.update(0, 11) // transport finishes closing a request after the worker exits
	if len(values) != 4 || p.done != 30 {
		t.Fatal("late request callback changed completed progress")
	}
}
