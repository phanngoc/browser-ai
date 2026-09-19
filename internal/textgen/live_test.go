package textgen

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/phanngoc/browser-ai/internal/snapshot"
)

// TestLive hits the real text model. Skipped unless TEXT_MODEL_API_KEY is set.
func TestLive(t *testing.T) {
	key := os.Getenv("TEXT_MODEL_API_KEY")
	if key == "" {
		t.Skip("TEXT_MODEL_API_KEY not set")
	}
	c := New(key, os.Getenv("TEXT_MODEL_BASE_URL"), os.Getenv("TEXT_MODEL"), os.Getenv("TEXT_MODEL_REASONING"))
	page := &snapshot.Page{Title: "Google Flights", Text: "Round trip  1 passenger  Economy\nWhere from?\nWhere to?\nDeparture\nSearch"}
	fc := FieldContext("Find one-way flights from Zurich to London on September 20, 2026, for one adult in economy.",
		snapshot.Action{Label: "Where from?", Role: "combobox", Value: ""}, page, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	v, meta, err := c.Text(ctx, fc)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("model=%s latency=%s value=%q usage=%+v", meta.Model, meta.Latency, v, meta.Usage)
	if v != "Zurich" && v != "Zürich" && v != "Zurich, Switzerland" {
		t.Errorf("unexpected value %q", v)
	}
}
