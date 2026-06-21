package mcptools

import (
	"context"
	"testing"
)

func TestContextSettersRoundTrip(t *testing.T) {
	ctx := context.Background()
	if got := TenantFromContext(ctx); got != "" {
		t.Errorf("empty ctx tenant = %q, want \"\"", got)
	}

	ctx = ContextWithTenant(ctx, "sgac1")
	ctx = ContextWithPrincipal(ctx, "user-1")

	if got := TenantFromContext(ctx); got != "sgac1" {
		t.Errorf("TenantFromContext = %q, want sgac1", got)
	}
	if got := PrincipalFromContext(ctx); got != "user-1" {
		t.Errorf("PrincipalFromContext = %q, want user-1", got)
	}
}
