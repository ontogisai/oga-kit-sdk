package connector_test

import (
	"context"
	"os"

	"github.com/ontogisai/oga-kit-sdk/auth"
	"github.com/ontogisai/oga-kit-sdk/connector"
	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// sitesThenRooms pulls a source's sites as page 1 and its rooms as page 2.
// Every room has an edge to its site, so page 1 asks the server to wait for
// its commit before page 2 is fetched.
type sitesThenRooms struct{}

func (sitesThenRooms) Sync(ctx context.Context, _ connector.Binding, cursor string, em *connector.Emitter) (*connector.SyncResult, error) {
	if cursor == "" {
		if err := em.Entities.WriteVertex(ctx, transfer.Vertex{ID: "site-1", EntityType: "ex:Site"}); err != nil {
			return nil, err
		}
		return &connector.SyncResult{NextCursor: "rooms", HasMore: true, WaitForCommit: true}, nil
	}
	if err := em.Entities.WriteVertex(ctx, transfer.Vertex{ID: "room-1", EntityType: "ex:Room"}); err != nil {
		return nil, err
	}
	if err := em.Entities.WriteEdge(ctx, transfer.Edge{
		RelationshipType: "ex:isPartOf", SourceID: "room-1", TargetID: "site-1",
	}); err != nil {
		return nil, err
	}
	return &connector.SyncResult{NextCursor: "done"}, nil
}

func (sitesThenRooms) Bindings(context.Context) []connector.Binding {
	return []connector.Binding{{ID: "spaces", ExternalSystem: "ex_source", SourceType: "spaces"}}
}
func (sitesThenRooms) Connect(context.Context) error { return nil }
func (sitesThenRooms) HandleWebhook(context.Context, connector.Binding, []byte, *connector.Emitter) error {
	return nil
}
func (sitesThenRooms) Health(context.Context) map[string]connector.Health {
	return map[string]connector.Health{"spaces": {OK: true}}
}

// A page whose records the next pages depend on sets WaitForCommit, and the
// server reads the job's status through the commit client its writers use.
func Example_waitForCommit() {
	ctx := context.Background()
	gatewayURL, tenantID := os.Getenv("PLATFORM_GATEWAY_URL"), os.Getenv("OGA_TENANT_ID")
	provider, stop, err := auth.WorkloadTokenProviderFromEnv(ctx, gatewayURL)
	if err != nil {
		return
	}
	defer stop()
	client, err := transfer.NewHTTPCommitClient(gatewayURL, tenantID, "my-kit",
		transfer.WithTokenProvider(provider))
	if err != nil {
		return
	}
	cfg := &connector.Config{
		Port: "8500",
		WriterFactory: func(context.Context, connector.Binding) (transfer.Writer, error) {
			return transfer.NewDataWriter(client, "my-kit"), nil
		},
		StatusClient: client,
	}
	_ = connector.ListenAndServe(ctx, cfg, sitesThenRooms{})
}
