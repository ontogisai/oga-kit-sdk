// Package ontology offers a small convenience layer over
// [github.com/ontogisai/oga-kit-sdk/transfer.Writer] for kit authors
// who think in "register a batch of types" terms.
//
// Most ontology loaders look like this:
//
//	func (l *MyOntologyLoader) Load(ctx context.Context, lc *loader.LoadContext) (*loader.LoadResponse, error) {
//	    types := buildEntityTypeDefs(...)
//	    hierarchy := buildHierarchy(...)
//	    reg := ontology.NewRegistrar(lc.Transfer)
//	    if err := reg.RegisterTypes(ctx, ontology.RegisterTypesRequest{
//	        EntityTypes:   types,
//	        TypeHierarchy: hierarchy,
//	    }); err != nil {
//	        return failed(err)
//	    }
//	    return ok(), nil // SDK closes lc.Transfer; commits via the platform's loader.* tools
//	}
//
// The registrar itself never closes the underlying writer. The SDK's
// loader server closes the writer after the kit handler returns, and
// the platform's dispatcher consumes the artifact (atomic DDL +
// EntityTypeDef + activation) asynchronously.
package ontology
