package githubapp

import (
	"crypto/rand"
	"io"
)

type cryptoRandomReader struct{}

func (cryptoRandomReader) Read(p []byte) (int, error) { return rand.Read(p) }

func readRandom(r io.Reader, p []byte) error {
	_, err := io.ReadFull(r, p)
	return err
}
