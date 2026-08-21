package newapi

import (
	"errors"
	"os"
)

func LoadIntegrationSecretFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("New API integration secret file is required")
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", errors.New("read New API integration secret file")
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
	if !validIntegrationSecret(secret) {
		return "", errors.New("New API integration secret must be one printable ASCII token of at least 32 bytes")
	}
	return string(secret), nil
}

func validIntegrationSecret(secret []byte) bool {
	if len(secret) < minimumSecretBytes {
		return false
	}
	for _, character := range secret {
		if character < '!' || character > '~' {
			return false
		}
	}
	return true
}
