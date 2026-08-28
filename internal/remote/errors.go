package remote

import (
	"errors"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// ErrNotFound is returned, wrapped so errors.Is finds it, by Head and Get
// when the key does not exist. Callers need to tell "not there" apart from
// "the network broke" -- a missing key in a restore plan means skip it or
// report it gone, where a network error means retry -- and the SDK's raw
// error does not make that distinction easy to check.
var ErrNotFound = errors.New("remote: object not found")

// isNotFound reports whether err represents a missing key.
//
// GetObject models the "no such key" case with a typed error
// (types.NoSuchKey). HeadObject cannot: a HEAD response has no body to hang
// a modeled error off, so S3 (and R2) answer a missing key with a bare 404
// and the SDK surfaces only the HTTP status. Both cases are checked here so
// callers do not need to know which operation they came from.
func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var nf *types.NotFound
	if errors.As(err, &nf) {
		return true
	}
	var re *smithyhttp.ResponseError
	if errors.As(err, &re) && re.HTTPStatusCode() == http.StatusNotFound {
		return true
	}
	return false
}
