package packet

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"sync"

	"github.com/golang/snappy"
	"github.com/klauspost/compress/flate"
	"github.com/sandertv/gophertunnel/minecraft/internal"
)

// Compression represents a compression algorithm that can compress and decompress data.
type Compression interface {
	// EncodeCompression encodes the compression algorithm into a uint16 ID.
	EncodeCompression() uint16
	// Compress compresses the given data and returns the compressed data.
	Compress(decompressed []byte) ([]byte, error)
	// Decompress decompresses the given data and returns the decompressed data.
	Decompress(compressed []byte) ([]byte, error)
}

var (
	// NopCompression is an empty implementation that does not compress data.
	NopCompression nopCompression
	// FlateCompression is the implementation of the Flate compression
	// algorithm. This is used by default.
	FlateCompression flateCompression
	// SnappyCompression is the implementation of the Snappy compression
	// algorithm. Snappy currently crashes devices without `avx2`.
	SnappyCompression snappyCompression

	DefaultCompression Compression = FlateCompression
)

func NewOnTheFlyCompression(underlyingCompression Compression) Compression {
	return onTheFlyCompression{underlyingCompression}
}

type (
	// nopCompression is an empty implementation that does not compress data.
	nopCompression struct{}
	// flateCompression is the implementation of the Flate compression algorithm.
	flateCompression struct{}
	// snappyCompression is the implementation of the Snappy compression algorithm.
	snappyCompression struct{}
	// onTheFlyCompression is the implementation of the both compression algorithms. This is used by default for decoding.
	onTheFlyCompression struct{ c Compression }
)

var (
	// flateDecompressPool is a sync.Pool for io.ReadCloser flate readers. These are pooled for connections.
	flateDecompressPool = sync.Pool{
		New: func() any { return flate.NewReader(bytes.NewReader(nil)) },
	}
	// flateCompressPool is a sync.Pool for io.ReadCloser flate writers. These are pooled for connections.
	flateCompressPool = sync.Pool{
		New: func() any {
			w, _ := flate.NewWriter(io.Discard, 6)
			return w
		},
	}
)

// EncodeCompression ...
func (nopCompression) EncodeCompression() uint16 {
	return CompressionAlgorithmNone
}

// Compress ...
func (nopCompression) Compress(decompressed []byte) ([]byte, error) {
	return decompressed, nil
}

// Decompress ...
func (nopCompression) Decompress(compressed []byte) ([]byte, error) {
	return compressed, nil
}

// EncodeCompression ...
func (flateCompression) EncodeCompression() uint16 {
	return CompressionAlgorithmFlate
}

// Compress ...
func (flateCompression) Compress(decompressed []byte) ([]byte, error) {
	compressed := internal.BufferPool.Get().(*bytes.Buffer)
	w := flateCompressPool.Get().(*flate.Writer)

	defer func() {
		// Reset the buffer, so we can return it to the buffer pool safely.
		compressed.Reset()
		internal.BufferPool.Put(compressed)
		flateCompressPool.Put(w)
	}()

	w.Reset(compressed)

	_, err := w.Write(decompressed)
	if err != nil {
		return nil, fmt.Errorf("compress flate: %w", err)
	}
	err = w.Close()
	if err != nil {
		return nil, fmt.Errorf("close flate writer: %w", err)
	}
	return append([]byte(nil), compressed.Bytes()...), nil
}

// Decompress ...
func (flateCompression) Decompress(compressed []byte) ([]byte, error) {
	buf := bytes.NewReader(compressed)
	c := flateDecompressPool.Get().(io.ReadCloser)
	defer flateDecompressPool.Put(c)

	if err := c.(flate.Resetter).Reset(buf, nil); err != nil {
		return nil, fmt.Errorf("reset flate: %w", err)
	}
	_ = c.Close()

	// Guess an uncompressed size of 2*len(compressed).
	decompressed := bytes.NewBuffer(make([]byte, 0, len(compressed)*2))
	if _, err := io.Copy(decompressed, c); err != nil {
		return nil, fmt.Errorf("decompress flate: %w", err)
	}
	return decompressed.Bytes(), nil
}

// EncodeCompression ...
func (snappyCompression) EncodeCompression() uint16 {
	return CompressionAlgorithmSnappy
}

// Compress ...
func (snappyCompression) Compress(decompressed []byte) ([]byte, error) {
	// Because Snappy allocates a slice only once, it is less important to have
	// a dst slice pre-allocated. With flateCompression this is more important,
	// because flate does a lot of smaller allocations which causes a
	// considerable slowdown.
	return snappy.Encode(nil, decompressed), nil
}

// Decompress ...
func (snappyCompression) Decompress(compressed []byte) ([]byte, error) {
	// Snappy writes a decoded data length prefix, so it can allocate the
	// perfect size right away and only needs to allocate once. No need to pool
	// byte slices here either.
	decompressed, err := snappy.Decode(nil, compressed)
	if err != nil {
		return nil, fmt.Errorf("decompress snappy: %w", err)
	}
	return decompressed, nil
}

// EncodeCompression ...
func (onTheFlyCompression) EncodeCompression() uint16 {
	return math.MaxUint16
}

// Compress ...
func (c onTheFlyCompression) Compress(decompressed []byte) ([]byte, error) {
	prepend := []byte{byte(c.c.EncodeCompression())}
	compressed, err := c.c.Compress(decompressed)
	if err != nil {
		return nil, err
	}
	return append(prepend, compressed...), nil
}

// Decompress ...
func (onTheFlyCompression) Decompress(compressed []byte) ([]byte, error) {
	var compression Compression
	if compressed[0] != 0xff {
		var ok bool
		compression, ok = CompressionByID(uint16(compressed[0]))
		if !ok {
			return nil, fmt.Errorf("error decompressing packet: unknown compression algorithm %v", compressed[0])
		}
	}
	compressed = compressed[1:]
	if compression != nil {
		return compression.Decompress(compressed)
	}
	return compressed, nil
}

// init registers all valid compressions with the protocol.
func init() {
	RegisterCompression(flateCompression{})
	RegisterCompression(snappyCompression{})
}

var compressions = map[uint16]Compression{}

// RegisterCompression registers a compression so that it can be used by the protocol.
func RegisterCompression(compression Compression) {
	compressions[compression.EncodeCompression()] = compression
}

// CompressionByID attempts to return a compression by the ID it was registered with. If found, the compression found
// is returned and the bool is true.
func CompressionByID(id uint16) (Compression, bool) {
	c, ok := compressions[id]
	if !ok {
		c = DefaultCompression
	}
	return c, ok
}

type CompressionError struct {
	// Op is the operation which caused the error.
	Op string
	// Err is the error that occurred during the operation.
	// The Error method panics if the error is nil.
	Err error
}

func (e *CompressionError) Unwrap() error { return e.Err }

func (e *CompressionError) Error() string {
	if e == nil {
		return "<nil>"
	}
	return e.Op + ": " + e.Err.Error()
}
