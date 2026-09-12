package crypt

import (
	"bytes"
	"crypto/rand"
	"io"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	c, err := NewCipher("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	sizes := []int{0, 1, 100, chunkSize - 1, chunkSize, chunkSize + 1, 3*chunkSize + 7}
	for _, n := range sizes {
		plain := make([]byte, n)
		_, _ = rand.Read(plain)

		enc, err := io.ReadAll(c.EncryptReader(bytes.NewReader(plain)))
		if err != nil {
			t.Fatalf("encrypt n=%d: %v", n, err)
		}
		if got := c.EncryptedSize(int64(n)); got != int64(len(enc)) {
			t.Errorf("EncryptedSize(%d) = %d, actual %d", n, got, len(enc))
		}
		if got := c.DecryptedSize(int64(len(enc))); got != int64(n) {
			t.Errorf("DecryptedSize for n=%d = %d", n, got)
		}
		dec, err := io.ReadAll(c.DecryptReader(bytes.NewReader(enc)))
		if err != nil {
			t.Fatalf("decrypt n=%d: %v", n, err)
		}
		if !bytes.Equal(dec, plain) {
			t.Errorf("round trip mismatch for n=%d", n)
		}
	}
}

func TestTamperDetected(t *testing.T) {
	c, _ := NewCipher("pw")
	enc, _ := io.ReadAll(c.EncryptReader(bytes.NewReader([]byte("secret payload"))))
	enc[len(enc)-1] ^= 0xff // flip a ciphertext bit
	if _, err := io.ReadAll(c.DecryptReader(bytes.NewReader(enc))); err == nil {
		t.Error("expected authentication failure on tampered ciphertext")
	}
}

func TestRangeDecrypt(t *testing.T) {
	c, _ := NewCipher("pw")
	plain := make([]byte, 3*chunkSize+123)
	_, _ = rand.Read(plain)
	enc, _ := io.ReadAll(c.EncryptReader(bytes.NewReader(plain)))

	base, err := ParseHeader(bytes.NewReader(enc))
	if err != nil {
		t.Fatal(err)
	}
	// decrypt starting at chunk 2
	startChunk := 2
	off := HeaderSize + startChunk*(ChunkSize+ChunkOverhead)
	dec, err := io.ReadAll(c.DecryptChunks(bytes.NewReader(enc[off:]), base, uint64(startChunk)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dec, plain[startChunk*ChunkSize:]) {
		t.Error("range decrypt mismatch")
	}
}
