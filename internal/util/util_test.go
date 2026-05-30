package util

import (
	"testing"

	. "github.com/onsi/gomega"
)

func TestSliceEqual_True(t *testing.T) {
	g := NewWithT(t)
	s1 := []string{"a", "b", "c"}
	s2 := []string{"a", "b", "c"}

	g.Expect(SliceEqual(s1, s2)).To(BeTrue())
}

func TestSliceEqual_DifferentLengths(t *testing.T) {
	g := NewWithT(t)
	s1 := []string{"a", "b", "c"}
	s2 := []string{"a", "b", "c", "d"}

	g.Expect(SliceEqual(s1, s2)).To(BeFalse())
}

func TestSliceEqual_DifferentContents(t *testing.T) {
	g := NewWithT(t)
	s1 := []string{"a", "b", "c"}
	s2 := []string{"a", "b", "d"}

	g.Expect(SliceEqual(s1, s2)).To(BeFalse())
}

func TestSliceSubtract(t *testing.T) {
	g := NewWithT(t)
	a1 := []string{"a", "b", "c"}
	a2 := []string{"a", "c"}

	result := SliceSubtract(a1, a2)
	g.Expect(result).To(Equal([]string{"b"}))
	g.Expect(a1).To(Equal([]string{"a", "b", "c"}))
	g.Expect(a2).To(Equal([]string{"a", "c"}))
}

func TestStringMapSubtract(t *testing.T) {
	g := NewWithT(t)
	m1 := map[string]string{"a": "a", "b": "b", "c": "sea"}
	m2 := map[string]string{"a": "a", "c": "c"}

	result := StringMapSubtract(m1, m2)
	g.Expect(result).To(Equal(map[string]string{"b": "b", "c": "sea"}))
	g.Expect(m1).To(Equal(map[string]string{"a": "a", "b": "b", "c": "sea"}))
	g.Expect(m2).To(Equal(map[string]string{"a": "a", "c": "c"}))
}

func TestStructMapSubtract(t *testing.T) {
	g := NewWithT(t)
	x := struct{}{}
	m1 := map[string]struct{}{"a": x, "b": x, "c": x}
	m2 := map[string]struct{}{"a": x, "c": x}

	result := StructMapSubtract(m1, m2)
	g.Expect(result).To(Equal(map[string]struct{}{"b": x}))
	g.Expect(m1).To(Equal(map[string]struct{}{"a": x, "b": x, "c": x}))
	g.Expect(m2).To(Equal(map[string]struct{}{"a": x, "c": x}))
}

// GenerateRandomSHA256 generates a random 64 character SHA 256 hash string
func TestGenerateRandomSHA256(t *testing.T) {
	g := NewWithT(t)
	res := GenerateRandomSHA256()
	g.Expect(res).To(HaveLen(64))
	g.Expect(res).NotTo(ContainSubstring("sha256:"))
}

func TestGenerateRandomPrefixedSHA256(t *testing.T) {
	g := NewWithT(t)
	res := GenerateRandomPrefixedSHA256()
	g.Expect(res).To(MatchRegexp("sha256:[0-9|a-f]{64}"))
}
