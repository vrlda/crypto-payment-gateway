package crypto

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
)

// Default master key environment variable name
const EnvMasterKey = "APP_SECRET" // Using APP_SECRET as the master key source

// EncryptCBC encrypts plain text using AES-256-CBC with PKCS#7 padding.
// It generates a random IV and prepends it to the result (IV + Ciphertext).
// Returns hex-encoded string.
func EncryptCBC(plainText string) (string, error) {
	keyStr := os.Getenv(EnvMasterKey)
	if keyStr == "" {
		return "", errors.New("APP_SECRET environment variable is not set")
	}

	// Ensure key is 32 bytes (256 bits)
	// If the key is hex encoded in env, decode it. If just string, hash it or slice it.
	// For simplicity/robustness, let's assume raw string and hash it to 32 bytes ensures consistent size,
	// OR require user to provide a valid 32-byte key.
	// Matching `aes_gcm.go` behavior or reference behavior is important.
	// Reference `encryption.util.ts` often expects a 32-byte hex string or raw string.
	// We will try to decode hex, if fail, use as raw bytes (padded/truncated).
	key := getKeyBytes(keyStr)

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	// IV needs to be unique, but not secure. It's common to include it at the beginning of the ciphertext.
	iv := make([]byte, aes.BlockSize)
	if _, err := io.ReadFull(rand.Reader, iv); err != nil {
		return "", err
	}

	paddedText := pkcs7Padding([]byte(plainText), aes.BlockSize)
	cipherText := make([]byte, len(paddedText))

	mode := cipher.NewCBCEncrypter(block, iv)
	mode.CryptBlocks(cipherText, paddedText)

	// Return hex(IV + CipherText)
	return hex.EncodeToString(append(iv, cipherText...)), nil
}

// DecryptCBC decrypts a hex-encoded string (IV + Ciphertext) using AES-256-CBC.
func DecryptCBC(encryptedHex string) (string, error) {
	keyStr := os.Getenv(EnvMasterKey)
	if keyStr == "" {
		return "", errors.New("APP_SECRET environment variable is not set")
	}
	key := getKeyBytes(keyStr)

	data, err := hex.DecodeString(encryptedHex)
	if err != nil {
		return "", fmt.Errorf("failed to decode hex: %w", err)
	}

	if len(data) < aes.BlockSize {
		return "", errors.New("cipher text too short")
	}

	iv := data[:aes.BlockSize]
	cipherText := data[aes.BlockSize:]

	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}

	if len(cipherText)%aes.BlockSize != 0 {
		return "", errors.New("cipher text is not a multiple of the block size")
	}

	mode := cipher.NewCBCDecrypter(block, iv)
	// Decrypt in-place
	mode.CryptBlocks(cipherText, cipherText)

	plainText, err := pkcs7Unpadding(cipherText)
	if err != nil {
		return "", err
	}

	return string(plainText), nil
}

// Helper to get 32-byte key from string
func getKeyBytes(keyStr string) []byte {
	// If hex encoded 32-byte key, use it
	if len(keyStr) == 64 {
		if k, err := hex.DecodeString(keyStr); err == nil {
			return k
		}
	}
	// Otherwise derive via SHA256 (consistent with aes_gcm.go)
	hash := sha256.Sum256([]byte(keyStr))
	return hash[:]
}

// PKCS#7 Padding
func pkcs7Padding(ciphertext []byte, blockSize int) []byte {
	padding := blockSize - (len(ciphertext) % blockSize)
	padtext := bytes.Repeat([]byte{byte(padding)}, padding)
	return append(ciphertext, padtext...)
}

func pkcs7Unpadding(src []byte) ([]byte, error) {
	length := len(src)
	if length == 0 {
		return nil, errors.New("unpadding error: input is empty")
	}
	unpadding := int(src[length-1])
	if unpadding > length {
		return nil, errors.New("unpadding error: invalid padding size")
	}
	return src[:(length - unpadding)], nil
}
