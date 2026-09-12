// Package crypt provides transparent, streaming authenticated encryption for
// object content. Data is encrypted with XSalsa20-Poly1305 (NaCl secretbox) in
// fixed-size chunks; the key is derived from a passphrase with scrypt. The
// passphrase never leaves the operator's configuration, so the storage backend
// (Google Drive) only ever holds ciphertext.
package crypt

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"

	"crypto/rand"

	"golang.org/x/crypto/nacl/secretbox"
	"golang.org/x/crypto/scrypt"
)

const (
	magic      = "GS3E"
	version    = 1
	chunkSize  = 64 * 1024
	overhead   = secretbox.Overhead // 16
	nonceSize  = 24
	headerSize = len(magic) + 1 + nonceSize // 4 + 1 + 24 = 29
)

type Cipher struct {
	key [32]byte
}

// NewCipher derives an encryption key from a passphrase.
func NewCipher(passphrase string) (*Cipher, error) {
	if passphrase == "" {
		return nil, errors.New("crypt: empty passphrase")
	}
	dk, err := scrypt.Key([]byte(passphrase), []byte("gdrive-s3-crypt-v1"), 1<<15, 8, 1, 32)
	if err != nil {
		return nil, err
	}
	var c Cipher
	copy(c.key[:], dk)
	return &c, nil
}

// EncryptedSize returns the ciphertext length for a plaintext of size n.
func (c *Cipher) EncryptedSize(n int64) int64 {
	chunks := (n + chunkSize - 1) / chunkSize
	return int64(headerSize) + n + chunks*int64(overhead)
}

// DecryptedSize returns the plaintext length for a ciphertext of size t.
func (c *Cipher) DecryptedSize(t int64) int64 {
	data := t - int64(headerSize)
	if data <= 0 {
		return 0
	}
	block := int64(chunkSize + overhead)
	full := data / block
	n := full * chunkSize
	if rem := data % block; rem > int64(overhead) {
		n += rem - int64(overhead)
	}
	return n
}

func (c *Cipher) EncryptReader(src io.Reader) io.Reader {
	return &encReader{key: &c.key, src: src}
}

func (c *Cipher) DecryptReader(src io.Reader) io.Reader {
	return &decReader{key: &c.key, src: src}
}

// Exposed format parameters, used to compute byte offsets for range reads.
const (
	HeaderSize    = headerSize
	ChunkSize     = chunkSize
	ChunkOverhead = overhead
	NonceSize     = nonceSize
)

// ParseHeader reads and validates the stream header, returning the base nonce.
func ParseHeader(r io.Reader) ([nonceSize]byte, error) {
	var base [nonceSize]byte
	hdr := make([]byte, headerSize)
	if _, err := io.ReadFull(r, hdr); err != nil {
		return base, err
	}
	if string(hdr[:len(magic)]) != magic || hdr[len(magic)] != version {
		return base, errors.New("crypt: bad header")
	}
	copy(base[:], hdr[len(magic)+1:])
	return base, nil
}

// DecryptChunks decrypts a ciphertext positioned at the start of chunk
// startCounter, using an already-known base nonce (no header is read). Used to
// serve byte ranges.
func (c *Cipher) DecryptChunks(src io.Reader, base [nonceSize]byte, startCounter uint64) io.Reader {
	return &decReader{key: &c.key, src: src, base: base, counter: startCounter, gotHdr: true}
}

func chunkNonce(base [nonceSize]byte, counter uint64) [nonceSize]byte {
	binary.BigEndian.PutUint64(base[nonceSize-8:], counter)
	return base
}

type encReader struct {
	key     *[32]byte
	src     io.Reader
	base    [nonceSize]byte
	counter uint64
	out     bytes.Buffer
	started bool
	eof     bool
}

func (e *encReader) Read(p []byte) (int, error) {
	for e.out.Len() == 0 && !e.eof {
		if !e.started {
			e.started = true
			_, _ = rand.Read(e.base[:])
			e.out.WriteString(magic)
			e.out.WriteByte(version)
			e.out.Write(e.base[:])
			continue
		}
		buf := make([]byte, chunkSize)
		n, err := io.ReadFull(e.src, buf)
		if n > 0 {
			nonce := chunkNonce(e.base, e.counter)
			e.out.Write(secretbox.Seal(nil, buf[:n], &nonce, e.key))
			e.counter++
		}
		if err != nil {
			e.eof = true
		}
	}
	if e.out.Len() == 0 {
		return 0, io.EOF
	}
	return e.out.Read(p)
}

type decReader struct {
	key     *[32]byte
	src     io.Reader
	base    [nonceSize]byte
	counter uint64
	out     bytes.Buffer
	gotHdr  bool
	eof     bool
}

func (d *decReader) Read(p []byte) (int, error) {
	for d.out.Len() == 0 && !d.eof {
		if !d.gotHdr {
			hdr := make([]byte, headerSize)
			if _, err := io.ReadFull(d.src, hdr); err != nil {
				if err == io.EOF {
					return 0, io.EOF
				}
				return 0, err
			}
			if string(hdr[:len(magic)]) != magic || hdr[len(magic)] != version {
				return 0, errors.New("crypt: bad header")
			}
			copy(d.base[:], hdr[len(magic)+1:])
			d.gotHdr = true
			continue
		}
		buf := make([]byte, chunkSize+overhead)
		n, err := io.ReadFull(d.src, buf)
		if n > 0 {
			nonce := chunkNonce(d.base, d.counter)
			pt, ok := secretbox.Open(nil, buf[:n], &nonce, d.key)
			if !ok {
				return 0, errors.New("crypt: authentication failed")
			}
			d.out.Write(pt)
			d.counter++
		}
		if err != nil {
			d.eof = true
		}
	}
	if d.out.Len() == 0 {
		return 0, io.EOF
	}
	return d.out.Read(p)
}
