package tenantname

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
)

// slugRe mirrors the regex enforced by the gateway/HTTP create handlers.
// The generator MUST always emit slug-shaped output so callers can drop the
// result into the slug column without re-validation.
var slugRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func TestGenerateMatchesSlugAndHasThreeGroups(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := Generate()
		if !slugRe.MatchString(got) {
			t.Fatalf("Generate() = %q, not slug-shaped", got)
		}
		if parts := strings.Split(got, "-"); len(parts) != 3 {
			t.Fatalf("Generate() = %q, want 3 dash groups, got %d", got, len(parts))
		}
	}
}

func TestGenerateSoftMatchesSlugAndHasThreeGroups(t *testing.T) {
	for i := 0; i < 200; i++ {
		got := GenerateSoft()
		if !slugRe.MatchString(got) {
			t.Fatalf("GenerateSoft() = %q, not slug-shaped", got)
		}
		parts := strings.Split(got, "-")
		if len(parts) != 3 {
			t.Fatalf("GenerateSoft() = %q, want 3 dash groups", got)
		}
		for _, p := range parts {
			if len(p) != 4 {
				t.Fatalf("GenerateSoft() group %q must be 4 chars", p)
			}
		}
	}
}

func TestGenerateUniqueRetriesUntilFree(t *testing.T) {
	calls := 0
	taken := func(_ context.Context, _ string) (bool, error) {
		calls++
		return calls < 3, nil // first two attempts collide, third succeeds
	}
	got, err := GenerateUnique(context.Background(), taken, 8)
	if err != nil {
		t.Fatalf("GenerateUnique returned error: %v", err)
	}
	if !slugRe.MatchString(got) {
		t.Fatalf("GenerateUnique returned non-slug: %q", got)
	}
	if calls != 3 {
		t.Fatalf("expected 3 lookup calls, got %d", calls)
	}
}

func TestGenerateUniqueExhaustsAttempts(t *testing.T) {
	always := func(_ context.Context, _ string) (bool, error) { return true, nil }
	if _, err := GenerateUnique(context.Background(), always, 3); !errors.Is(err, ErrUniquenessExhausted) {
		t.Fatalf("want ErrUniquenessExhausted, got %v", err)
	}
}

func TestGenerateUniquePropagatesLookupError(t *testing.T) {
	boom := errors.New("db down")
	failing := func(_ context.Context, _ string) (bool, error) { return false, boom }
	if _, err := GenerateUnique(context.Background(), failing, 3); !errors.Is(err, boom) {
		t.Fatalf("want lookup error to propagate, got %v", err)
	}
}

func TestGenerateUniqueNilLookup(t *testing.T) {
	got, err := GenerateUnique(context.Background(), nil, 3)
	if err != nil {
		t.Fatalf("nil lookup should not error: %v", err)
	}
	if !slugRe.MatchString(got) {
		t.Fatalf("nil-lookup result is not slug-shaped: %q", got)
	}
}

func TestDisplayName(t *testing.T) {
	cases := map[string]string{
		"":                  "",
		"happy":             "Happy",
		"happy-amber-otter": "Happy Amber Otter",
		"a-b-c":             "A B C",
	}
	for in, want := range cases {
		if got := DisplayName(in); got != want {
			t.Errorf("DisplayName(%q) = %q, want %q", in, got, want)
		}
	}
}
