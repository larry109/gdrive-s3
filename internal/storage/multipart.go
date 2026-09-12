package storage

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"

	"gdrives3/internal/gdrive"
)

const uploadsFolder = ".uploads"

var ErrNoSuchUpload = errors.New("no such upload")

func (s *Store) uploadsRoot(ctx context.Context) (string, error) {
	root, err := s.root(ctx)
	if err != nil {
		return "", err
	}
	f, err := s.gd.Child(ctx, uploadsFolder, root, true)
	if errors.Is(err, gdrive.ErrNotFound) {
		nf, cerr := s.gd.CreateFolder(ctx, uploadsFolder, root)
		if cerr != nil {
			return "", cerr
		}
		return nf.ID, nil
	}
	if err != nil {
		return "", err
	}
	return f.ID, nil
}

func (s *Store) uploadFolder(ctx context.Context, uploadID string) (string, error) {
	up, err := s.uploadsRoot(ctx)
	if err != nil {
		return "", err
	}
	f, err := s.gd.Child(ctx, uploadID, up, true)
	if errors.Is(err, gdrive.ErrNotFound) {
		return "", ErrNoSuchUpload
	}
	if err != nil {
		return "", err
	}
	return f.ID, nil
}

// CreateMultipartUpload allocates an upload id and a staging folder for its parts.
func (s *Store) CreateMultipartUpload(ctx context.Context, bucket, key string) (string, error) {
	if _, err := s.bucketID(ctx, bucket); err != nil {
		return "", err
	}
	up, err := s.uploadsRoot(ctx)
	if err != nil {
		return "", err
	}
	id := randomID()
	if _, err := s.gd.CreateFolder(ctx, id, up); err != nil {
		return "", err
	}
	return id, nil
}

// UploadPart stores a single part; the returned ETag is its MD5.
func (s *Store) UploadPart(ctx context.Context, uploadID string, partNumber int, size int64, r io.Reader) (string, error) {
	folder, err := s.uploadFolder(ctx, uploadID)
	if err != nil {
		return "", err
	}
	name := partName(partNumber)
	if old, cerr := s.gd.Child(ctx, name, folder, false); cerr == nil {
		_ = s.gd.Delete(ctx, old.ID)
	}
	f, err := s.gd.Upload(ctx, name, folder, size, r, "application/octet-stream")
	if err != nil {
		return "", err
	}
	return f.MD5, nil
}

// CompleteMultipartUpload concatenates the listed parts (in order) into the final
// object and removes the staging folder.
func (s *Store) CompleteMultipartUpload(ctx context.Context, bucket, key, uploadID string, parts []int) (Object, error) {
	bid, err := s.bucketID(ctx, bucket)
	if err != nil {
		return Object{}, err
	}
	folder, err := s.uploadFolder(ctx, uploadID)
	if err != nil {
		return Object{}, err
	}
	children, err := s.gd.List(ctx, folder)
	if err != nil {
		return Object{}, err
	}
	byName := map[string]gdrive.File{}
	for _, c := range children {
		byName[c.Name] = c
	}

	var ordered []gdrive.File
	var total int64
	for _, n := range parts {
		f, ok := byName[partName(n)]
		if !ok {
			return Object{}, fmt.Errorf("%w: missing part %d", ErrNoSuchUpload, n)
		}
		ordered = append(ordered, f)
		total += f.Size
	}

	if old, cerr := s.gd.Child(ctx, key, bid, false); cerr == nil {
		_ = s.gd.Delete(ctx, old.ID)
	}
	pr := &partsReader{ctx: ctx, gd: s.gd, parts: ordered}
	var reader io.Reader = pr
	uploadSize := total
	if s.cipher != nil {
		reader = s.cipher.EncryptReader(pr)
		uploadSize = s.cipher.EncryptedSize(total)
	}
	f, err := s.gd.Upload(ctx, key, bid, uploadSize, reader, "application/octet-stream")
	pr.close()
	if err != nil {
		return Object{}, err
	}
	_ = s.gd.Delete(ctx, folder)
	size, tag := s.objInfo(*f)
	return Object{Key: key, Size: size, ETag: tag, LastModified: f.Modified}, nil
}

func (s *Store) AbortMultipartUpload(ctx context.Context, uploadID string) error {
	folder, err := s.uploadFolder(ctx, uploadID)
	if errors.Is(err, ErrNoSuchUpload) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.gd.Delete(ctx, folder)
}

// partsReader streams the parts back to back, opening each from Drive on demand.
type partsReader struct {
	ctx   context.Context
	gd    *gdrive.Client
	parts []gdrive.File
	idx   int
	cur   io.ReadCloser
}

func (p *partsReader) Read(b []byte) (int, error) {
	for {
		if p.cur == nil {
			if p.idx >= len(p.parts) {
				return 0, io.EOF
			}
			rc, err := p.gd.Download(p.ctx, p.parts[p.idx].ID)
			if err != nil {
				return 0, err
			}
			p.cur = rc
			p.idx++
		}
		n, err := p.cur.Read(b)
		if err == io.EOF {
			p.cur.Close()
			p.cur = nil
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (p *partsReader) close() {
	if p.cur != nil {
		p.cur.Close()
	}
}

func partName(n int) string { return fmt.Sprintf("%05d", n) }

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
