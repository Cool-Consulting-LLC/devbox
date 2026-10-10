package gcp

import (
	"errors"
	"fmt"
	"net/url"
	"testing"

	"golang.org/x/oauth2"
)

func TestNeedsLogin(t *testing.T) {
	rapt := &oauth2.RetrieveError{ErrorCode: "invalid_grant", ErrorDescription: "reauth related error (invalid_rapt)"}
	for _, tc := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New("googleapi: Error 404: not found"), false},
		{rapt, true},
		{fmt.Errorf("get instance: %w", &url.Error{Op: "Get", URL: "https://compute.googleapis.com", Err: rapt}), true},
	} {
		if got := NeedsLogin(tc.err); got != tc.want {
			t.Errorf("NeedsLogin(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}
