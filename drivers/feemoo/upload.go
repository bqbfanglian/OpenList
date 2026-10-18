package feemoo

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/shirou/gopsutil/v4/disk"
)

type preparedFile struct {
	reader  io.ReaderAt
	parts   []string
	hash    string
	fullMD5 string
	temp    *os.File
}

func (p *preparedFile) close() error {
	if p.temp == nil {
		return nil
	}
	return errors.Join(p.temp.Close(), os.Remove(p.temp.Name()))
}

type countWriter struct {
	done, total int64
	up          driver.UpdateProgress
}

func (p *countWriter) Write(b []byte) (int, error) {
	p.done += int64(len(b))
	if p.total > 0 {
		p.up(float64(p.done) / float64(p.total) * 40)
	}
	return len(b), nil
}

// Hashing is mandatory before initialization. Non-seekable sources go to disk;
// only the 32 KiB copy buffer and one MD5 per part stay in memory.
func (d *Feemoo) prepare(ctx context.Context, source model.FileStreamer, up driver.UpdateProgress) (prepared *preparedFile, err error) {
	p := &preparedFile{}
	defer func() {
		if err != nil {
			_ = p.close()
		}
	}()
	size := source.GetSize()
	var reader io.Reader = source
	if existing := source.GetFile(); existing != nil {
		p.reader = existing
		reader = io.NewSectionReader(existing, 0, size)
	} else {
		dir := d.TempDir
		if dir == "" && conf.Conf != nil {
			dir = conf.Conf.TempDir
		}
		if dir == "" {
			dir = os.TempDir()
		}
		if err := os.MkdirAll(dir, 0700); err != nil {
			return nil, fmt.Errorf("feemoo: cannot create temporary directory: %w", err)
		}
		usage, err := disk.UsageWithContext(ctx, dir)
		if err != nil {
			return nil, fmt.Errorf("feemoo: cannot check temporary disk space: %w", err)
		}
		if usage.Free < uint64(size)+(64<<20) {
			return nil, errors.New("feemoo: 临时磁盘空间不足，需要文件大小加 64 MiB")
		}
		p.temp, err = os.CreateTemp(dir, "feemoo-*")
		if err != nil {
			return nil, fmt.Errorf("feemoo: cannot create temporary file: %w", err)
		}
		p.reader = p.temp
	}
	reader = &driver.ReaderWithCtx{Reader: reader, Ctx: ctx}
	stop := context.AfterFunc(ctx, func() { _ = source.Close() })
	defer stop()
	aggregate, full := md5.New(), md5.New()
	progress := &countWriter{total: size, up: up}
	buffer := make([]byte, 32<<10)
	for offset := int64(0); offset < size || offset == 0; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		length := min(d.partSize, size-offset)
		part := md5.New()
		writers := []io.Writer{part, full, progress}
		if p.temp != nil {
			writers = []io.Writer{p.temp, part, full, progress}
		}
		n, err := io.CopyBuffer(io.MultiWriter(writers...), io.LimitReader(reader, length), buffer)
		if err != nil {
			return nil, fmt.Errorf("feemoo: prepare source: %w", err)
		}
		if n != length {
			return nil, fmt.Errorf("feemoo: source is shorter than its declared size: %w", io.ErrUnexpectedEOF)
		}
		digest := part.Sum(nil)
		p.parts = append(p.parts, hex.EncodeToString(digest))
		_, _ = aggregate.Write(digest)
		offset += length
		if size == 0 {
			break
		}
	}
	var extra [1]byte
	if n, err := reader.Read(extra[:]); n != 0 || err != io.EOF {
		if err != nil && err != io.EOF {
			return nil, fmt.Errorf("feemoo: source size check: %w", err)
		}
		return nil, errors.New("feemoo: source is longer than its declared size")
	}
	p.hash, p.fullMD5 = hex.EncodeToString(aggregate.Sum(nil)), hex.EncodeToString(full.Sum(nil))
	up(40)
	return p, nil
}

func (d *Feemoo) Put(ctx context.Context, dir model.Obj, source model.FileStreamer, up driver.UpdateProgress) (obj model.Obj, err error) {
	if d.partSize <= 0 {
		return nil, errs.StorageNotInit
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !dir.IsDir() {
		return nil, errs.NotFolder
	}
	if err := validateName(source.GetName()); err != nil {
		return nil, err
	}
	size := source.GetSize()
	if size < 0 || size > d.maxSize {
		return nil, fmt.Errorf("feemoo: 文件大小未知或超出账号单文件上限（%d 字节）", d.maxSize)
	}
	if up == nil {
		up = func(float64) {}
	}
	unlock, err := d.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()
	if source.GetExist() != nil {
		return nil, errs.ObjectAlreadyExists
	}
	existing, err := d.find(ctx, dir, source.GetName())
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, errs.ObjectAlreadyExists
	}
	prepared, err := d.prepare(ctx, source, up)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, prepared.close()) }()
	var session uploadSession
	err = d.api(ctx, "/disk-service/file/getDirectUploadLink", map[string]any{"folderid": dir.GetID(), "type": 1, "hash": prepared.hash, "totalSize": size, "filename": source.GetName(), "upload_line": d.uploadLine}, &session, false)
	if err != nil {
		return nil, err
	}
	// A session may be cleaned up after a failed part, using an independent short
	// context so task cancellation does not suppress the provider's cancel API.
	merged := false
	defer func() {
		if !merged && session.TaskID != "" && session.CancelURL != "" {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if cleanupErr := d.sessionPost(cleanupCtx, session.CancelURL, map[string]string{"task_id": string(session.TaskID)}, nil); cleanupErr != nil {
				err = errors.Join(err, fmt.Errorf("feemoo: 未完成上传会话清理失败: %w", cleanupErr))
			}
		}
	}()
	if id, _ := session.FileID.int64(); id <= 0 {
		if session.TaskID == "" || session.CancelURL == "" {
			return nil, errors.New("feemoo: missing upload session fields")
		}
		for _, endpoint := range []string{session.PartURL, session.MergeURL, session.CancelURL} {
			if err := secureUploadURL(endpoint); err != nil {
				return nil, err
			}
		}
		lastProgress := float64(40)
		for index, md5sum := range prepared.parts {
			offset := int64(index) * d.partSize
			length := min(d.partSize, size-offset)
			var partErr error
			for attempt := 0; attempt <= d.PartRetries; attempt++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				var signed struct {
					URL string `json:"sign_url"`
				}
				partErr = d.sessionPost(ctx, session.PartURL, map[string]string{"task_id": string(session.TaskID), "m": md5sum, "part_number": strconv.Itoa(index + 1), "upload_line": d.uploadLine}, &signed)
				if partErr == nil {
					partErr = d.putPart(ctx, signed.URL, prepared.reader, offset, length, func(n int64) {
						p := float64(95)
						if size > 0 {
							p = 40 + float64(offset+n)/float64(size)*55
						}
						if p > lastProgress {
							lastProgress = p
							up(p)
						}
					})
				}
				if partErr == nil {
					break
				}
				if attempt >= d.PartRetries || !retryable(partErr) {
					return nil, fmt.Errorf("feemoo: upload part %d: %w", index+1, partErr)
				}
				if err := delay(ctx, attempt); err != nil {
					return nil, err
				}
			}
		}
		// Never blindly retry a merge: its result can be ambiguous after a lost
		// response. Reconcile against a fresh listing before reporting failure.
		err = d.sessionPost(ctx, session.MergeURL, map[string]string{"task_id": string(session.TaskID)}, nil)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			found, verifyErr := d.verify(ctx, dir, source.GetName(), size)
			if verifyErr != nil {
				return nil, errors.Join(err, verifyErr)
			}
			merged = true
			up(100)
			return found, nil
		}
	}
	merged = true
	obj, err = d.verify(ctx, dir, source.GetName(), size)
	if err == nil {
		up(100)
	}
	return obj, err
}

type partReader struct {
	io.Reader
	sent int64
	up   func(int64)
}

func (r *partReader) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.sent += int64(n)
	r.up(r.sent)
	return n, err
}

func (d *Feemoo) putPart(ctx context.Context, endpoint string, reader io.ReaderAt, offset, length int64, up func(int64)) error {
	if err := secureUploadURL(endpoint); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(d.RequestTimeout)*time.Second)
	defer cancel()
	limited := driver.NewLimitedUploadStream(ctx, io.NewSectionReader(reader, offset, length))
	body := &partReader{Reader: limited, up: up}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, body)
	if err != nil {
		return errors.New("feemoo: invalid part endpoint")
	}
	req.ContentLength = length
	if length == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := d.client.Do(req)
	if err != nil {
		return transportError(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &httpError{resp.StatusCode}
	}
	_, err = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return err
}
