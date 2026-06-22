// Package passwd provides password hashing for master auth accounts using
// bcrypt — a memory-hard, salted, adaptive KDF suitable for production. The
// salt and cost are embedded in the returned hash string, so Verify needs only
// the stored hash and the candidate password.
package passwd

import "golang.org/x/crypto/bcrypt"

// Cost is the bcrypt work factor. Higher is slower and more brute-force
// resistant; 12 is a sensible default for an internal service in 2025+.
const Cost = 12

// Hash returns the bcrypt hash of the password.
func Hash(password string) (string, error) {
	b, err := bcrypt.GenerateFromPassword([]byte(password), Cost)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// Verify reports whether password matches the stored bcrypt hash. It is
// constant-time with respect to the hash by virtue of bcrypt's design.
func Verify(password, hash string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
