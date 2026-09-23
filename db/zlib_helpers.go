package db

import (
	"bytes"
	"compress/zlib"
	"crypto/rand"
	"io"
)

func newZlibWriter(w io.Writer) *zlib.Writer {
	return zlib.NewWriter(w)
}

func zlibDecompress(data []byte) ([]byte, error) {
	r, err := zlib.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer r.Close()
	return io.ReadAll(r)
}

func readRandom(b []byte) (int, error) {
	return rand.Read(b)
}
