// Package storage maps the S3 object model onto Google Drive: a bucket is a
// folder under a configurable root, and an object is a file inside that folder
// whose name is the full object key. S3 has a flat keyspace, so keys (including
// "/") are stored verbatim as Drive file names and prefix/delimiter listing is
// resolved in memory.
package storage

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"gdrives3/internal/crypt"
	"gdrives3/internal/gdrive"
)

var (
	ErrNoSuchBucket   = errors.New("no such bucket")
	ErrNoSuchKey      = errors.New("no such key")
	ErrBucketExists   = errors.New("bucket already exists")
	ErrBucketNotEmpty = errors.New("bucket not empty")
)

type Bucket struct {
	Name    string
	Created time.Time
}

type Object struct {
	Key          string
	Size         int64
	ETag         string
	LastModified time.Time
	ContentType  string
}

type ListResult struct {
	Objects        []Object
	CommonPrefixes []string
	IsTruncated    bool
	NextToken      string
}

type Store struct {
	gd       *gdrive.Client
	rootName string
	cipher   *crypt.Cipher // optional at-rest encryption

	mu     sync.Mutex
	rootID string
}

func New(gd *gdrive.Client, rootName string, cipher *crypt.Cipher) *Store {
	return &Store{gd: gd, rootName: rootName, cipher: cipher}
}

// objInfo reports the plaintext size and ETag for a stored file, accounting for
// encryption (which changes the on-Drive size and makes the MD5 opaque).
func (s *Store) objInfo(f gdrive.File) (int64, string) {
	if s.cipher != nil {
		return s.cipher.DecryptedSize(f.Size), f.MD5 + "-enc"
	}
	return f.Size, f.MD5
}

type decryptCloser struct {
	r io.Reader
	c io.Closer
}

func (d decryptCloser) Read(p []byte) (int, error) { return d.r.Read(p) }
func (d decryptCloser) Close() error               { return d.c.Close() }

func (s *Store) root(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rootID != "" {
		return s.rootID, nil
	}
	f, err := s.gd.Child(ctx, s.rootName, "root", true)
	if errors.Is(err, gdrive.ErrNotFound) {
		nf, cerr := s.gd.CreateFolder(ctx, s.rootName, "root")
		if cerr != nil {
			return "", cerr
		}
		s.rootID = nf.ID
		return s.rootID, nil
	}
	if err != nil {
		return "", err
	}
	s.rootID = f.ID
	return s.rootID, nil
}

func (s *Store) bucketID(ctx context.Context, name string) (string, error) {
	root, err := s.root(ctx)
	if err != nil {
		return "", err
	}
	f, err := s.gd.Child(ctx, name, root, true)
	if errors.Is(err, gdrive.ErrNotFound) {
		return "", ErrNoSuchBucket
	}
	if err != nil {
		return "", err
	}
	return f.ID, nil
}

func (s *Store) ListBuckets(ctx context.Context) ([]Bucket, error) {
	root, err := s.root(ctx)
	if err != nil {
		return nil, err
	}
	children, err := s.gd.List(ctx, root)
	if err != nil {
		return nil, err
	}
	var out []Bucket
	for _, c := range children {
		if c.IsFolder && !strings.HasPrefix(c.Name, ".") {
			out = append(out, Bucket{Name: c.Name, Created: c.Modified})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (s *Store) CreateBucket(ctx context.Context, name string) error {
	root, err := s.root(ctx)
	if err != nil {
		return err
	}
	if _, err := s.gd.Child(ctx, name, root, true); err == nil {
		return ErrBucketExists
	} else if !errors.Is(err, gdrive.ErrNotFound) {
		return err
	}
	_, err = s.gd.CreateFolder(ctx, name, root)
	return err
}

func (s *Store) HeadBucket(ctx context.Context, name string) error {
	_, err := s.bucketID(ctx, name)
	return err
}

func (s *Store) DeleteBucket(ctx context.Context, name string) error {
	id, err := s.bucketID(ctx, name)
	if err != nil {
		return err
	}
	children, err := s.gd.List(ctx, id)
	if err != nil {
		return err
	}
	if len(children) > 0 {
		return ErrBucketNotEmpty
	}
	return s.gd.Delete(ctx, id)
}

func (s *Store) PutObject(ctx context.Context, bucket, key string, size int64, r io.Reader, contentType string) (Object, error) {
	id, err := s.bucketID(ctx, bucket)
	if err != nil {
		return Object{}, err
	}
	// Overwrite semantics: drop an existing object with the same key first.
	if old, cerr := s.gd.Child(ctx, key, id, false); cerr == nil {
		_ = s.gd.Delete(ctx, old.ID)
	} else if !errors.Is(cerr, gdrive.ErrNotFound) {
		return Object{}, cerr
	}
	plainSize := size
	if s.cipher != nil {
		r = s.cipher.EncryptReader(r)
		size = s.cipher.EncryptedSize(size)
	}
	f, err := s.gd.Upload(ctx, key, id, size, r, contentType)
	if err != nil {
		return Object{}, err
	}
	tag := f.MD5
	if s.cipher != nil {
		tag = f.MD5 + "-enc"
	}
	return Object{Key: key, Size: plainSize, ETag: tag, LastModified: f.Modified, ContentType: contentType}, nil
}

func (s *Store) object(ctx context.Context, bucket, key string) (string, gdrive.File, error) {
	id, err := s.bucketID(ctx, bucket)
	if err != nil {
		return "", gdrive.File{}, err
	}
	f, err := s.gd.Child(ctx, key, id, false)
	if errors.Is(err, gdrive.ErrNotFound) {
		return "", gdrive.File{}, ErrNoSuchKey
	}
	if err != nil {
		return "", gdrive.File{}, err
	}
	return f.ID, *f, nil
}

func (s *Store) HeadObject(ctx context.Context, bucket, key string) (Object, error) {
	_, f, err := s.object(ctx, bucket, key)
	if err != nil {
		return Object{}, err
	}
	size, tag := s.objInfo(f)
	return Object{Key: key, Size: size, ETag: tag, LastModified: f.Modified, ContentType: f.MimeType}, nil
}

func (s *Store) GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, Object, error) {
	fid, f, err := s.object(ctx, bucket, key)
	if err != nil {
		return nil, Object{}, err
	}
	rc, err := s.gd.Download(ctx, fid)
	if err != nil {
		return nil, Object{}, err
	}
	if s.cipher != nil {
		rc = decryptCloser{r: s.cipher.DecryptReader(rc), c: rc}
	}
	size, tag := s.objInfo(f)
	return rc, Object{Key: key, Size: size, ETag: tag, LastModified: f.Modified, ContentType: f.MimeType}, nil
}

// GetObjectRange returns a reader for the inclusive plaintext byte range
// [start, end]. For encrypted objects it downloads only the ciphertext chunks
// that cover the range, decrypts them and trims to the exact bounds.
func (s *Store) GetObjectRange(ctx context.Context, bucket, key string, start, end int64) (io.ReadCloser, Object, error) {
	fid, f, err := s.object(ctx, bucket, key)
	if err != nil {
		return nil, Object{}, err
	}
	size, tag := s.objInfo(f)
	obj := Object{Key: key, Size: size, ETag: tag, LastModified: f.Modified, ContentType: f.MimeType}

	if s.cipher == nil {
		rc, err := s.gd.DownloadRange(ctx, fid, start, end)
		if err != nil {
			return nil, Object{}, err
		}
		return rc, obj, nil
	}

	hrc, err := s.gd.DownloadRange(ctx, fid, 0, int64(crypt.HeaderSize-1))
	if err != nil {
		return nil, Object{}, err
	}
	base, err := crypt.ParseHeader(hrc)
	hrc.Close()
	if err != nil {
		return nil, Object{}, err
	}
	block := int64(crypt.ChunkSize + crypt.ChunkOverhead)
	firstChunk := start / int64(crypt.ChunkSize)
	lastChunk := end / int64(crypt.ChunkSize)
	cipherStart := int64(crypt.HeaderSize) + firstChunk*block
	cipherEnd := int64(crypt.HeaderSize) + (lastChunk+1)*block - 1
	if cipherEnd > f.Size-1 {
		cipherEnd = f.Size - 1
	}
	crc, err := s.gd.DownloadRange(ctx, fid, cipherStart, cipherEnd)
	if err != nil {
		return nil, Object{}, err
	}
	dec := s.cipher.DecryptChunks(crc, base, uint64(firstChunk))
	if skip := start - firstChunk*int64(crypt.ChunkSize); skip > 0 {
		if _, err := io.CopyN(io.Discard, dec, skip); err != nil {
			crc.Close()
			return nil, Object{}, err
		}
	}
	return decryptCloser{r: io.LimitReader(dec, end-start+1), c: crc}, obj, nil
}

func (s *Store) DeleteObject(ctx context.Context, bucket, key string) error {
	fid, _, err := s.object(ctx, bucket, key)
	if errors.Is(err, ErrNoSuchKey) {
		return nil // S3 delete is idempotent
	}
	if err != nil {
		return err
	}
	return s.gd.Delete(ctx, fid)
}

// ListObjects implements the ListObjectsV2 prefix/delimiter/pagination logic on
// the flat set of keys stored in the bucket folder.
func (s *Store) ListObjects(ctx context.Context, bucket, prefix, delimiter, startAfter string, maxKeys int) (ListResult, error) {
	id, err := s.bucketID(ctx, bucket)
	if err != nil {
		return ListResult{}, err
	}
	children, err := s.gd.List(ctx, id)
	if err != nil {
		return ListResult{}, err
	}
	keys := make([]gdrive.File, 0, len(children))
	for _, c := range children {
		if !c.IsFolder && strings.HasPrefix(c.Name, prefix) {
			keys = append(keys, c)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i].Name < keys[j].Name })

	if maxKeys <= 0 || maxKeys > 1000 {
		maxKeys = 1000
	}
	var res ListResult
	seenPrefix := map[string]bool{}
	count := 0
	for _, f := range keys {
		if startAfter != "" && f.Name <= startAfter {
			continue
		}
		if delimiter != "" {
			rest := f.Name[len(prefix):]
			if i := strings.Index(rest, delimiter); i >= 0 {
				cp := prefix + rest[:i+len(delimiter)]
				if !seenPrefix[cp] {
					if count >= maxKeys {
						res.IsTruncated = true
						res.NextToken = f.Name
						return res, nil
					}
					seenPrefix[cp] = true
					res.CommonPrefixes = append(res.CommonPrefixes, cp)
					count++
				}
				continue
			}
		}
		if count >= maxKeys {
			res.IsTruncated = true
			res.NextToken = f.Name
			return res, nil
		}
		size, tag := s.objInfo(f)
		res.Objects = append(res.Objects, Object{Key: f.Name, Size: size, ETag: tag, LastModified: f.Modified, ContentType: f.MimeType})
		count++
	}
	return res, nil
}
