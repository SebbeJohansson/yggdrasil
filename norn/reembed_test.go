package norn

import (
	"context"
	"testing"
)

func TestReembedGivesOldMemoriesVectorsFromTheNewModel(t *testing.T) {
	store, err := OpenStore(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	_, _ = store.AddMemory(ctx, "My manager is Kim", "manual", []float64{1, 0}, "nomic-embed-text")
	_, _ = store.AddMemory(ctx, "I work on the widget", "manual", nil, "")
	_, _ = store.AddMemory(ctx, "Already new", "manual", []float64{0, 1}, "nomic-embed-text-v1.5")
	asked := 0
	brain := &Brain{Store: store, EmbedModel: "nomic-embed-text-v1.5", Warn: t.Logf,
		Embed: func(_ context.Context, texts []string) ([][]float64, error) {
			asked += len(texts)
			vectors := make([][]float64, len(texts))
			for i := range texts {
				vectors[i] = []float64{0.5, 0.5}
			}
			return vectors, nil
		}}
	if err := brain.Reembed(ctx); err != nil {
		t.Fatal(err)
	}
	if asked != 2 {
		t.Fatalf("embedded %d memories, want the 2 stale ones", asked)
	}
	memories, _ := store.Memories(ctx)
	for _, m := range memories {
		if m.Model != "nomic-embed-text-v1.5" || m.Embedding == nil {
			t.Errorf("%q: %q %v", m.Text, m.Model, m.Embedding)
		}
	}
	asked = 0
	_ = brain.Reembed(ctx)
	if asked != 0 {
		t.Fatalf("embedded again: %d", asked)
	}
}
