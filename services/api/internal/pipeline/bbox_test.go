package pipeline

import (
	"encoding/json"
	"testing"

	ingestionpb "github.com/tanzeelgcuf/ai-auditor/services/api/genproto/ingestion"
)

// Round 7 traceability guard: an entity's OCR geometry must survive into the
// bbox column so the PDF overlay can highlight the cited total line. The column
// was previously hardcoded '{}' — real coordinates were dropped at persistence.
func TestBboxJSON_PersistsGeometry(t *testing.T) {
	e := &ingestionpb.ExtractedEntity{
		Bbox: &ingestionpb.BoundingBox{X: 0.1, Y: 0.2, Width: 0.3, Height: 0.4},
	}
	got := bboxJSON(e)
	var m map[string]float64
	if err := json.Unmarshal([]byte(got), &m); err != nil {
		t.Fatalf("bboxJSON(%v) = %q, not valid JSON: %v", e, got, err)
	}
	if m["x"] != 0.1 || m["y"] != 0.2 || m["width"] != 0.3 || m["height"] != 0.4 {
		t.Errorf("bboxJSON = %v, want x=0.1 y=0.2 width=0.3 height=0.4", m)
	}
}

func TestBboxJSON_EmptyWhenNil(t *testing.T) {
	e := &ingestionpb.ExtractedEntity{}
	if got := bboxJSON(e); got != "{}" {
		t.Errorf("bboxJSON(nil bbox) = %q, want {}", got)
	}
}
