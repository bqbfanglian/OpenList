package feemoo

import (
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
)

type Feemoo struct {
	model.Storage
	Addition
	client            *http.Client
	apiBase           string
	requestKey        *rsa.PublicKey
	responseKey       *rsa.PrivateKey
	partSize, maxSize int64
	uploadLine        string
	writeGate         chan struct{}
}

func (d *Feemoo) Config() driver.Config          { return config }
func (d *Feemoo) GetAddition() driver.Additional { return &d.Addition }

func (d *Feemoo) Init(ctx context.Context) error {
	if d.RequestTimeout == 0 {
		d.RequestTimeout = 180
	}
	if d.RequestTimeout < 1 || d.RequestTimeout > 3600 || d.PartRetries < 0 || d.PartRetries > 5 {
		return errors.New("feemoo: invalid timeout or retry settings")
	}
	if d.RootFolderID == "" {
		d.RootFolderID = "0"
	}
	if d.apiBase == "" {
		d.apiBase = apiBaseURL
	}
	if d.client == nil {
		d.client = &http.Client{Transport: http.DefaultTransport.(*http.Transport).Clone(), CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	}
	if err := d.loadProtocolKeys(); err != nil {
		return err
	}
	d.writeGate = make(chan struct{}, 1)
	return d.authenticate(ctx)
}

func (d *Feemoo) Drop(context.Context) error {
	if d.client != nil {
		d.client.CloseIdleConnections()
	}
	return nil
}

func (d *Feemoo) lock(ctx context.Context) (func(), error) {
	if d.writeGate == nil {
		return nil, errs.StorageNotInit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	select {
	case d.writeGate <- struct{}{}:
		return func() { <-d.writeGate }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func validateName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\\\x00\r\n") {
		return errors.New("feemoo: invalid file or folder name")
	}
	return nil
}

func (d *Feemoo) List(ctx context.Context, dir model.Obj, _ model.ListArgs) ([]model.Obj, error) {
	if d.client == nil || d.requestKey == nil {
		return nil, errs.StorageNotInit
	}
	if !dir.IsDir() {
		return nil, errs.NotFolder
	}
	var objects []model.Obj
	seen := make(map[string]bool)
	for page := 1; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		var result listResult
		err := d.api(ctx, "/disk-service/file/list", map[string]any{"views": "", "down": "", "folder_id": dir.GetID(), "keywords": "", "page": page, "limit": 60, "size": "", "time": 1, "type": 0, "tag_id": ""}, &result, true)
		if err != nil {
			return nil, err
		}
		total, err := result.Total.int64()
		if err != nil || total < 0 {
			return nil, errors.New("feemoo: invalid listing total")
		}
		added := 0
		for _, item := range result.List {
			obj, err := item.object()
			if err != nil {
				return nil, err
			}
			key := string(item.IsFile) + ":" + obj.GetID()
			if !seen[key] {
				seen[key] = true
				objects = append(objects, obj)
				added++
			}
		}
		if int64(len(objects)) >= total {
			return objects, nil
		}
		if added == 0 {
			return nil, errors.New("feemoo: incomplete or repeating listing page")
		}
	}
}

func (d *Feemoo) Link(context.Context, model.Obj, model.LinkArgs) (*model.Link, error) {
	return nil, fmt.Errorf("feemoo: 此驱动优先用于上传，尚未实现下载和播放: %w", errs.NotSupport)
}

func (d *Feemoo) find(ctx context.Context, dir model.Obj, name string) (model.Obj, error) {
	objects, err := d.List(ctx, dir, model.ListArgs{Refresh: true})
	if err != nil {
		return nil, err
	}
	var found model.Obj
	for _, obj := range objects {
		if obj.GetName() == name {
			if found != nil {
				return nil, errors.New("feemoo: 目录中有多个同名对象，无法安全定位")
			}
			found = obj
		}
	}
	return found, nil
}

func (d *Feemoo) MakeDir(ctx context.Context, parent model.Obj, name string) (model.Obj, error) {
	if err := validateName(name); err != nil {
		return nil, err
	}
	if len(utf16.Encode([]rune(name))) > 50 {
		return nil, errors.New("feemoo: 网页协议文件夹名最长 50 个 UTF-16 字符；不会自动截断")
	}
	unlock, err := d.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	existing, err := d.find(ctx, parent, name)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if !existing.IsDir() {
			return nil, errs.ObjectAlreadyExists
		}
		return existing, nil
	}
	var result struct {
		FolderID scalar `json:"folder_id"`
	}
	err = d.api(ctx, "/disk-service/file/modifolder", map[string]any{"name": name, "pid": parent.GetID(), "id": "", "is_same": 1}, &result, false)
	if err != nil {
		var apiErr *apiError
		if !errors.As(err, &apiErr) || apiErr.code != "60010" {
			return nil, err
		}
	}
	obj, err := d.find(ctx, parent, name)
	if err != nil {
		return nil, err
	}
	if obj == nil || !obj.IsDir() {
		return nil, errors.New("feemoo: 创建目录后未能确认目标文件夹")
	}
	return obj, nil
}

func (d *Feemoo) verify(ctx context.Context, dir model.Obj, name string, size int64) (model.Obj, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for {
		obj, err := d.find(ctx, dir, name)
		if err != nil {
			return nil, err
		}
		if obj != nil {
			if obj.IsDir() || obj.GetSize() != size {
				return nil, errors.New("feemoo: 上传后目标文件类型或大小不一致")
			}
			return obj, nil
		}
		if err := delay(ctx, 2); err != nil {
			return nil, fmt.Errorf("feemoo: 上传后未在刷新目录中确认目标文件: %w", err)
		}
	}
}

var _ driver.Driver = (*Feemoo)(nil)
var _ driver.PutResult = (*Feemoo)(nil)
var _ driver.MkdirResult = (*Feemoo)(nil)
