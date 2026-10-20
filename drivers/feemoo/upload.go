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
	"sync"
	"sync/atomic"
	"time"

	"github.com/OpenListTeam/OpenList/v4/internal/conf"
	"github.com/OpenListTeam/OpenList/v4/internal/driver"
	"github.com/OpenListTeam/OpenList/v4/internal/errs"
	"github.com/OpenListTeam/OpenList/v4/internal/model"
	"github.com/shirou/gopsutil/v4/disk"
	"golang.org/x/sync/errgroup"
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
	// The source's HTTP stream already shares the task context. op.Put owns its
	// Close; concurrently closing FileStream's non-thread-safe closer slice here
	// can race with source initialization or the copier's deferred cleanup.
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
		if err := d.uploadParts(ctx, session, prepared, size, up); err != nil {
			return nil, err
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

// Progress callbacks are serialized, and each part contributes only its highest
// byte count. Retries and out-of-order completion must not inflate or regress it.
type uploadProgress struct {
	mu          sync.Mutex
	sent        []int64
	done, total int64
	up          driver.UpdateProgress
	closed      bool
}

func (p *uploadProgress) update(index int, n int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || n <= p.sent[index] {
		return
	}
	p.done += n - p.sent[index]
	p.sent[index] = n
	if p.total > 0 {
		p.up(40 + float64(p.done)/float64(p.total)*55)
	}
}

func (p *uploadProgress) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
}

func (d *Feemoo) uploadParts(ctx context.Context, session uploadSession, prepared *preparedFile, size int64, up driver.UpdateProgress) error {
	group, partCtx := errgroup.WithContext(ctx)
	progress := &uploadProgress{sent: make([]int64, len(prepared.parts)), total: size, up: up}
	// A transport may finish closing an unsuccessful request body after Do
	// returns. Ignore late notifications before the caller reports completion.
	defer progress.close()
	var next atomic.Int64
	workers := min(max(d.partConcurrency, 1), len(prepared.parts))
	for range workers {
		group.Go(func() error {
			for {
				if err := partCtx.Err(); err != nil {
					return err
				}
				index := int(next.Add(1) - 1)
				if index >= len(prepared.parts) {
					return nil
				}
				if err := d.uploadPart(partCtx, session, prepared, index, size, func(n int64) {
					progress.update(index, n)
				}); err != nil {
					return err
				}
			}
		})
	}
	// Wait for every worker before merging, canceling the session or releasing
	// the temporary file. The first failure cancels all in-flight siblings.
	return group.Wait()
}

func (d *Feemoo) uploadPart(ctx context.Context, session uploadSession, prepared *preparedFile, index int, size int64, up func(int64)) error {
	offset := int64(index) * d.partSize
	length := min(d.partSize, size-offset)
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		var signed struct {
			URL string `json:"sign_url"`
		}
		err := d.sessionPost(ctx, session.PartURL, map[string]string{"task_id": string(session.TaskID), "m": prepared.parts[index], "part_number": strconv.Itoa(index + 1), "upload_line": d.uploadLine}, &signed)
		if err == nil {
			err = d.putPart(ctx, signed.URL, prepared.reader, offset, length, up)
		}
		if err == nil {
			return nil
		}
		if attempt >= d.PartRetries || !retryable(err) {
			return fmt.Errorf("feemoo: upload part %d: %w", index+1, err)
		}
		if err := delay(ctx, attempt); err != nil {
			return err
		}
	}
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
