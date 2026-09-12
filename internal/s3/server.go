package s3

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"gdrives3/internal/storage"
)

// CredentialFunc resolves the secret key for an access key.
type CredentialFunc func(accessKey string) (secret string, ok bool)

type Server struct {
	store  *storage.Store
	creds  CredentialFunc
	region string
}

func New(store *storage.Store, creds CredentialFunc, region string) *Server {
	if region == "" {
		region = "us-east-1"
	}
	return &Server{store: store, creds: creds, region: region}
}

func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.route) }

func (s *Server) route(w http.ResponseWriter, r *http.Request) {
	if _, aerr := verifySigV4(r, s.creds); aerr != nil {
		writeError(w, r, *aerr)
		return
	}
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
		s.mapErr(w, r, s.store.HeadBucket(r.Context(), bucket))
	case http.MethodPut:
		s.mapErr(w, r, s.store.CreateBucket(r.Context(), bucket))
	case http.MethodDelete:
		if err := s.store.DeleteBucket(r.Context(), bucket); err != nil {
			s.mapErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, r, errNotImplemented)
	}
}

func (s *Server) objectOp(w http.ResponseWriter, r *http.Request, bucket, key string) {
	switch r.Method {
	case http.MethodPut:
		s.putObject(w, r, bucket, key)
	case http.MethodGet:
		s.getObject(w, r, bucket, key)
	case http.MethodHead:
		s.headObject(w, r, bucket, key)
	case http.MethodDelete:
		if err := s.store.DeleteObject(r.Context(), bucket, key); err != nil {
			s.mapErr(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		writeError(w, r, errNotImplemented)
	}
}

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request) {
	buckets, err := s.store.ListBuckets(r.Context())
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

	res, err := s.store.ListObjects(r.Context(), bucket, prefix, delimiter, startAfter, maxKeys)
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
	obj, err := s.store.PutObject(r.Context(), bucket, key, size, body, r.Header.Get("Content-Type"))
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
	rc, obj, err := s.store.GetObject(r.Context(), bucket, key)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	defer rc.Close()
	setObjectHeaders(w, obj)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, rc)
}

func (s *Server) headObject(w http.ResponseWriter, r *http.Request, bucket, key string) {
	obj, err := s.store.HeadObject(r.Context(), bucket, key)
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
