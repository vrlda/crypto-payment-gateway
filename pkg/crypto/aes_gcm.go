package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"io"
	"os"
)

// Encrypt encrypts plain text string using AES-GCM with the key from APP_SECRET env
func Encrypt(plainText string) (string, error) {
	keyStr := os.Getenv("APP_SECRET")
	if len(keyStr) == 0 {
		return "", errors.New("APP_SECRET is not set")
	}
	key := deriveKey(keyStr)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	cipherText := gcm.Seal(nonce, nonce, []byte(plainText), nil)
	return base64.StdEncoding.EncodeToString(cipherText), nil
}

// Decrypt decrypts base64 encoded cipher text
func Decrypt(cipherTextB64 string) (string, error) {
	keyStr := os.Getenv("APP_SECRET")
	if len(keyStr) == 0 {
		return "", errors.New("APP_SECRET is not set")
	}
	key := deriveKey(keyStr)

	cipherText, err := base64.StdEncoding.DecodeString(cipherTextB64)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	if len(cipherText) < gcm.NonceSize() {
		return "", errors.New("malformed ciphertext")
	}

	nonce, cipherText := cipherText[:gcm.NonceSize()], cipherText[gcm.NonceSize():]
	plainText, err := gcm.Open(nil, nonce, cipherText, nil)
	if err != nil {
		return "", err
	}

	return string(plainText), nil
}

// EncryptWithEnv encrypts plain text string using AES-GCM with the key from specified env var
func EncryptWithEnv(plainText, envVar string) (string, error) {
	keyStr := os.Getenv(envVar)
	if len(keyStr) == 0 {
		return "", errors.New(envVar + " is not set")
	}

	key := deriveKey(keyStr)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	nonce := make([]byte, gcm.NonceSize())
	if _, err = io.ReadFull(rand.Reader, nonce); err != nil {
		return "", err
	}

	cipherText := gcm.Seal(nonce, nonce, []byte(plainText), nil)
	return base64.StdEncoding.EncodeToString(cipherText), nil
}

// DecryptWithEnv decrypts base64 encoded cipher text using key from specified env var
func DecryptWithEnv(cipherTextB64, envVar string) (string, error) {
	keyStr := os.Getenv(envVar)
	if len(keyStr) == 0 {
		return "", errors.New(envVar + " is not set")
	}
	key := deriveKey(keyStr)

	cipherText, err := base64.StdEncoding.DecodeString(cipherTextB64)
	if err != nil {
		return "", err
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}

	if len(cipherText) < gcm.NonceSize() {
		return "", errors.New("malformed ciphertext")
	}

	nonce, cipherText := cipherText[:gcm.NonceSize()], cipherText[gcm.NonceSize():]
	plainText, err := gcm.Open(nil, nonce, cipherText, nil)
	if err != nil {
		return "", err
	}

	return string(plainText), nil
}

// deriveKey hashes the input string to get a consistent 32-byte key for AES-256
func deriveKey(secret string) []byte {
	hash := sha256.Sum256([]byte(secret))
	return hash[:]
}
