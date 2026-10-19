package shacrypt

import (
	"regexp"
	"strings"
	"testing"
)

// Expected values from `openssl passwd -6 -salt SALT PASSWORD`.
func TestHash(t *testing.T) {
	for _, tc := range []struct{ key, salt, want string }{
		{"Hello world!", "saltstring", "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"},
		{strings.Repeat("x", 150), "q8L4n0mVq2zXp7Ty", "$6$q8L4n0mVq2zXp7Ty$XbA.IQCxro4C26jTfj8mPJSCtNUgwSZDMQ6A65HoPH2Y/XhZsDAac7SHbHmY2zColicwUNohiKmDytQ9grwj7."},
	} {
		if got := hash([]byte(tc.key), []byte(tc.salt)); got != tc.want {
			t.Errorf("hash(%q, %q) = %s, want %s", tc.key, tc.salt, got, tc.want)
		}
	}
}

// provision.sh refuses anything that does not match this.
func TestHashFormat(t *testing.T) {
	h := Hash([]byte("pw"))
	if !regexp.MustCompile(`^\$6\$[./A-Za-z0-9]{16}\$[./A-Za-z0-9]{86}$`).MatchString(h) {
		t.Fatalf("Hash = %s", h)
	}
	if h == Hash([]byte("pw")) {
		t.Fatal("same salt twice")
	}
}
