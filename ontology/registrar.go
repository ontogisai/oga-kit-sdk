package ontology

import (
	"context"
	"errors"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// Registrar is a small convenience layer over transfer.Writer for
// kit authors who think in "register a batch of types" terms rather
// than "stream entries to a writer." The registrar collapses the
// per-type WriteEntityType + WriteHierarchy calls into a single
// RegisterTypes invocation so the kit code stays declarative.
//
// Internally the registrar uses transfer.Writer; kit authors can mix
// freely with direct lc.Transfer.* calls when they need finer control
// (interleaved writes, custom record ordering, etc.).
type Registrar struct {
	writer transfer.Writer
}

// NewRegistrar wraps a transfer.Writer with the convenience surface.
// The supplied writer is consumed by the registrar but NOT closed —
// the SDK's loader server closes the writer for the kit at the end
// of the request, so RegisterTypes leaves the writer open for any
// follow-up entries the kit may want to emit.
func NewRegistrar(writer transfer.Writer) *Registrar {
	return &Registrar{writer: writer}
}

// RegisterTypes streams every entity-type definition and hierarchy
// entry in req through the underlying writer. The platform-side
// dispatch (atomic DDL + EntityTypeDef + activation) happens when
// the writer is later closed.
//
// Idempotency: the platform key is (tenant, kit_id, content_hash).
// Calling RegisterTypes with the same input on the same kit produces
// the same content_hash and is a no-op on the server.
func (r *Registrar) RegisterTypes(ctx context.Context, req RegisterTypesRequest) error {
	if r == nil || r.writer == nil {
		return errors.New("ontology.Registrar: nil writer")
	}
	if len(req.EntityTypes) == 0 {
		return errors.New("ontology.RegisterTypes: at least one entity type is required")
	}
	for i := range req.EntityTypes {
		t := req.EntityTypes[i]
		if err := r.writer.WriteEntityType(ctx, transfer.EntityTypeDef{
			Name:        t.Name,
			DisplayName: t.DisplayName,
			Description: t.Description,
			ParentType:  t.ParentType,
			Category:    t.Category,
			Properties:  toTransferProperties(t.Properties),
		}); err != nil {
			return err
		}
	}
	for i := range req.TypeHierarchy {
		h := req.TypeHierarchy[i]
		if err := r.writer.WriteHierarchy(ctx, transfer.HierarchyEntry{
			TypeName:   h.TypeName,
			ParentType: h.ParentType,
		}); err != nil {
			return err
		}
	}
	return nil
}

// RegisterTypesRequest is the input to [Registrar.RegisterTypes].
type RegisterTypesRequest struct {
	// EntityTypes is the type catalog this call registers. Required.
	EntityTypes []EntityTypeDef

	// TypeHierarchy declares parent-child relationships among
	// EntityTypes. Optional — hierarchy can also be encoded on
	// EntityTypeDef.ParentType, but providing the explicit
	// hierarchy lets the platform validate the graph for cycles
	// and missing parents before activating.
	TypeHierarchy []TypeHierarchyEntry
}

// EntityTypeDef is the kit-author-facing entity type definition.
// Mirrors transfer.EntityTypeDef but lives in the ontology package
// so kit code reads as "this is an entity type, not a wire record."
type EntityTypeDef struct {
	Name        string
	DisplayName map[string]string
	Description map[string]string
	ParentType  string
	Category    string
	Properties  []TypeProperty
}

// TypeProperty mirrors transfer.TypeProperty.
type TypeProperty struct {
	Name        string
	Description map[string]string
	Type        string
	Required    bool
}

// TypeHierarchyEntry mirrors transfer.HierarchyEntry.
type TypeHierarchyEntry struct {
	TypeName   string
	ParentType string
}

func toTransferProperties(props []TypeProperty) []transfer.TypeProperty {
	if len(props) == 0 {
		return nil
	}
	out := make([]transfer.TypeProperty, len(props))
	for i, p := range props {
		out[i] = transfer.TypeProperty{
			Name:        p.Name,
			Description: p.Description,
			Type:        p.Type,
			Required:    p.Required,
		}
	}
	return out
}
