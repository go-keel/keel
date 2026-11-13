package pwd

import (
	"github.com/alexedwards/argon2id"
)

var argon2idParams = argon2id.DefaultParams

// HashPassword hashes the given password with the Argon2id algorithm.
func HashPassword(password string) (string, error) {
	hash, err := argon2id.CreateHash(password, argon2idParams)
	if err != nil {
		return "", err
	}
	return hash, nil
}

// VerifyPassword checks whether the given password matches the stored hash.
func VerifyPassword(password, hash string) (bool, error) {
	ok, err := argon2id.ComparePasswordAndHash(password, hash)
	if err != nil {
		return false, err
	}
	return ok, nil
}
