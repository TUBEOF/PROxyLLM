package mockserver

// Testcontainers path (ADR-027d): a contributor with Docker but without a
// local JRE can run the conformance matrix by setting
// MOCKSERVER_CONTAINER=1. CI keeps using its service container through
// MOCKSERVER_URL, and a machine with neither URL, JAR nor Docker keeps the
// existing skip.

import (
	"context"
	"fmt"

	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

// ContainerImage is the MockServer image the testcontainers path starts.
const ContainerImage = "mockserver/mockserver:5.15.0"

// StartContainer starts one MockServer container and returns its base URL plus
// a terminate function. The caller owns the lifetime (the e2e TestMain keeps
// it for the whole run).
func StartContainer(ctx context.Context) (string, func(), error) {
	req := tc.ContainerRequest{
		Image:        ContainerImage,
		ExposedPorts: []string{"1080/tcp"},
		WaitingFor:   wait.ForHTTP("/mockserver/status").WithPort("1080/tcp"),
	}
	c, err := tc.GenericContainer(ctx, tc.GenericContainerRequest{
		ContainerRequest: req,
		Started:          true,
	})
	if err != nil {
		return "", nil, fmt.Errorf("mockserver container: %w", err)
	}
	host, err := c.Host(ctx)
	if err != nil {
		_ = c.Terminate(context.Background())
		return "", nil, fmt.Errorf("mockserver container host: %w", err)
	}
	port, err := c.MappedPort(ctx, "1080/tcp")
	if err != nil {
		_ = c.Terminate(context.Background())
		return "", nil, fmt.Errorf("mockserver container port: %w", err)
	}
	cleanup := func() { _ = c.Terminate(context.Background()) }
	return fmt.Sprintf("http://%s:%s", host, port.Port()), cleanup, nil
}
