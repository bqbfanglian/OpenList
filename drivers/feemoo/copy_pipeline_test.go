package feemoo_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OpenListTeam/OpenList/v4/drivers/feemoo"
	webdavdriver "github.com/OpenListTeam/OpenList/v4/drivers/webdav"
	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/fs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/OpenListTeam/OpenList/v4/internal/op"
	"golang.org/x/net/webdav"
)

func webdavSource(t *testing.T, files map[string][]byte) *webdavdriver.WebDav {
	t.Helper()
	directory := t.TempDir()
	for name, content := range files {
		target := filepath.Join(directory, filepath.FromSlash(name))
		if content == nil {
			if err := os.MkdirAll(target, 0700); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	handler := &webdav.Handler{FileSystem: webdav.Dir(directory), LockSystem: webdav.NewMemLS()}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "test" || password != "test" {
			w.Header().Set("WWW-Authenticate", `Basic realm="test"`)
			w.WriteHeader(401)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	d := &webdavdriver.WebDav{Addition: webdavdriver.Addition{Vendor: "other", Address: server.URL, Username: "test", Password: "test", RootPath: driver.RootPath{RootFolderPath: "/"}}}
	d.SetStorage(model.Storage{MountPath: "/webdav-source-" + t.Name(), CacheExpiration: 1})
	if err := d.Init(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Drop(context.Background()) })
	return d
}

func runCopy(t *testing.T, ctx context.Context, src, dst driver.Driver, sourcePath string) {
	t.Helper()
	var callback func(*fs.FileTransferTask) error
	callback = func(task *fs.FileTransferTask) error { task.SetCtx(ctx); return task.RunWithNextTaskCallback(callback) }
	task := &fs.FileTransferTask{TaskData: fs.TaskData{SrcStorage: src, DstStorage: dst, SrcActualPath: sourcePath, DstActualPath: "/", SrcStorageMp: src.GetStorage().MountPath, DstStorageMp: dst.GetStorage().MountPath}}
	if err := callback(task); err != nil {
		t.Fatal(err)
	}
}

func setupCopyConfig(t *testing.T) {
	t.Helper()
	previous := conf.Conf
	conf.Conf = &conf.Config{TempDir: t.TempDir()}
	t.Cleanup(func() { conf.Conf = previous })
}

func TestNativeCopyWebDAVToFeemooMock(t *testing.T) {
	setupCopyConfig(t)
	files := map[string][]byte{"fixture/one.txt": []byte("first file"), "fixture/two.bin": bytes.Repeat([]byte{0x43}, 150000), "fixture/子目录/中文 空格 &+ #.txt": []byte("保留目录与内容"), "fixture/子目录/empty.txt": {}, "fixture/空目录": nil}
	src := webdavSource(t, files)
	dst, uploaded := feemoo.NewMockForCopyTests(t)
	dst.SetStorage(model.Storage{MountPath: "/feemoo-" + t.Name(), CacheExpiration: 1})
	ctx := context.Background()
	// Single and batch copies use the same native FileTransferTask as the UI.
	runCopy(t, ctx, src, dst, "/fixture/one.txt")
	runCopy(t, ctx, src, dst, "/fixture/two.bin")
	runCopy(t, ctx, src, dst, "/fixture")
	for name, data := range files {
		obj, err := op.Get(ctx, dst, "/"+name)
		if err != nil {
			t.Fatalf("missing copied path %s: %v", name, err)
		}
		if data == nil {
			if !obj.IsDir() {
				t.Fatal("empty directory not preserved")
			}
			continue
		}
		if obj.GetSize() != int64(len(data)) {
			t.Fatalf("wrong copied size: %s", name)
		}
		if !bytes.Equal(uploaded(filepath.Base(name)), data) {
			t.Fatalf("copied bytes differ: %s", name)
		}
	}
}

// Real tests are deliberately opt-in; normal CI never reads a user's credentials.
func TestLiveNativeCopyToFeemoo(t *testing.T) {
	file := os.Getenv("FEEMOO_LIVE_SESSION_FILE")
	if file == "" {
		t.Skip("set FEEMOO_LIVE_SESSION_FILE to an ignored local session JSON to run real uploads")
	}
	setupCopyConfig(t)
	raw, err := os.ReadFile(file)
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
	dst := &feemoo.Feemoo{Addition: feemoo.Addition{Cookie: session.Cookie, Par: session.Par, Pto: session.Pto, UserAgent: session.UserAgent, PartRetries: 2, TempDir: conf.Conf.TempDir}}
	dst.SetStorage(model.Storage{MountPath: "/feemoo-live", CacheExpiration: 1})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	if err := dst.Init(ctx); err != nil {
		t.Fatal(err)
	}
	defer dst.Drop(context.Background())
	name := "OpenList_Go_test_" + time.Now().UTC().Format("20060102_150405")
	root, err := dst.MakeDir(ctx, &model.Object{ID: "0", IsFolder: true}, name)
	if err != nil {
		t.Fatal(err)
	}
	dst.RootFolderID = root.GetID()
	nonce := []byte(fmt.Sprintf("native-copy-%d", time.Now().UnixNano()))
	large := bytes.Repeat(nonce, (21<<20)/len(nonce)+1)
	files := map[string][]byte{"fixture/one.txt": append([]byte("Go 上传测试\n"), nonce...), "fixture/two.bin": large, "fixture/子目录/中文 空格 &+ #.txt": append([]byte("目录与字节完整\n"), nonce...), "fixture/子目录/empty.txt": {}, "fixture/空目录": nil}
	src := webdavSource(t, files)
	runCopy(t, ctx, src, dst, "/fixture/one.txt")
	runCopy(t, ctx, src, dst, "/fixture/two.bin")
	runCopy(t, ctx, src, dst, "/fixture")
	for file, content := range files {
		obj, err := op.Get(ctx, dst, "/"+file)
		if err != nil {
			t.Fatalf("missing remote result %s: %v", file, err)
		}
		if content == nil {
			if !obj.IsDir() {
				t.Fatal("missing empty directory")
			}
		} else if obj.GetSize() != int64(len(content)) {
			t.Fatalf("remote size mismatch: %s", file)
		}
	}
	t.Logf("REAL_UPLOAD_VERIFIED folder=%s; single, batch, recursive, empty files and empty folders; listing/size checked; server does not expose a content hash", name)
}
