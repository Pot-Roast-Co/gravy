package api

import (
	"errors"
	"testing"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/store"
)

// A duplicate approval is refused by the daemon, and the client has to be able to tell that
// refusal apart from a landing that actually failed. The distinction only survives the wire if
// the error kind is carried, so this is the test that keeps it honest.
func TestErrorKindsSurviveTheWire(t *testing.T) {
	for _, tc := range []struct {
		name     string
		err      error
		sentinel error
	}{
		{"already landed", core.ErrAlreadyLanded, core.ErrAlreadyLanded},
		{"wrapped already landed", errors.Join(errors.New("land: x is done"), core.ErrAlreadyLanded), core.ErrAlreadyLanded},
		{"not found", store.ErrNotFound, store.ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := toRPCError(tc.err).toError()
			if !errors.Is(got, tc.sentinel) {
				t.Fatalf("errors.Is lost %v across the wire: %v", tc.sentinel, got)
			}
		})
	}
}

// An ordinary failure must not be mistaken for either sentinel; a landing that failed on
// validation is exactly the case that still needs to read as a failure.
func TestOrdinaryErrorCarriesNoKind(t *testing.T) {
	got := toRPCError(errors.New("land: re-validation: check FAILED (exit 2)")).toError()
	if errors.Is(got, core.ErrAlreadyLanded) || errors.Is(got, store.ErrNotFound) {
		t.Fatalf("a plain failure picked up a sentinel: %v", got)
	}
	if got.Error() == "" {
		t.Fatal("the message was lost")
	}
}
