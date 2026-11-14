package transfer_test

import (
	"context"
	"fmt"

	"github.com/ontogisai/oga-kit-sdk/transfer"
)

// A loader that sends its parents and their children as two artifacts waits
// for the parents' job before it commits the children, so every child's edge
// finds its parent already written. In a sidecar the client is the
// *transfer.HTTPCommitClient the writers commit through.
func ExampleWaitForJob() {
	ctx := context.Background()
	client := &transfer.FakeCommitClient{}

	parents := transfer.NewDataWriter(client, "my-kit")
	_ = parents.WriteVertex(ctx, transfer.Vertex{ID: "site-1", EntityType: "ex:Site"})
	receipt, err := parents.Close(ctx)
	if err != nil {
		fmt.Println("commit:", err)
		return
	}
	st, err := transfer.WaitForJob(ctx, client, receipt, transfer.WaitOptions{})
	if err != nil {
		fmt.Println("wait:", err)
		return
	}
	fmt.Println(st.JobID, st.Status)

	children := transfer.NewDataWriter(client, "my-kit")
	_ = children.WriteVertex(ctx, transfer.Vertex{ID: "room-1", EntityType: "ex:Room"})
	_ = children.WriteEdge(ctx, transfer.Edge{RelationshipType: "ex:isPartOf", SourceID: "room-1", TargetID: "site-1"})
	if _, err := children.Close(ctx); err != nil {
		fmt.Println("commit:", err)
	}
	// Output: job-1 completed
}
