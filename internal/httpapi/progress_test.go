package httpapi

import (
	"context"
	"testing"

	"obsigon/internal/rag"
)

func TestIndexProgressReportsAndCancels(t *testing.T) {
	progress := NewIndexProgress()
	if !progress.Start() {
		t.Fatal("no inició")
	}
	ctx, cancel := context.WithCancel(context.Background())
	progress.BindCancel(cancel)
	progress.Update(rag.IndexProgressEvent{Phase: "embedding", Processed: 2, Total: 5, CurrentFile: "nota.md"})
	snapshot := progress.Snapshot()
	if snapshot["processed"] != 2 || snapshot["total"] != 5 || snapshot["current_file"] != "nota.md" || snapshot["percent"] != float64(40) {
		t.Fatalf("snapshot=%+v", snapshot)
	}
	if !progress.Cancel() {
		t.Fatal("no canceló")
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("el contexto siguió activo")
	}
	progress.Finish(rag.IndexResult{}, context.Canceled)
	snapshot = progress.Snapshot()
	if snapshot["running"] != false || snapshot["phase"] != "cancelled" || snapshot["cancelled"] != true {
		t.Fatalf("cancelled=%+v", snapshot)
	}
}
