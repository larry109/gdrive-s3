package s3

import (
	"encoding/xml"
	"net/http"
)

// APIError is an S3-style error rendered as an XML body with a matching status.
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e APIError) Error() string { return e.Code + ": " + e.Message }

var (
	errNoSuchBucket      = APIError{http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist."}
	errNoSuchKey         = APIError{http.StatusNotFound, "NoSuchKey", "The specified key does not exist."}
	errNoSuchUpload      = APIError{http.StatusNotFound, "NoSuchUpload", "The specified multipart upload does not exist."}
	errBucketExists      = APIError{http.StatusConflict, "BucketAlreadyOwnedByYou", "The bucket already exists."}
	errBucketNotEmpty    = APIError{http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty."}
	errAccessDenied      = APIError{http.StatusForbidden, "AccessDenied", "Access denied."}
	errSignatureMismatch = APIError{http.StatusForbidden, "SignatureDoesNotMatch", "The request signature does not match."}
	errInvalidAccessKey  = APIError{http.StatusForbidden, "InvalidAccessKeyId", "The access key id does not exist."}
	errMissingAuth       = APIError{http.StatusForbidden, "AccessDenied", "Missing authentication credentials."}
	errNotImplemented    = APIError{http.StatusNotImplemented, "NotImplemented", "This operation is not implemented."}
	errInvalidRange      = APIError{http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable."}
	errInternal          = APIError{http.StatusInternalServerError, "InternalError", "We encountered an internal error."}
	errInvalidRequest    = APIError{http.StatusBadRequest, "InvalidRequest", "Invalid request."}
)

type errorBody struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`
}

func writeError(w http.ResponseWriter, r *http.Request, err APIError) {
	writeXML(w, err.Status, errorBody{
		Code: err.Code, Message: err.Message, Resource: r.URL.Path, RequestID: reqID(r),
	})
}

func writeXML(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_ = xml.NewEncoder(w).Encode(v)
}
