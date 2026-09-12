package s3

import (
	"bufio"
	"io"
	"strings"
	"testing"
)

func TestURIEncode(t *testing.T) {
	cases := []struct {
		in          string
		encodeSlash bool
		want        string
	}{
		{"abc", false, "abc"},
		{"a b", false, "a%20b"},
		{"a/b", false, "a/b"},
		{"a/b", true, "a%2Fb"},
		{"key~1_2.3-4", false, "key~1_2.3-4"},
		{"é", false, "%C3%A9"},
	}
	for _, c := range cases {
		if got := uriEncode(c.in, c.encodeSlash); got != c.want {
			t.Errorf("uriEncode(%q, %v) = %q, want %q", c.in, c.encodeSlash, got, c.want)
		}
	}
}

func TestCollapse(t *testing.T) {
	if got := collapse("  a   b  \tc "); got != "a b c" {
		t.Errorf("collapse = %q", got)
	}
}

func TestChunkedReader(t *testing.T) {
	body := "5;chunk-signature=deadbeef\r\nhello\r\n" +
		"6;chunk-signature=cafebabe\r\n world\r\n" +
		"0;chunk-signature=00000000\r\n\r\n"
	cr := &chunkedReader{br: bufio.NewReader(strings.NewReader(body))}
	got, err := io.ReadAll(cr)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != "hello world" {
		t.Errorf("decoded = %q, want %q", got, "hello world")
	}
}

func TestSigningKeyDeterministic(t *testing.T) {
	a := signingKey("secret", "20240101", "us-east-1", "s3")
	b := signingKey("secret", "20240101", "us-east-1", "s3")
	if string(a) != string(b) {
		t.Error("signing key not deterministic")
	}
	if string(a) == string(signingKey("other", "20240101", "us-east-1", "s3")) {
		t.Error("signing key must depend on the secret")
	}
}
