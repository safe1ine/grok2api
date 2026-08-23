package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"fmt"
)

// encryptor 用 AES-GCM 加解密 refresh_token。
type encryptor struct {
	aead cipher.AEAD
}

func newEncryptor(key []byte) (*encryptor, error) {
	// key 需 32 字节
	if len(key) != 32 {
		return nil, fmt.Errorf("encryption key 必须为 32 字节，实际 %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &encryptor{aead: aead}, nil
}

func (e *encryptor) Encrypt(plain []byte) ([]byte, error) {
	nonce := make([]byte, e.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return e.aead.Seal(nonce, nonce, plain, nil), nil
}

func (e *encryptor) Decrypt(ciphertext []byte) ([]byte, error) {
	ns := e.aead.NonceSize()
	if len(ciphertext) < ns {
		return nil, fmt.Errorf("密文过短")
	}
	nonce, ct := ciphertext[:ns], ciphertext[ns:]
	return e.aead.Open(nil, nonce, ct, nil)
}
