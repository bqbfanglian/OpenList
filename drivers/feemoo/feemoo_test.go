package feemoo

import (
	"bytes"
	"context"
	"crypto/md5"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/stream"
	"github.com/OpenListTeam/OpenList/v4/pkg/utils"
)

type mockTask struct {
	folder, name, hash string
	size               int64
	parts              map[int][]byte
	digests            map[int]string
}

type mockFeemoo struct {
	mu                               sync.Mutex
	t                                *testing.T
	server                           *httptest.Server
	key                              *rsa.PrivateKey
	items                            map[string][]entry
	tasks                            map[string]*mockTask
	next                             int
	initCalls, putCalls, cancelCalls int
	failParts                        int
	listCode                         string
	maxPart                          scalar
	mergeError, wrongSize            bool
	onPut                            func(http.ResponseWriter, *http.Request)
}

func newMock(t *testing.T) (*Feemoo, *mockFeemoo) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	m := &mockFeemoo{t: t, key: key, items: map[string][]entry{"0": {}}, tasks: make(map[string]*mockTask)}
	m.server = httptest.NewTLSServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.server.Close)
	d := &Feemoo{Addition: Addition{RootID: driver.RootID{RootFolderID: "0"}, Cookie: "fake=credential", Par: "fake-par", Pto: "fake-pto", UserAgent: "mock-agent", TempDir: t.TempDir(), PartRetries: 2}, client: m.server.Client(), apiBase: m.server.URL, requestKey: &key.PublicKey, responseKey: key}
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Drop(context.Background()) })
	return d, m
}

func (m *mockFeemoo) respond(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "1", "data": data})
}

func (m *mockFeemoo) handle(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/part/") {
		if r.Header.Get("Cookie") != "" || r.Header.Get("par") != "" {
			m.t.Error("account credential leaked to object store")
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return
		}
		components := strings.Split(strings.TrimPrefix(r.URL.Path, "/part/"), "/")
		number, _ := strconv.Atoi(components[1])
		m.mu.Lock()
		m.putCalls++
		hook := m.onPut
		if m.failParts > 0 {
			m.failParts--
			m.mu.Unlock()
			w.WriteHeader(503)
			return
		}
		task := m.tasks[components[0]]
		sum := md5.Sum(body)
		if hex.EncodeToString(sum[:]) != task.digests[number] {
			m.t.Error("part checksum does not match signed checksum")
		}
		task.parts[number] = body
		m.mu.Unlock()
		if hook != nil {
			hook(w, r)
			return
		}
		w.WriteHeader(200)
		return
	}
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		m.t.Error(err)
		w.WriteHeader(400)
		return
	}
	defer r.MultipartForm.RemoveAll()
	if strings.HasPrefix(r.URL.Path, "/session/") {
		if r.Header.Get("Cookie") != "" || r.Header.Get("pto") != "" {
			m.t.Error("account credential leaked to session endpoint")
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		taskID := r.FormValue("task_id")
		task := m.tasks[taskID]
		switch r.URL.Path {
		case "/session/sign":
			n, _ := strconv.Atoi(r.FormValue("part_number"))
			task.digests[n] = r.FormValue("m")
			m.respond(w, map[string]any{"sign_url": m.server.URL + "/part/" + taskID + "/" + strconv.Itoa(n)})
		case "/session/cancel":
			m.cancelCalls++
			delete(m.tasks, taskID)
			m.respond(w, map[string]any{})
		case "/session/merge":
			combined := md5.New()
			var size int64
			for i := 1; i <= len(task.parts); i++ {
				part := task.parts[i]
				size += int64(len(part))
				sum := md5.Sum(part)
				_, _ = combined.Write(sum[:])
			}
			if size != task.size || hex.EncodeToString(combined.Sum(nil)) != task.hash {
				m.t.Error("merge has missing parts or incorrect aggregate hash")
			}
			ext := path.Ext(task.name)
			name := strings.TrimSuffix(task.name, ext)
			if m.wrongSize {
				size++
			}
			m.next++
			m.items[task.folder] = append(m.items[task.folder], entry{ID: scalar(strconv.Itoa(m.next)), Name: name, Extension: ext, IsFile: "1", Size: scalar(strconv.FormatInt(size, 10))})
			if m.mergeError {
				w.WriteHeader(503)
				return
			}
			m.respond(w, map[string]any{"domain": "https://example.invalid"})
		default:
			w.WriteHeader(404)
		}
		return
	}
	if r.Header.Get("Cookie") != "fake=credential" || r.Header.Get("par") != "fake-par" || r.Header.Get("pto") != "fake-pto" {
		m.t.Error("missing account headers")
	}
	sn, err := base64.StdEncoding.DecodeString(r.FormValue("sn"))
	if err != nil {
		m.t.Error(err)
		return
	}
	code, err := rsa.DecryptPKCS1v15(rand.Reader, m.key, sn)
	if err != nil {
		m.t.Error(err)
		return
	}
	plain, err := decryptSecret(r.FormValue("jt"), string(code))
	if err != nil {
		m.t.Error(err)
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(plain, &payload); err != nil {
		m.t.Error(err)
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	text := func(key string) string { return fmt.Sprint(payload[key]) }
	switch r.URL.Path {
	case "/user-service/user/info":
		m.respond(w, map[string]any{"upload_type": "1", "upload_part_size": "65536", "upload_max_limit": "10737418240", "upload_line": "1", "direct_upload_max_part": m.maxPart})
	case "/disk-service/file/list":
		if m.listCode != "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": m.listCode, "msg": "fake failure"})
			return
		}
		items := m.items[text("folder_id")]
		page := int(payload["page"].(float64))
		start := min((page-1)*60, len(items))
		end := min(start+60, len(items))
		m.respond(w, map[string]any{"list": items[start:end], "total": strconv.Itoa(len(items))})
	case "/disk-service/file/modifolder":
		m.next++
		id := strconv.Itoa(m.next)
		m.items[text("pid")] = append(m.items[text("pid")], entry{ID: scalar(id), Name: text("name"), IsFile: "0"})
		m.items[id] = []entry{}
		m.respond(w, map[string]any{"folder_id": id})
	case "/disk-service/file/getDirectUploadLink":
		m.initCalls++
		id := strconv.Itoa(m.initCalls)
		m.tasks[id] = &mockTask{folder: text("folderid"), name: text("filename"), hash: text("hash"), size: int64(payload["totalSize"].(float64)), parts: make(map[int][]byte), digests: make(map[int]string)}
		m.respond(w, map[string]any{"file_id": "0", "task_id": id, "upload_part_url": m.server.URL + "/session/sign", "merge_part_url": m.server.URL + "/session/merge", "cancel_url": m.server.URL + "/session/cancel"})
	default:
		w.WriteHeader(404)
	}
}

func root() model.Obj { return &model.Object{ID: "0", Name: "root", IsFolder: true} }
func source(name string, content []byte, seekable bool) *stream.FileStream {
	var reader io.Reader = bytes.NewBuffer(content)
	if seekable {
		reader = bytes.NewReader(content)
	}
	return &stream.FileStream{Ctx: context.Background(), Obj: &model.Object{Name: name, Size: int64(len(content))}, Reader: reader}
}

func TestPutMockSmallMultipartAndEmpty(t *testing.T) {
	for _, size := range []int{0, 117, 3*65536 + 123} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			d, m := newMock(t)
			data := bytes.Repeat([]byte{0x5a}, size)
			file := source("中文 空格 &+ #.bin", data, false)
			var progress []float64
			obj, err := d.Put(context.Background(), root(), file, func(p float64) { progress = append(progress, p) })
			if err != nil {
				t.Fatal(err)
			}
			if obj.GetName() != file.GetName() || obj.GetSize() != int64(size) {
				t.Fatal("incorrect uploaded object")
			}
			for i, p := range progress {
				if p < 0 || p > 100 || i > 0 && p < progress[i-1] {
					t.Fatalf("invalid progress: %v", progress)
				}
			}
			if len(progress) == 0 || progress[len(progress)-1] != 100 {
				t.Fatal("success progress missing")
			}
			if m.initCalls != 1 || m.cancelCalls != 0 {
				t.Fatal("unexpected session lifecycle")
			}
			entries, _ := os.ReadDir(d.TempDir)
			if len(entries) != 0 {
				t.Fatal("temporary files leaked")
			}
		})
	}
}

func TestPartRetryAndFailureCleanup(t *testing.T) {
	for _, failures := range []int{1, 100} {
		t.Run(strconv.Itoa(failures), func(t *testing.T) {
			d, m := newMock(t)
			m.failParts = failures
			_, err := d.Put(context.Background(), root(), source("retry.bin", bytes.Repeat([]byte("x"), 65537), false), nil)
			if failures == 1 && err != nil {
				t.Fatal(err)
			}
			if failures == 100 && err == nil {
				t.Fatal("expected exhausted retry error")
			}
			if m.initCalls != 1 {
				t.Fatal("retry restarted entire upload")
			}
			if m.putCalls != 3 {
				t.Fatalf("expected 3 PUTs, got %d", m.putCalls)
			}
			if failures == 100 && m.cancelCalls != 1 {
				t.Fatal("failed session was not canceled")
			}
			entries, _ := os.ReadDir(d.TempDir)
			if len(entries) != 0 {
				t.Fatal("temporary file leaked")
			}
		})
	}
}

func TestCancelCleansSessionAndTempFile(t *testing.T) {
	d, m := newMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	m.onPut = func(_ http.ResponseWriter, r *http.Request) { close(started); <-r.Context().Done() }
	result := make(chan error, 1)
	go func() {
		_, err := d.Put(ctx, root(), source("cancel.bin", []byte("cancel me"), false), nil)
		result <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("part never started")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected cancel, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancel did not stop upload")
	}
	m.mu.Lock()
	canceled := m.cancelCalls
	m.mu.Unlock()
	if canceled != 1 {
		t.Fatal("canceled session not cleaned")
	}
	entries, _ := os.ReadDir(d.TempDir)
	if len(entries) != 0 {
		t.Fatal("temporary file leaked")
	}
}

func TestDuplicateExpiredAuthAndInvalidSize(t *testing.T) {
	d, m := newMock(t)
	m.items["0"] = []entry{{ID: "old", Name: "exists", Extension: ".txt", IsFile: "1", Size: "3"}}
	_, err := d.Put(context.Background(), root(), source("exists.txt", []byte("new"), false), nil)
	if !errors.Is(err, errs.ObjectAlreadyExists) {
		t.Fatalf("expected conflict, got %v", err)
	}
	m.listCode = "10020"
	_, err = d.Put(context.Background(), root(), source("new.txt", []byte("new"), false), nil)
	if !errors.Is(err, ErrSessionExpired) {
		t.Fatalf("expected expired session, got %v", err)
	}
	m.listCode = ""
	for _, size := range []int64{-1, d.maxSize + 1} {
		f := source("invalid.bin", nil, false)
		f.Obj = &model.Object{Name: "invalid.bin", Size: size}
		if _, err = d.Put(context.Background(), root(), f, nil); err == nil {
			t.Fatal("accepted invalid size")
		}
	}
	if m.initCalls != 0 {
		t.Fatal("invalid upload allocated a session")
	}
}

func TestSourceMismatchAndInsufficientDiskCleanup(t *testing.T) {
	for _, declared := range []int64{2, 8, 1 << 59} {
		t.Run(strconv.FormatInt(declared, 10), func(t *testing.T) {
			d, m := newMock(t)
			d.maxSize = 1 << 60
			f := source("size.txt", []byte("hello"), false)
			f.Obj = &model.Object{Name: "size.txt", Size: declared}
			if _, err := d.Put(context.Background(), root(), f, nil); err == nil {
				t.Fatal("expected source size/disk error")
			}
			if m.initCalls != 0 {
				t.Fatal("failed preparation allocated session")
			}
			entries, _ := os.ReadDir(d.TempDir)
			if len(entries) != 0 {
				t.Fatal("partial temporary file leaked")
			}
		})
	}
}

func TestMergeReconciliationAndVerificationFailure(t *testing.T) {
	for _, wrongSize := range []bool{false, true} {
		t.Run(strconv.FormatBool(wrongSize), func(t *testing.T) {
			d, m := newMock(t)
			m.mergeError = !wrongSize
			m.wrongSize = wrongSize
			p := float64(0)
			_, err := d.Put(context.Background(), root(), source("merge.txt", []byte("data"), true), func(v float64) { p = v })
			if wrongSize && (err == nil || p == 100) {
				t.Fatal("unverified upload reported successful")
			}
			if !wrongSize && err != nil {
				t.Fatalf("merge result was not reconciled: %v", err)
			}
			if m.initCalls != 1 {
				t.Fatal("merge ambiguity created duplicate session")
			}
		})
	}
}

func TestPaginationDirectoriesAndLockCancellation(t *testing.T) {
	d, m := newMock(t)
	for i := 0; i < 125; i++ {
		m.items["0"] = append(m.items["0"], entry{ID: scalar(strconv.Itoa(1000 + i)), Name: strconv.Itoa(i), IsFile: "0"})
	}
	objects, err := d.List(context.Background(), root(), model.ListArgs{})
	if err != nil || len(objects) != 125 {
		t.Fatalf("pagination: %d %v", len(objects), err)
	}
	folder, err := d.MakeDir(context.Background(), root(), "中文 目录 &+#")
	if err != nil {
		t.Fatal(err)
	}
	again, err := d.MakeDir(context.Background(), root(), folder.GetName())
	if err != nil || again.GetID() != folder.GetID() {
		t.Fatal("directory was duplicated")
	}
	if _, err = d.MakeDir(context.Background(), root(), strings.Repeat("a", 51)); err == nil {
		t.Fatal("folder name silently truncated")
	}
	unlock, err := d.lock(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = d.lock(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal("lock wait ignored cancellation")
	}
}

type cancelDuringRead struct {
	reader io.Reader
	cancel context.CancelFunc
}

func (r *cancelDuringRead) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.cancel()
	return n, err
}

func TestCancelDuringHashLeavesSourceOwnershipWithCopier(t *testing.T) {
	d, m := newMock(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := source("cancel-hash.bin", bytes.Repeat([]byte("x"), 128<<10), false)
	f.Reader = &cancelDuringRead{reader: f.Reader, cancel: cancel}
	closed := false
	f.Add(utils.CloseFunc(func() error { closed = true; return nil }))
	_, err := d.Put(ctx, root(), f, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("hash preparation did not stop: %v", err)
	}
	if closed {
		t.Fatal("driver closed the caller-owned source concurrently")
	}
	if m.initCalls != 0 {
		t.Fatal("canceled hash allocated upload session")
	}
	entries, _ := os.ReadDir(d.TempDir)
	if len(entries) != 0 {
		t.Fatal("canceled hashing leaked temporary file")
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("source owner could not release stream")
	}
}
