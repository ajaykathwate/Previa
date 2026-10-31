package secrets

import (
	"bufio"
	"crypto/cipher"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// Streams encrypt data too large to seal in one piece, such as a test-data
// baseline's database dump (docs/32-persistent-test-data.md), with the same
// versioned keys as repository secrets. A stream is a header, then chunks of
// at most streamChunk bytes, each sealed with AES-256-GCM. A chunk's nonce is
// the stream's random prefix, its counter and whether it is the last chunk, so
// chunks cannot be reordered, dropped or repeated and a truncated stream is
// detected. The associated data binds the stream to its repository and
// purpose, so a file cannot be opened as another repository's or another
// purpose's.

const (
	streamMagic   = "PVST"
	streamVersion = 1
	streamChunk   = 64 << 10
	streamPrefix  = 7
	// header: magic, format version, key version, nonce prefix.
	streamHeader = len(streamMagic) + 1 + 4 + streamPrefix
)

// ErrStreamCorrupt hides why a stream failed to open: a wrong key, tampering,
// truncation or a stream of another repository or purpose look the same.
var ErrStreamCorrupt = errors.New("encrypted stream is corrupt, truncated or not for this purpose")

func streamData(repoID int64, purpose string, version int32) []byte {
	return []byte("previa:stream:v1\x00" + strconv.FormatInt(repoID, 10) + "\x00" + purpose + "\x00" + strconv.FormatInt(int64(version), 10))
}

func streamNonce(prefix []byte, counter uint32, last bool) []byte {
	nonce := make([]byte, nonceBytes)
	copy(nonce, prefix)
	binary.BigEndian.PutUint32(nonce[streamPrefix:], counter)
	if last {
		nonce[nonceBytes-1] = 1
	}
	return nonce
}

// EncryptStream returns a writer that encrypts what is written to it into w
// with the active key. Close writes the final chunk; it does not close w.
func (k *Keyring) EncryptStream(w io.Writer, repoID int64, purpose string) (io.WriteCloser, int32, error) {
	prefix := make([]byte, streamPrefix)
	if _, err := rand.Read(prefix); err != nil {
		return nil, 0, fmt.Errorf("generate stream nonce: %w", err)
	}
	header := make([]byte, 0, streamHeader)
	header = append(header, streamMagic...)
	header = append(header, streamVersion)
	header = binary.BigEndian.AppendUint32(header, uint32(k.active))
	header = append(header, prefix...)
	if _, err := w.Write(header); err != nil {
		return nil, 0, err
	}
	return &streamWriter{w: w, aead: k.keys[k.active], prefix: prefix, ad: streamData(repoID, purpose, k.active), buf: make([]byte, 0, streamChunk)}, k.active, nil
}

type streamWriter struct {
	w       io.Writer
	aead    cipher.AEAD
	prefix  []byte
	ad      []byte
	counter uint32
	buf     []byte
	closed  bool
	err     error
}

func (s *streamWriter) Write(p []byte) (int, error) {
	if s.closed {
		return 0, errors.New("write to a closed encrypted stream")
	}
	if s.err != nil {
		return 0, s.err
	}
	written := 0
	for len(p) > 0 {
		// A full buffer is sealed only once more data arrives, so the last
		// chunk is always the one Close seals.
		if len(s.buf) == streamChunk {
			if err := s.seal(false); err != nil {
				return written, err
			}
		}
		n := min(len(p), streamChunk-len(s.buf))
		s.buf = append(s.buf, p[:n]...)
		p, written = p[n:], written+n
	}
	return written, nil
}

func (s *streamWriter) seal(last bool) error {
	if s.counter == ^uint32(0) {
		s.err = errors.New("encrypted stream is too long")
		return s.err
	}
	sealed := s.aead.Seal(nil, streamNonce(s.prefix, s.counter, last), s.buf, s.ad)
	clear(s.buf)
	s.buf = s.buf[:0]
	s.counter++
	if _, err := s.w.Write(sealed); err != nil {
		s.err = err
		return err
	}
	return nil
}

func (s *streamWriter) Close() error {
	if s.closed {
		return s.err
	}
	s.closed = true
	if s.err != nil {
		return s.err
	}
	return s.seal(true)
}

// DecryptStream returns a reader of the plaintext of an encrypted stream
// read from r, for the same repository and purpose. Reading fails with
// ErrStreamCorrupt as soon as a chunk does not authenticate, and at the end
// if the final chunk is missing; ErrUnknownKeyVersion when the stream's key
// is no longer configured.
func (k *Keyring) DecryptStream(r io.Reader, repoID int64, purpose string) (io.Reader, int32, error) {
	header := make([]byte, streamHeader)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, 0, ErrStreamCorrupt
	}
	if string(header[:len(streamMagic)]) != streamMagic || header[len(streamMagic)] != streamVersion {
		return nil, 0, ErrStreamCorrupt
	}
	version := int32(binary.BigEndian.Uint32(header[len(streamMagic)+1:]))
	aead, ok := k.keys[version]
	if !ok {
		return nil, version, ErrUnknownKeyVersion
	}
	return &streamReader{r: bufio.NewReaderSize(r, streamChunk+64), aead: aead, prefix: header[len(header)-streamPrefix:],
		ad: streamData(repoID, purpose, version)}, version, nil
}

type streamReader struct {
	r       *bufio.Reader
	aead    cipher.AEAD
	prefix  []byte
	ad      []byte
	counter uint32
	plain   []byte
	done    bool
	err     error
}

func (s *streamReader) Read(p []byte) (int, error) {
	for len(s.plain) == 0 {
		if s.err != nil {
			return 0, s.err
		}
		if s.done {
			return 0, io.EOF
		}
		s.next()
	}
	n := copy(p, s.plain)
	s.plain = s.plain[n:]
	return n, nil
}

// next opens the next chunk. A chunk is the last one when nothing follows
// it; it must then carry the last-chunk nonce.
func (s *streamReader) next() {
	sealed := make([]byte, streamChunk+s.aead.Overhead())
	n, err := io.ReadFull(s.r, sealed)
	switch {
	case errors.Is(err, io.EOF), n < s.aead.Overhead():
		// The stream ended without its final chunk.
		s.err = ErrStreamCorrupt
		return
	case err != nil && !errors.Is(err, io.ErrUnexpectedEOF):
		s.err = err
		return
	}
	last := errors.Is(err, io.ErrUnexpectedEOF)
	if !last {
		if _, peek := s.r.Peek(1); errors.Is(peek, io.EOF) {
			last = true
		} else if peek != nil {
			s.err = peek
			return
		}
	}
	plain, openErr := s.aead.Open(sealed[:0], streamNonce(s.prefix, s.counter, last), sealed[:n], s.ad)
	if openErr != nil {
		s.err = ErrStreamCorrupt
		return
	}
	s.counter++
	s.plain, s.done = plain, last
}
