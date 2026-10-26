package services

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/fernet/fernet-go"
)

// getFernetKey derives the Fernet key from stored secrets (same as API key decryption).
func getFernetKey() (*fernet.Key, error) {
	sDir := secretsDir()
	saltFile := filepath.Join(sDir, ".keystore_salt")
	secretFile := filepath.Join(sDir, ".flask_secret")

	salt, err := os.ReadFile(saltFile)
	if err != nil {
		// Create salt if missing
		salt = make([]byte, 16)
		rand.Read(salt)
		os.WriteFile(saltFile, salt, 0600)
	}
	secret, err := os.ReadFile(secretFile)
	if err != nil {
		return nil, fmt.Errorf("flask_secret introuvable")
	}

	dk := pbkdf2Derive([]byte(strings.TrimSpace(string(secret))), salt, 480000, 32)
	fernetKey := base64Url(dk)
	return fernet.DecodeKey(string(fernetKey))
}

// EncryptSecret encrypts a plaintext string using Fernet.
func EncryptSecret(plaintext string) (string, error) {
	key, err := getFernetKey()
	if err != nil {
		return "", err
	}
	token, err := fernet.EncryptAndSign([]byte(plaintext), key)
	if err != nil {
		return "", err
	}
	return base64.URLEncoding.EncodeToString(token), nil
}

// DecryptSecret decrypts a Fernet-encrypted string.
func DecryptSecret(ciphertext string) (string, error) {
	key, err := getFernetKey()
	if err != nil {
		return "", err
	}
	data, err := base64.URLEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	plaintext := fernet.VerifyAndDecrypt(data, 0, []*fernet.Key{key})
	if plaintext == nil {
		return "", fmt.Errorf("dechiffrement echoue")
	}
	return string(plaintext), nil
}
