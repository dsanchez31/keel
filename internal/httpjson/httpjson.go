// Package httpjson is the JSON edge of KEEL's HTTP endpoints: strict request
// bodies in, canonical answers out, errors as RFC 9457 problem details.
//
// The daemon's REST API (internal/transport) and keelsim's fault endpoint
// both answer through it, so a body one of them refuses the other refuses
// too, with the same status and the same detail. The closed-schema check
// itself is internal/strictjson's; this package adds what HTTP puts around
// it: the media type, the size bound, the status codes.
package httpjson

import (
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"

	"github.com/dsanchez31/keel/internal/eventlog"
	"github.com/dsanchez31/keel/internal/strictjson"
)

// MaxBodyBytes bounds a request body. The largest KEEL takes is an intent, a
// few hundred bytes.
const MaxBodyBytes = 1 << 20

// Problem is an error answer, RFC 9457 problem details with the default type
// "about:blank": Title is the HTTP status text and Detail says what went
// wrong. An endpoint needing an extension member declares its own type with
// these three fields and writes it through WriteJSON.
type Problem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// ReadBody decodes a request body strictly as T: JSON media type (else 415),
// at most MaxBodyBytes (else 413), one value, exact keys, no unknown field,
// every field without omitempty present (else 400). A refusal is answered
// here and reported as false.
func ReadBody[T any](w http.ResponseWriter, r *http.Request) (T, bool) {
	var v T
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "application/json" {
		WriteProblem(w, http.StatusUnsupportedMediaType, "the body must be application/json")
		return v, false
	}
	b, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
			WriteProblem(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body exceeds %d bytes", MaxBodyBytes))
		} else {
			WriteProblem(w, http.StatusBadRequest, "reading the body: "+err.Error())
		}
		return v, false
	}
	if v, err = strictjson.Decode[T](b, "body"); err != nil {
		WriteProblem(w, http.StatusBadRequest, err.Error())
		return v, false
	}
	return v, true
}

// WriteProblem answers an error as problem details.
func WriteProblem(w http.ResponseWriter, status int, detail string) {
	WriteJSON(w, status, Problem{Title: http.StatusText(status), Status: status, Detail: detail})
}

// WriteJSON answers through the canonical encoder, the one encoder of the
// repository: sorted keys, no null, no NaN. A status of 400 and above is sent
// as application/problem+json. A value the encoder refuses is a bug upstream,
// answered 500.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	b, err := eventlog.Canonical(v)
	if err != nil {
		status = http.StatusInternalServerError
		b = eventlog.MustCanonical(Problem{Title: http.StatusText(status), Status: status, Detail: "internal error"})
	}
	contentType := "application/json"
	if status >= http.StatusBadRequest {
		contentType = "application/problem+json"
	}
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(status)
	_, _ = w.Write(b)
}
