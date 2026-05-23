package util

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRandName_ShapeMatchesIsRandName(t *testing.T) {
	// Every RandName() output must be classified as random by IsRandName —
	// otherwise the self-update safety net's "is this a previous rename
	// target?" heuristic would miss every actual previous rename target.
	for range 50 {
		name := RandName()
		assert.True(t, IsRandName(name), "RandName output %q should be IsRandName(true)", name)
	}
}

func TestIsRandName_RejectsCanonicalNames(t *testing.T) {
	cases := []string{
		"watchtower",
		"my-app",
		"my_service_1",
		"",
		"a",                                 // too short
		"thisStringIsExactly32CharsLong00",  // 32 chars but contains digits
		"thisStringIs31CharsLongMixedCAS",   // 31 chars, all letters
		"thisStringIs33CharsLongMixedCASEE", // 33 chars
	}
	for _, n := range cases {
		assert.False(t, IsRandName(n), "IsRandName(%q) should be false", n)
	}
}

func TestIsRandName_AcceptsExactRandShape(t *testing.T) {
	// 32 chars, all in [a-zA-Z], exactly RandName's output shape.
	cases := []string{
		"abcdefghijklmnopqrstuvwxyzABCDEF",
		"VWhtejHFazORFJVQPmEDXTirLeVHxFAz", // an actual observed AX41 rename target
		"sqlAmSZchFmMBUsqhtIwjoGGwWAmQQWo", // another from today's incident
	}
	for _, n := range cases {
		assert.True(t, IsRandName(n), "IsRandName(%q) should be true", n)
	}
}
