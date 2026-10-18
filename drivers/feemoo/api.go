package feemoo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

const apiBaseURL = "https://fmpapi.feemoo.com"
const maxAPIResponse = 16 << 20

type httpError struct{ code int }

func (e *httpError) Error() string { return "feemoo: HTTP " + strconv.Itoa(e.code) }

type apiError struct{ code string }

func (e *apiError) Error() string {
	switch e.code {
	case "40100":
		return "feemoo: 网页设备认证失败，请重新登录并更新会话请求头（40100）"
	case "40001":
		return "feemoo: 账号需要在官网完成手机绑定（40001）"
	default:
		return "feemoo: API status " + e.code + "；请在官网检查账号权限、配额或限制"
	}
}

func retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, ErrSessionExpired) {
		return false
	}
	var h *httpError
	if errors.As(err, &h) {
		return h.code == 429 || h.code >= 500
	}
	var a *apiError
	if errors.As(err, &a) {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var n net.Error
	if errors.As(err, &n) {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)
}

func delay(ctx context.Context, attempt int) error {
	timer := time.NewTimer(time.Duration(1<<attempt) * 250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func formBody(values map[string]string) (*bytes.Buffer, string, error) {
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	for key, value := range values {
		if err := w.WriteField(key, value); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return &body, w.FormDataContentType(), nil
}

func (d *Feemoo) api(ctx context.Context, endpoint string, payload any, result any, readOnly bool) error {
	fields, err := d.encryptRequest(payload)
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err = d.post(ctx, d.apiBase+endpoint, fields, result, true)
		if err == nil || !readOnly || attempt >= 2 || !retryable(err) {
			return err
		}
		if err = delay(ctx, attempt); err != nil {
			return err
		}
	}
}

// Session and object-store requests deliberately carry no account credentials.
func (d *Feemoo) sessionPost(ctx context.Context, endpoint string, fields map[string]string, result any) error {
	if err := secureUploadURL(endpoint); err != nil {
		return err
	}
	return d.post(ctx, endpoint, fields, result, false)
}

func (d *Feemoo) post(ctx context.Context, endpoint string, fields map[string]string, result any, account bool) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	body, contentType, err := formBody(fields)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return errors.New("feemoo: invalid request endpoint")
	}
	req.Header.Set("Content-Type", contentType)
	if account {
		req.Header.Set("Cookie", d.Cookie)
		req.Header.Set("par", d.Par)
		req.Header.Set("pto", d.Pto)
		req.Header.Set("User-Agent", d.UserAgent)
		req.Header.Set("os", "pc")
		req.Header.Set("fm-lang", "zh-CN")
		req.Header.Set("Origin", "https://www.feemoo.com")
		req.Header.Set("Referer", "https://www.feemoo.com/")
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &httpError{resp.StatusCode}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxAPIResponse+1))
	if err != nil {
		return transportError(ctx, err)
	}
	if len(raw) > maxAPIResponse {
		return errors.New("feemoo: API response exceeds safety limit")
	}
	var env envelope
	if err = json.Unmarshal(raw, &env); err != nil {
		return errors.New("feemoo: invalid API JSON response")
	}
	if env.Status != "1" {
		if env.Status == "10020" || env.Status == "1012" {
			return ErrSessionExpired
		}
		return &apiError{string(env.Status)}
	}
	if result == nil {
		return nil
	}
	data, err := d.decryptResponse(env.Data)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, result); err != nil {
		return errors.New("feemoo: unexpected API data format")
	}
	return nil
}

func transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var u *url.Error
	if errors.As(err, &u) {
		err = u.Err
	} // Do not expose signed URLs in task errors.
	return fmt.Errorf("feemoo: HTTP transport: %w", err)
}

func secureUploadURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return errors.New("feemoo: invalid or non-HTTPS upload URL")
	}
	return nil
}
