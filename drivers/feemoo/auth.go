package feemoo

import (
	"context"
	"errors"
	"strings"
)

var ErrSessionExpired = errors.New("feemoo: 登录会话已失效，请在官网重新登录并更新 Cookie、par、pto 和 User-Agent")

func (d *Feemoo) authenticate(ctx context.Context) error {
	for _, value := range []string{d.Cookie, d.Par, d.Pto, d.UserAgent} {
		if strings.TrimSpace(value) == "" || strings.ContainsAny(value, "\r\n") {
			return errors.New("feemoo: 请填写同一次网页登录请求中的 Cookie、par、pto 和 User-Agent（不得包含换行）")
		}
	}
	var info userInfo
	if err := d.api(ctx, "/user-service/user/info", map[string]any{}, &info, true); err != nil {
		return err
	}
	if info.UploadType != "1" {
		return errors.New("feemoo: 当前账号未启用已验证的网页直传协议（upload_type=1）")
	}
	partSize, err := info.PartSize.int64()
	if err != nil || partSize <= 0 {
		return errors.New("feemoo: 账号未返回有效的分片大小")
	}
	maxSize, err := info.MaxSize.int64()
	if err != nil || maxSize < 0 {
		return errors.New("feemoo: 账号未返回有效的单文件上传限制")
	}
	d.partSize, d.maxSize, d.uploadLine = partSize, maxSize, string(info.UploadLine)
	// The official web client uses direct_upload_max_part for its worker count.
	// Missing or malformed limits retain the conservative single-part path.
	d.partConcurrency = 1
	if allowed, err := info.MaxPart.int64(); err == nil && allowed > 0 {
		d.partConcurrency = int(min(allowed, 8))
	}
	if d.PartConcurrency > 0 {
		d.partConcurrency = min(d.partConcurrency, d.PartConcurrency)
	}
	return nil
}
