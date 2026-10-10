package transfer_test

import (
	"context"
	"fmt"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// writeGoldenRecords writes a deterministic mix of every record kind: vertices
// whose properties carry HTML-significant characters, quotes, non-ASCII text,
// nested values and floats; edges; and, every 50 records, an entity type, a
// relationship type and a hierarchy entry.
//
// testdata/golden-recorder runs this exact function through the released
// v0.98.0-beta writer to produce testdata/golden-v0.98.0-beta*.{json,ndjson}:
// its record.sh copies this file in at run time, so the recorder cannot hold a
// stale copy. Changing anything here means re-running
// transfer/testdata/golden-recorder/record.sh and committing what it writes.
// Map keys are sorted by encoding/json, so the output depends only on n.
func writeGoldenRecords(ctx context.Context, w transfer.Writer, n int) error {
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("asset-%05d", i)
		if err := w.WriteVertex(ctx, transfer.Vertex{
			ID:         id,
			EntityType: "Equipment",
			Label:      fmt.Sprintf("AHU <%d> & \"fan\" – ñ", i),
			Properties: map[string]any{
				"index":  i,
				"ratio":  float64(i) / 7,
				"tags":   []string{"brick:AHU", "<b>", "a&b"},
				"nested": map[string]any{"floor": i % 9, "zone": fmt.Sprintf("Z-%d", i%4)},
			},
		}); err != nil {
			return err
		}
		if i > 0 {
			if err := w.WriteEdge(ctx, transfer.Edge{
				RelationshipType: "feeds",
				SourceID:         id,
				TargetID:         fmt.Sprintf("asset-%05d", i-1),
				Properties:       map[string]any{"weight": i % 3},
			}); err != nil {
				return err
			}
		}
		if i%50 == 0 {
			typeName := fmt.Sprintf("Type%04d", i)
			if err := w.WriteEntityType(ctx, transfer.EntityTypeDef{
				Name:        typeName,
				DisplayName: map[string]string{"en-US": "Type <" + typeName + ">", "vi-VN": "Loại " + typeName},
				ParentType:  "Equipment",
			}); err != nil {
				return err
			}
			if err := w.WriteRelationshipType(ctx, transfer.RelationshipTypeDef{
				Name:       fmt.Sprintf("rel%04d", i),
				SourceType: typeName,
				TargetType: "Equipment",
			}); err != nil {
				return err
			}
			if err := w.WriteHierarchy(ctx, transfer.HierarchyEntry{TypeName: typeName, ParentType: "Equipment"}); err != nil {
				return err
			}
		}
	}
	return nil
}

// goldenCase names one run of writeGoldenRecords recorded against v0.98.0-beta.
type goldenCase struct {
	Name        string `json:"name"`
	Records     int    `json:"records"`
	Mode        string `json:"mode"`
	Bytes       int64  `json:"bytes"`
	EntryCount  int    `json:"entry_count"`
	ContentHash string `json:"content_hash"`
}

// goldenFile is testdata/golden-v0.98.0-beta.json.
type goldenFile struct {
	SDKVersion string       `json:"sdk_version"`
	Kind       string       `json:"kind"`
	KitID      string       `json:"kit_id"`
	Cases      []goldenCase `json:"cases"`
}
