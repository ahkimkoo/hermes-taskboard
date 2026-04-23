package migrate

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io"
)

// aeadEncrypt mirrors internal/userdir's AEAD so legacy → new migration
// writes the same envelope format the runtime expects.
func aeadEncrypt(key []byte, plain string) (string, error) {
	if len(key) < 32 {
		return "", errors.New("bad key")
	}
	blk, err := aes.NewCipher(key[:32])
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(blk)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}
	ct := gcm.Seal(nonce, nonce, []byte(plain), nil)
	return base64.StdEncoding.EncodeToString(ct), nil
}

// cryptoRead fills b with crypto/rand bytes. Returned separately so the
// id helper can stub it in tests if needed.
func cryptoRead(b []byte) (int, error) {
	return io.ReadFull(rand.Reader, b)
}
