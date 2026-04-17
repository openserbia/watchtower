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
