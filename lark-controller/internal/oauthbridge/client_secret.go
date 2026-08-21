package oauthbridge

import (
	"errors"
	"io"
	"os"
)

const (
	minimumBridgeClientSecretSize = 32
	maximumBridgeClientSecretSize = 4096
)

func LoadClientSecretFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("OAuth bridge client secret file is required")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", errors.New("read OAuth bridge client secret file")
	}
	defer func() { _ = file.Close() }()
	contents, err := io.ReadAll(io.LimitReader(file, maximumBridgeClientSecretSize+3))
	if err != nil {
		return "", errors.New("read OAuth bridge client secret file")
	}
	defer func() {
		for index := range contents {
			contents[index] = 0
		}
	}()
	secret := contents
	if len(secret) > 0 && secret[len(secret)-1] == '\n' {
		secret = secret[:len(secret)-1]
		if len(secret) > 0 && secret[len(secret)-1] == '\r' {
			secret = secret[:len(secret)-1]
		}
	}
	if !validBridgeClientSecret(string(secret)) {
		return "", errors.New("OAuth bridge client secret must be one printable ASCII token between 32 and 4096 bytes")
	}
	return string(secret), nil
}

func validBridgeClientSecret(secret string) bool {
	if len(secret) < minimumBridgeClientSecretSize || len(secret) > maximumBridgeClientSecretSize {
		return false
	}
	for index := range secret {
		if secret[index] < '!' || secret[index] > '~' {
			return false
		}
	}
	return true
}
