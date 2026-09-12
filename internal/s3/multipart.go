package s3

import (
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
)

type initiateResult struct {
	XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	UploadID string   `xml:"UploadId"`
}

type completeRequest struct {
	XMLName xml.Name `xml:"CompleteMultipartUpload"`
	Parts   []struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	} `xml:"Part"`
}

type completeResult struct {
	XMLName  xml.Name `xml:"CompleteMultipartUploadResult"`
	XMLNS    string   `xml:"xmlns,attr"`
	Location string   `xml:"Location"`
	Bucket   string   `xml:"Bucket"`
	Key      string   `xml:"Key"`
	ETag     string   `xml:"ETag"`
}

func (s *Server) createMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	id, err := s.st(r).CreateMultipartUpload(r.Context(), bucket, key)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, initiateResult{XMLNS: s3NS, Bucket: bucket, Key: key, UploadID: id})
}

func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || partNumber < 1 {
		writeError(w, r, errInvalidRequest)
		return
	}
	uploadID := r.URL.Query().Get("uploadId")
	body, size := bodyReader(r)
	et, err := s.st(r).UploadPart(r.Context(), uploadID, partNumber, size, body)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	if et != "" {
		w.Header().Set("ETag", etag(et))
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) completeMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	uploadID := r.URL.Query().Get("uploadId")
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	var req completeRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeError(w, r, errInvalidRequest)
		return
	}
	parts := make([]int, 0, len(req.Parts))
	for _, p := range req.Parts {
		parts = append(parts, p.PartNumber)
	}
	obj, err := s.st(r).CompleteMultipartUpload(r.Context(), bucket, key, uploadID, parts)
	if err != nil {
		s.mapErr(w, r, err)
		return
	}
	writeXML(w, http.StatusOK, completeResult{
		XMLNS: s3NS, Location: "/" + bucket + "/" + key, Bucket: bucket, Key: key, ETag: etag(obj.ETag),
	})
}

func (s *Server) abortMultipart(w http.ResponseWriter, r *http.Request, bucket, key string) {
	if err := s.st(r).AbortMultipartUpload(r.Context(), r.URL.Query().Get("uploadId")); err != nil {
		s.mapErr(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
