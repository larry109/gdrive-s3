package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"gdrives3/internal/storage"
)

// Accounts resolves an access key to its secret and per-user storage backend.
type Accounts interface {
	Lookup(accessKey string) (secret string, store *storage.Store, ok bool)
}

type ctxKey int

const storeKey ctxKey = 0

type Server struct {
	accounts Accounts
	region   string
}

func New(accounts Accounts, region string) *Server {
	if region == "" {
		region = "us-east-1"
	}
	return &Server{accounts: accounts, region: region}
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.route) }

func (s *Server) st(r *http.Request) *storage.Store {
	return r.Context().Value(storeKey).(*storage.Store)
}

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	accessKey, aerr := verifySigV4(r, func(ak string) (string, bool) {
		secret, _, ok := s.accounts.Lookup(ak)
		return secret, ok
	})
	if aerr != nil {
		writeError(w, r, *aerr)
		return
	}
	_, store, ok := s.accounts.Lookup(accessKey)
	if !ok {
		writeError(w, r, errInvalidAccessKey)
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), storeKey, store))
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")

	switch {
	case bucket == "":
		if r.Method == http.MethodGet {
			s.listBuckets(w, r)
			return
		}
		writeError(w, r, errNotImplemented)
	case key == "":
		s.bucketOp(w, r, bucket)
	default:
		s.objectOp(w, r, bucket, key)
	}
}

func (s *Server) bucketOp(w http.ResponseWriter, r *http.Request, bucket string) {
	switch r.Method {
	case http.MethodGet:
		if _, ok := r.URL.Query()["location"]; ok {
			s.getBucketLocation(w, r)
			return
		}
		s.listObjects(w, r, bucket)
	case http.MethodHead:
		s.mapErr(w, r, s.st(r).HeadBucket(r.Context(), bucket))
	case http.MethodPut:
		s.mapErr(w, r, s.st(r).CreateBucket(r.Context(), bucket))
	case http.MethodDelete:
		if err := s.st(r).DeleteBucket(r.Context(), bucket); err != nil {
			s.mapErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, r, errNotImplemented)
	}
}

func (s *Server) objectOp(w http.ResponseWriter, r *http.Request, bucket, key string) {
	q := r.URL.Query()
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		s.createMultipart(w, r, bucket, key)
		return
	case r.Method == http.MethodPut && q.Get("uploadId") != "":
		s.uploadPart(w, r, bucket, key)
		return
	case r.Method == http.MethodPost && q.Get("uploadId") != "":
		s.completeMultipart(w, r, bucket, key)
		return
	case r.Method == http.MethodDelete && q.Get("uploadId") != "":
		s.abortMultipart(w, r, bucket, key)
		return
	}
	switch r.Method {
	case http.MethodPut:
		s.putObject(w, r, bucket, key)
	case http.MethodGet:
		s.getObject(w, r, bucket, key)
	case http.MethodHead:
		s.headObject(w, r, bucket, key)
	case http.MethodDelete:
		if err := s.st(r).DeleteObject(r.Context(), bucket, key); err != nil {
			s.mapErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, r, errNotImplemented)
	}
}

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := s.st(r).ListBuckets(r.Context())
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	res := listAllMyBucketsResult{XMLNS: s3NS, Owner: owner{ID: "gdrive-s3", DisplayName: "gdrive-s3"}}
	for _, b := range buckets {
		res.Buckets.Bucket = append(res.Buckets.Bucket, bucketEntry{Name: b.Name, CreationDate: amzTime(b.Created)})
	}
	writeXML(w, http.StatusOK, res)
}

func (s *Server) getBucketLocation(w http.ResponseWriter, r *http.Request) {
	type loc struct {
		XMLName xml.Name `xml:"LocationConstraint"`
		XMLNS   string   `xml:"xmlns,attr"`
		Value   string   `xml:",chardata"`
	}
	writeXML(w, http.StatusOK, loc{XMLNS: s3NS, Value: s.region})
}

func (s *Server) listObjects(w http.ResponseWriter, r *http.Request, bucket string) {
	q := r.URL.Query()
	prefix := q.Get("prefix")
	delimiter := q.Get("delimiter")
	maxKeys, _ := strconv.Atoi(q.Get("max-keys"))
	// ListObjectsV2 uses continuation-token/start-after; V1 uses marker.
	startAfter := q.Get("start-after")
	if t := q.Get("continuation-token"); t != "" {
		startAfter = t
	} else if m := q.Get("marker"); m != "" {
		startAfter = m
	}

	res, err := s.st(r).ListObjects(r.Context(), bucket, prefix, delimiter, startAfter, maxKeys)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	out := listBucketResult{
		XMLNS: s3NS, Name: bucket, Prefix: prefix, Delimiter: delimiter,
		MaxKeys: maxKeys, IsTruncated: res.IsTruncated, NextToken: res.NextToken,
	}
	for _, o := range res.Objects {
		out.Contents = append(out.Contents, objectEntry{
			Key: o.Key, LastModified: amzTime(o.LastModified), ETag: etag(o.ETag),
			Size: o.Size, StorageClass: "STANDARD",
		})
	}
	for _, cp := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: cp})
	}
	out.KeyCount = len(out.Contents) + len(out.CommonPrefixes)
	writeXML(w, http.StatusOK, out)
}

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	body, size := bodyReader(r)
	obj, err := s.st(r).PutObject(r.Context(), bucket, key, size, body, r.Header.Get("Content-Type"))
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if obj.ETag != "" {
		w.Header().Set("ETag", etag(obj.ETag))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	st := s.st(r)
	rangeHdr := r.Header.Get("Range")
	if rangeHdr == "" {
		rc, obj, err := st.GetObject(r.Context(), bucket, key)
		if err != nil {
			s.mapErr(w, r, err)
			return
		}
		defer rc.Close()
		setObjectHeaders(w, obj)
		w.WriteHeader(http.StatusOK)
		_, _ = io.Copy(w, rc)
		return
	}

	head, err := st.HeadObject(r.Context(), bucket, key)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	start, end, ok := parseRange(rangeHdr, head.Size)
	if !ok {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", head.Size))
		writeError(w, r, errInvalidRange)
		return
	}
	rc, obj, err := st.GetObjectRange(r.Context(), bucket, key, start, end)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	defer rc.Close()
	setObjectHeaders(w, obj)
	w.Header().Set("Content-Length", strconv.FormatInt(end-start+1, 10))
	w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, head.Size))
	w.WriteHeader(http.StatusPartialContent)
	_, _ = io.Copy(w, rc)
}

// parseRange parses a single-range "bytes=" header against the object size.
func parseRange(h string, size int64) (int64, int64, bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = spec[:i]
	}
	dash := strings.IndexByte(spec, '-')
	if dash < 0 {
		return 0, 0, false
	}
	startS, endS := spec[:dash], spec[dash+1:]
	var start, end int64
	switch {
	case startS == "":
		n, err := strconv.ParseInt(endS, 10, 64)
		if err != nil || n <= 0 {
			return 0, 0, false
		}
		if n > size {
			n = size
		}
		start, end = size-n, size-1
	case endS == "":
		s, err := strconv.ParseInt(startS, 10, 64)
		if err != nil {
			return 0, 0, false
		}
		start, end = s, size-1
	default:
		s, err1 := strconv.ParseInt(startS, 10, 64)
		e, err2 := strconv.ParseInt(endS, 10, 64)
		if err1 != nil || err2 != nil {
			return 0, 0, false
		}
		start, end = s, e
		if end > size-1 {
			end = size - 1
		}
	}
	if size == 0 || start < 0 || start > end || start >= size {
		return 0, 0, false
	}
	return start, end, true
}

func (s *Server) headObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	obj, err := s.st(r).HeadObject(r.Context(), bucket, key)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	setObjectHeaders(w, obj)
	w.WriteHeader(http.StatusOK)
}

func setObjectHeaders(w http.ResponseWriter, o storage.Object) {
	if o.ContentType != "" {
		w.Header().Set("Content-Type", o.ContentType)
	}
	w.Header().Set("Content-Length", strconv.FormatInt(o.Size, 10))
	if o.ETag != "" {
		w.Header().Set("ETag", etag(o.ETag))
	}
	if !o.LastModified.IsZero() {
		w.Header().Set("Last-Modified", o.LastModified.UTC().Format(http.TimeFormat))
	}
	w.Header().Set("Accept-Ranges", "bytes")
}

func (s *Server) mapErr(w http.ResponseWriter, r *http.Request, err error) {
	if err == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
	switch {
	case errors.Is(err, storage.ErrNoSuchBucket):
		writeError(w, r, errNoSuchBucket)
	case errors.Is(err, storage.ErrNoSuchKey):
		writeError(w, r, errNoSuchKey)
	case errors.Is(err, storage.ErrNoSuchUpload):
		writeError(w, r, errNoSuchUpload)
	case errors.Is(err, storage.ErrBucketExists):
		writeError(w, r, errBucketExists)
	case errors.Is(err, storage.ErrBucketNotEmpty):
		writeError(w, r, errBucketNotEmpty)
	case errors.Is(err, context.Canceled):
		// client went away
	default:
		writeError(w, r, errInternal)
	}
}

func reqID(r *http.Request) string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
