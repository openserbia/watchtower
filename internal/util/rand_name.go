// Package util contains small utility helpers shared across watchtower.
package util

import "math/rand"

const randNameLength = 32

var letters = []rune("abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ")

// RandName Generates a random, 32-character, Docker-compatible container name.
func RandName() string {
	b := make([]rune, randNameLength)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}

	return string(b)
}

// IsRandName reports whether s has the exact shape produced by RandName():
// 32 characters drawn from the [a-zA-Z] alphabet. Used by the self-update
// safety net to detect that a container's *cached* Name was set by a prior
// rename-and-respawn cycle (not by the operator) so the canonical name can
// be re-derived from compose labels instead of being faithfully propagated.
//
// Conservative: extra-strict so a legitimate 32-char operator-chosen name
// won't be mistaken for a rename target. There is a non-zero false-positive
// risk for operators who literally name their container 32 mixed-case
// letters; that case is rare enough that the trade is acceptable, and the
// fallback when ComposeService() returns "" preserves the original name
// anyway.
func IsRandName(s string) bool {
	if len(s) != randNameLength {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') {
			return false
		}
	}
	return true
}
