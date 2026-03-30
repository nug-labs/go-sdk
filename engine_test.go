package nuglabs

import (
	"context"
	"testing"
)

func TestEngineGetStrain(t *testing.T) {
	ctx := context.Background()
	e, err := Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close(ctx)

	if err := e.LoadBundledRules(ctx); err != nil {
		t.Fatal(err)
	}
	if err := e.LoadBundledDataset(ctx); err != nil {
		t.Fatal(err)
	}

	s, err := e.GetStrain(ctx, "bd")
	if err != nil {
		t.Fatal(err)
	}
	if s == nil || s["name"] != "Blue Dream" {
		t.Fatalf("got %+v", s)
	}

	all, err := e.GetAllStrains(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) == 0 {
		t.Fatal("expected bundled dataset to include strains")
	}

	hits, err := e.SearchStrains(ctx, "dream")
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) == 0 {
		t.Fatal("expected partial search hits for 'dream'")
	}

	actions, err := e.TickActions(ctx, 1_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(actions) == 0 {
		t.Fatal("expected at least one sync action")
	}
}
