package main

import (
	"context"
	"testing"
	"time"

	"github.com/pot-roast-co/gravy/internal/core"
	"github.com/pot-roast-co/gravy/internal/router"
)

type reviewAvailability struct{ cooling bool }

func (a reviewAvailability) IsUnavailable(_ context.Context, provider, _ string, _ time.Time) (bool, error) {
	return a.cooling && provider == "pinned", nil
}

func TestReviewResolverHonorsProjectRestriction(t *testing.T) {
	for _, cooling := range []bool{false, true} {
		r := router.New(reviewAvailability{cooling}, func(core.Route) []core.Choice {
			return []core.Choice{{ProviderID: "global", Model: "default"}}
		}, func(id string) bool { return cooling || id != "pinned" })
		resolve := reviewResolver(r)
		_, err := resolve(context.Background(), core.RouteReview, core.Constraints{Routes: map[core.Route][]core.Choice{
			core.RouteReview: {{ProviderID: "pinned", Model: "reviewer"}},
		}})
		if err == nil {
			t.Fatal("unavailable project reviewer fell back to another bucket")
		}
	}
}

func TestReviewResolverFallsBackWithoutProjectPin(t *testing.T) {
	r := router.New(reviewAvailability{}, func(route core.Route) []core.Choice {
		if route == core.RouteImplementation {
			return []core.Choice{{ProviderID: "global", Model: "default"}}
		}
		return nil
	}, nil)
	choice, err := reviewResolver(r)(context.Background(), core.RouteReview, core.Constraints{})
	if err != nil || choice.ProviderID != "global" {
		t.Fatalf("fallback: %+v, %v", choice, err)
	}
}
