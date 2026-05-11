// Package tenantname generates user-friendly tenant identifiers.
//
// The generator emits three dash-separated groups (e.g. "happy-amber-otter")
// drawn from curated, slug-safe word lists. When the dictionary path is
// unavailable a "soft" fallback synthesises pronounceable nonsense
// (e.g. "miro-vana-keso") so callers always get a usable name.
//
// Output is safe to display to end users without further sanitisation:
// ASCII lowercase letters and dashes only, matches the project's slug regex.
package tenantname

import (
	"context"
	"crypto/rand"
	"errors"
	"math/big"
	"strings"
)

// SlugTakenFunc reports whether a slug is already in use. The callsite owns
// the "not found" semantics of the underlying store (e.g. sql.ErrNoRows ->
// false, nil) so this package stays decoupled from any DB driver.
type SlugTakenFunc func(ctx context.Context, slug string) (bool, error)

// ErrUniquenessExhausted is returned when the generator cannot find an
// unused slug within the configured attempt budget.
var ErrUniquenessExhausted = errors.New("tenantname: failed to generate a unique slug")

// Generate returns a slug-shaped friendly name composed of three groups
// from the curated word lists: adjective-noun-animal.
func Generate() string {
	return strings.Join([]string{
		pick(adjectives),
		pick(nouns),
		pick(animals),
	}, "-")
}

// GenerateSoft returns a slug-shaped name made of three soft-syllable groups
// (e.g. "miro-vana-keso"). Useful as a fallback or when the curated
// dictionary is intentionally avoided.
func GenerateSoft() string {
	return strings.Join([]string{
		softGroup(),
		softGroup(),
		softGroup(),
	}, "-")
}

// GenerateUnique calls Generate up to maxAttempts times, returning the first
// slug for which taken returns false. Falls through to GenerateSoft on the
// final attempt so the soft-syllable space (≈14M combinations) backstops the
// curated dictionary in case all easy candidates collide.
func GenerateUnique(ctx context.Context, taken SlugTakenFunc, maxAttempts int) (string, error) {
	if maxAttempts < 1 {
		maxAttempts = 8
	}
	for attempt := 0; attempt < maxAttempts; attempt++ {
		var candidate string
		if attempt == maxAttempts-1 {
			candidate = GenerateSoft()
		} else {
			candidate = Generate()
		}
		if taken == nil {
			return candidate, nil
		}
		inUse, err := taken(ctx, candidate)
		if err != nil {
			return "", err
		}
		if !inUse {
			return candidate, nil
		}
	}
	return "", ErrUniquenessExhausted
}

// DisplayName converts a slug-shaped name like "happy-amber-otter" into a
// title-cased label "Happy Amber Otter" suitable for the tenants.name column.
// Input is treated as untrusted for casing purposes only — non-ASCII bytes
// pass through unchanged so callers can hand back any prior name unmodified.
func DisplayName(slug string) string {
	if slug == "" {
		return ""
	}
	parts := strings.Split(slug, "-")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

func softGroup() string {
	// Two consonant-vowel pairs => 4 chars, e.g. "miro".
	var b strings.Builder
	b.Grow(4)
	for i := 0; i < 2; i++ {
		b.WriteByte(softConsonants[randIndex(len(softConsonants))])
		b.WriteByte(softVowels[randIndex(len(softVowels))])
	}
	return b.String()
}

func pick(list []string) string {
	return list[randIndex(len(list))]
}

// randIndex returns a uniformly distributed [0, n) index using crypto/rand.
// On the unreachable error path it falls back to index 0 — this keeps the
// public API panic-free while still producing a valid (if degenerate) slug.
func randIndex(n int) int {
	if n <= 0 {
		return 0
	}
	idx, err := rand.Int(rand.Reader, big.NewInt(int64(n)))
	if err != nil {
		return 0
	}
	return int(idx.Int64())
}

